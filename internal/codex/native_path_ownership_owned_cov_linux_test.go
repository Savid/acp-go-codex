//go:build linux

package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The refusals the durable native-owned surface owes its callers, named once so
// a repeated literal never lands on top of the production string it asserts.
const (
	nativeOwnedUntrustedAncestry     = "native-owned path ancestry has an untrusted owner"
	nativeOwnedUntraversableAncestry = "native-owned path ancestry is not traversable by the target identity"
	nativeOwnedForeignLeaf           = "native-owned directory is not owned by the target identity"
	nativeOwnedNotTargetDirectory    = "native-owned path is not a target-owned directory"
)

// nativeOwnedSharedAncestor is a directory root owns that any identity may
// create beneath, which is what lets an unprivileged case stage the ancestry a
// shared identity actually has to walk.
const nativeOwnedSharedAncestor = "/tmp"

// nativeOwnedHome builds the durable home the native-owned predicate admits:
// mode exactly 0700 and owned outright by the target identity, beneath a
// trusted ancestry that identity can still traverse. Both the home and the
// parent are returned so a case can move the ancestry and then assert the home
// itself never changed.
func nativeOwnedHome(t *testing.T) (string, string) {
	t.Helper()
	nativeOwnershipRequireRoot(t)

	parent := testTraversableTempDir(t)
	home := filepath.Join(parent, "home")
	require.NoError(t, os.Mkdir(home, 0o700))
	require.NoError(t, os.Chown(home, int(nativeOwnershipTargetUID), int(nativeOwnershipTargetGID)))
	require.NoError(t, os.Chmod(home, 0o700))

	return home, parent
}

// nativeOwnedRequireIntact asserts a refused home still belongs to the target
// identity at exactly 0700. Every refusal below has to be a pure read: the
// durable check never chowns and never relaxes a mode, so a case that changed
// the home while refusing it would have refused for the wrong reason.
func nativeOwnedRequireIntact(t *testing.T, home string) {
	t.Helper()

	uid, gid := nativeOwnershipOwner(t, home)
	require.Equal(t, nativeOwnershipTargetUID, uid, home)
	require.Equal(t, nativeOwnershipTargetGID, gid, home)

	info, err := os.Stat(home)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), home)
}

// TestNativeOwnedDirectoryAdmitsOnlyATargetOwnedPrivateHome proves the durable
// home the isolated runtime is handed is admitted only when the walk can prove
// every level of it, and that each unsafe shape is refused for its own stated
// reason. The home outlives the process it is given to, so a wrong answer here
// hands durable state to an identity that was never meant to hold it — or lets
// a third party reach state the isolated identity owns. Every refusal also
// asserts the home is untouched, because this surface only reads.
func TestNativeOwnedDirectoryAdmitsOnlyATargetOwnedPrivateHome(t *testing.T) {
	isolation := nativeOwnershipIsolation()

	t.Run("relative root", func(t *testing.T) {
		require.ErrorContains(
			t,
			validateNativeOwnedDirectory("relative/home", isolation),
			"native-owned path must be absolute",
		)
	})

	t.Run("proven home", func(t *testing.T) {
		home, _ := nativeOwnedHome(t)
		require.NoError(t, validateNativeOwnedDirectory(home, isolation))
		nativeOwnedRequireIntact(t, home)
	})

	t.Run("missing component", func(t *testing.T) {
		home, _ := nativeOwnedHome(t)
		require.ErrorIs(
			t, validateNativeOwnedDirectory(filepath.Join(home, "absent"), isolation), unix.ENOENT,
		)
		nativeOwnedRequireIntact(t, home)
	})

	t.Run("ancestor owned by a third identity", func(t *testing.T) {
		home, parent := nativeOwnedHome(t)
		require.NoError(t, os.Chown(parent, int(nativeOwnershipTargetUID)+1, int(nativeOwnershipTargetGID)+1))

		require.ErrorContains(
			t, validateNativeOwnedDirectory(home, isolation), nativeOwnedUntrustedAncestry,
		)
		nativeOwnedRequireIntact(t, home)
	})

	t.Run("ancestor anyone may write to", func(t *testing.T) {
		home, parent := nativeOwnedHome(t)
		require.NoError(t, os.Chmod(parent, 0o777))

		require.ErrorContains(
			t, validateNativeOwnedDirectory(home, isolation),
			"native-owned path ancestor mode 0777 is writable",
		)
		nativeOwnedRequireIntact(t, home)

		// Sticky protection readmits the ancestor: a third party can write
		// there but cannot replace an entry it does not own, which is the
		// property the writable refusal is actually about.
		require.NoError(t, os.Chmod(parent, 0o777|os.ModeSticky))
		require.NoError(t, validateNativeOwnedDirectory(home, isolation))
	})

	t.Run("ancestor the target identity cannot enter", func(t *testing.T) {
		home, parent := nativeOwnedHome(t)
		require.NoError(t, os.Chmod(parent, 0o700))

		require.ErrorContains(
			t, validateNativeOwnedDirectory(home, isolation), nativeOwnedUntraversableAncestry,
		)
		nativeOwnedRequireIntact(t, home)
	})

	t.Run("leaf owned by the wrapper rather than the target", func(t *testing.T) {
		parent := testTraversableTempDir(t)
		home := filepath.Join(parent, "home")
		require.NoError(t, os.Mkdir(home, 0o711))
		require.NoError(t, os.Chmod(home, 0o711))

		require.ErrorContains(t, validateNativeOwnedDirectory(home, isolation), nativeOwnedForeignLeaf)

		uid, gid := nativeOwnershipOwner(t, home)
		require.Equal(t, uint32(0), uid, "a refused home changed hands")
		require.Equal(t, uint32(0), gid)
	})

	t.Run("leaf reachable beyond its owner", func(t *testing.T) {
		home, _ := nativeOwnedHome(t)
		require.NoError(t, os.Chmod(home, 0o500))

		require.ErrorContains(
			t, validateNativeOwnedDirectory(home, isolation), "native-owned directory mode 0500 is unsafe",
		)

		uid, gid := nativeOwnershipOwner(t, home)
		require.Equal(t, nativeOwnershipTargetUID, uid)
		require.Equal(t, nativeOwnershipTargetGID, gid)
	})
}

// TestNativeOwnedWalkAdmitsARootOwnedAncestorUnderASharedIdentity proves the
// durable walk accepts the only ancestry a wrapper that never dropped privilege
// can offer. Its own identity is the identity the home belongs to, so no
// privilege boundary separates the two ends, yet every path to a directory that
// identity owns still crosses root-owned components. The case runs unprivileged
// on purpose — that is the deployment the relaxation is about — so the root-owned
// ancestor is a real one and is reached by substituting it for the filesystem
// root the walk starts from. A privileged caller owns that ancestor outright and
// would prove nothing here, which the root-gated case below covers instead.
func TestNativeOwnedWalkAdmitsARootOwnedAncestorUnderASharedIdentity(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged caller")
	}

	var ancestor unix.Stat_t
	require.NoError(t, unix.Stat(nativeOwnedSharedAncestor, &ancestor))

	if ancestor.Uid != 0 || ancestor.Gid != 0 {
		t.Skipf("%s is not root-owned", nativeOwnedSharedAncestor)
	}

	home, err := os.MkdirTemp(nativeOwnedSharedAncestor, "acp-go-codex-shared-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	require.NoError(t, os.Chown(home, os.Geteuid(), os.Getegid()))
	require.NoError(t, os.Chmod(home, 0o700))

	previous := nativeOwnershipOpenFilesystemRoot
	nativeOwnershipOpenFilesystemRoot = func() (int, error) {
		return unix.Open(
			nativeOwnedSharedAncestor, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
		)
	}

	t.Cleanup(func() { nativeOwnershipOpenFilesystemRoot = previous })

	staged := "/" + filepath.Base(home)
	shared := &ProcessIsolation{
		UID:             uint32(os.Geteuid()),
		GID:             uint32(os.Getegid()),
		BaseEnvironment: map[string]string{},
	}
	require.NoError(
		t, validateNativeOwnedDirectory(staged, shared),
		"the root-owned ancestry every home directory is reached through was refused",
	)

	// Two distinct identities keep the rule they had: the same root-owned
	// ancestor belongs to neither of them and is refused, with no advice about a
	// boundary that is working as intended.
	isolatedErr := validateNativeOwnedDirectory(staged, nativeOwnershipIsolation())
	require.ErrorContains(t, isolatedErr, nativeOwnedUntrustedAncestry)
	require.NotContains(t, isolatedErr.Error(), nativeSharedIdentityRemedy)
}

// TestNativeOwnedDirectoryUnderASharedIdentityAcceptsOnlyRootAncestors proves
// how far the durable ancestry rule relaxes once the trusted identity is the
// target identity, on a real filesystem rather than a staged one. Root-owned
// ancestry is admitted because there is no privilege boundary left for it to
// defend, and nothing else moves: an ancestor a third identity owns, an ancestor
// root left writable without sticky protection, and a home root still owns are
// all refused, the first two naming what an operator can change. The effective
// identity is staged through its seams so the shared identity is never root,
// which is the only way a privileged caller can observe the relaxation at all.
func TestNativeOwnedDirectoryUnderASharedIdentityAcceptsOnlyRootAncestors(t *testing.T) {
	shareIdentityWithTarget := func(t *testing.T) {
		t.Helper()

		previousUID, previousGID := effectiveUIDSource, effectiveGIDSource
		t.Cleanup(func() { effectiveUIDSource, effectiveGIDSource = previousUID, previousGID })

		effectiveUIDSource = func() int { return int(nativeOwnershipTargetUID) }
		effectiveGIDSource = func() int { return int(nativeOwnershipTargetGID) }
	}

	isolation := nativeOwnershipIsolation()

	t.Run("root-owned ancestry the shared identity has to walk", func(t *testing.T) {
		home, parent := nativeOwnedHome(t)
		require.NoError(t, os.Chmod(parent, 0o755))

		shareIdentityWithTarget(t)

		require.NoError(
			t, validateNativeOwnedDirectory(home, isolation),
			"the root-owned ancestry every home directory is reached through was refused",
		)
		nativeOwnedRequireIntact(t, home)
	})

	t.Run("root-owned ancestor anyone may write to under sticky protection", func(t *testing.T) {
		home, parent := nativeOwnedHome(t)
		require.NoError(t, os.Chmod(parent, 0o777|os.ModeSticky))

		shareIdentityWithTarget(t)

		require.NoError(t, validateNativeOwnedDirectory(home, isolation))
		nativeOwnedRequireIntact(t, home)
	})

	t.Run("root-owned ancestor anyone may write to without sticky protection", func(t *testing.T) {
		home, parent := nativeOwnedHome(t)
		require.NoError(t, os.Chmod(parent, 0o777))

		shareIdentityWithTarget(t)

		require.ErrorContains(
			t, validateNativeOwnedDirectory(home, isolation),
			"native-owned path ancestor mode 0777 is writable",
		)
		nativeOwnedRequireIntact(t, home)
	})

	t.Run("ancestor owned by a third identity", func(t *testing.T) {
		home, parent := nativeOwnedHome(t)
		require.NoError(t, os.Chown(parent, 4242, 4242))

		shareIdentityWithTarget(t)

		err := validateNativeOwnedDirectory(home, isolation)
		require.ErrorContains(t, err, "native-owned path ancestor is uid=4242 gid=4242")
		require.ErrorContains(t, err, nativeSharedIdentityRemedy)
		nativeOwnedRequireIntact(t, home)
	})

	t.Run("home still owned by root", func(t *testing.T) {
		home, _ := nativeOwnedHome(t)
		require.NoError(t, os.Chown(home, 0, 0))

		shareIdentityWithTarget(t)

		err := validateNativeOwnedDirectory(home, isolation)
		require.ErrorContains(t, err, nativeOwnedUntrustedAncestry)
	})

	t.Run("the rule two distinct identities had is unchanged", func(t *testing.T) {
		home, parent := nativeOwnedHome(t)
		require.NoError(t, os.Chown(parent, 4242, 4242))

		err := validateNativeOwnedDirectory(home, isolation)
		require.ErrorContains(t, err, nativeOwnedUntrustedAncestry)
		require.NotContains(t, err.Error(), nativeSharedIdentityRemedy)
		nativeOwnedRequireIntact(t, home)
	})
}

func TestNativeOwnedWalkRefusesANonDirectoryComponent(t *testing.T) {
	regular := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(regular, []byte("seeded"), 0o600))

	previous := nativeOwnershipOpenFilesystemRoot
	nativeOwnershipOpenFilesystemRoot = func() (int, error) {
		return unix.Open(regular, unix.O_PATH|unix.O_CLOEXEC, 0)
	}

	t.Cleanup(func() { nativeOwnershipOpenFilesystemRoot = previous })

	require.ErrorContains(
		t,
		validateNativeOwnedDirectory("/home", nativeOwnershipIsolation()),
		nativeOwnedUntrustedAncestry,
	)
}

// TestNativeOwnedDirectoryFailsClosedOnKernelFaults proves the durable check
// aborts whenever the kernel stops answering for a descriptor the walk already
// accepted. None of these can be driven through a real filesystem — the kernel
// answers for a descriptor this code just opened — so each is reached through
// the seam that exists for it. Swallowing any of them would admit a home whose
// ownership was never proven, and the isolated runtime would then be launched
// against it.
func TestNativeOwnedDirectoryFailsClosedOnKernelFaults(t *testing.T) {
	isolation := nativeOwnershipIsolation()

	t.Run("filesystem root unopenable", func(t *testing.T) {
		home, _ := nativeOwnedHome(t)

		previous := nativeOwnershipOpenFilesystemRoot
		nativeOwnershipOpenFilesystemRoot = func() (int, error) { return -1, unix.EMFILE }

		t.Cleanup(func() { nativeOwnershipOpenFilesystemRoot = previous })

		require.ErrorIs(t, validateNativeOwnedDirectory(home, isolation), unix.EMFILE)
		nativeOwnedRequireIntact(t, home)
	})

	t.Run("ancestor unstattable", func(t *testing.T) {
		home, _ := nativeOwnedHome(t)

		previous := nativeOwnershipFstat
		nativeOwnershipFstat = func(int, *unix.Stat_t) error { return unix.EIO }

		t.Cleanup(func() { nativeOwnershipFstat = previous })

		require.ErrorIs(t, validateNativeOwnedDirectory(home, isolation), unix.EIO)
	})
}

// TestNativeOwnedDirectoryRecheckDisagreeingWithTheWalkIsRefused proves the
// final inspection of the opened descriptor is load-bearing rather than a
// restatement of the walk. The walk validates the home one component at a time
// and the leaf can be replaced between the last openat and the moment the
// descriptor is used, so the check re-reads the descriptor it actually holds and
// refuses on any disagreement instead of admitting a home it never proved. A
// real filesystem cannot make one descriptor answer twice with two different
// inodes, so the disagreement is staged on the fstat seam; the home on disk is
// asserted intact after each refusal, so the refusal is provably about the
// re-read rather than about damage the walk did.
func TestNativeOwnedDirectoryRecheckDisagreeingWithTheWalkIsRefused(t *testing.T) {
	home, _ := nativeOwnedHome(t)
	isolation := nativeOwnershipIsolation()

	baseline := nativeOwnershipFstat
	t.Cleanup(func() { nativeOwnershipFstat = baseline })

	total := 0
	nativeOwnershipFstat = func(fd int, stat *unix.Stat_t) error {
		total++

		return baseline(fd, stat)
	}

	require.NoError(t, validateNativeOwnedDirectory(home, isolation))

	nativeOwnershipFstat = baseline
	require.Greater(t, total, 1, "the walk made no ancestor stat before the final inspection")

	for _, testCase := range []struct {
		name    string
		replace func(*unix.Stat_t) error
		want    string
	}{
		{
			name:    "descriptor stops answering",
			replace: func(*unix.Stat_t) error { return unix.EIO },
			want:    unix.EIO.Error(),
		},
		{
			name: "descriptor is no longer a directory",
			replace: func(stat *unix.Stat_t) error {
				stat.Mode = unix.S_IFREG | 0o700

				return nil
			},
			want: nativeOwnedNotTargetDirectory,
		},
		{
			name: "descriptor is owned by another identity",
			replace: func(stat *unix.Stat_t) error {
				stat.Uid = nativeOwnershipTargetUID + 1

				return nil
			},
			want: nativeOwnedNotTargetDirectory,
		},
		{
			name: "descriptor became writable by others",
			replace: func(stat *unix.Stat_t) error {
				stat.Mode = unix.S_IFDIR | 0o702

				return nil
			},
			want: "native-owned directory mode 0702 is unsafe",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			nativeOwnershipFstat = func(fd int, stat *unix.Stat_t) error {
				calls++
				if statErr := baseline(fd, stat); statErr != nil {
					return statErr
				}

				if calls == total {
					return testCase.replace(stat)
				}

				return nil
			}

			t.Cleanup(func() { nativeOwnershipFstat = baseline })

			require.ErrorContains(t, validateNativeOwnedDirectory(home, isolation), testCase.want)

			nativeOwnershipFstat = baseline
			nativeOwnedRequireIntact(t, home)
			require.NoError(t, validateNativeOwnedDirectory(home, isolation))
		})
	}
}
