//go:build linux

package codex

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const linuxSupervisorGuardianGoneMessage = "codex guardian exited before native launch"

const linuxSupervisorTrustedIdentityMessage = "codex liveness supervisor requires a distinct trusted root identity"

// supervisorIdentityCapability is a duplicable process identity capability that
// hands back exactly the descriptor the case gave it, so a case can pin which
// descriptor a supervised launch puts at which inherited position.
type supervisorIdentityCapability struct{ file *os.File }

func (capability supervisorIdentityCapability) Duplicate() (*os.File, error) {
	return capability.file, nil
}

// TestVerifyLinuxTrustedSupervisorIdentityRequiresADistinctRootSupervisor proves
// the identity the supervisor is asked to hold is never its own and never root.
// The supervisor keeps root so it can contain a tree the native process cannot
// signal back; if it were asked to run the native process as root, or as the
// very identity it already is, the isolation would be nominal and the native
// process could reach its own supervisor. Root supervising a distinct
// unprivileged identity is the only accepted shape.
func TestVerifyLinuxTrustedSupervisorIdentityRequiresADistinctRootSupervisor(t *testing.T) {
	linuxSupervisorIdentitySeams(t)
	const distinct = uint32(65534)
	if os.Geteuid() != 0 {
		require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(distinct), linuxSupervisorTrustedIdentityMessage)

		return
	}

	require.NoError(t, verifyLinuxTrustedSupervisorIdentity(distinct))

	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(0), linuxSupervisorTrustedIdentityMessage)

	effectiveUIDSource = func() int { return int(distinct) }
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(distinct), linuxSupervisorTrustedIdentityMessage)
}

// TestLinuxSupervisorControlCancellationIgnoresAControlStreamWithNoDescriptor
// proves the cancellation watch is only armed for a control stream the kernel
// can report a hang-up for. A stream that is not a descriptor has no hang-up to
// observe, so the watch must hand back no cancel channel at all rather than one
// that never closes — an identity claim would otherwise wait on a signal that
// can never arrive — and its stop must own nothing.
func TestLinuxSupervisorControlCancellationIgnoresAControlStreamWithNoDescriptor(t *testing.T) {
	canceled, stop := linuxSupervisorControlCancellation(strings.NewReader("control"))
	require.Nil(t, canceled)
	// Releasing a watch that was never armed releases nothing, so it stays safe
	// however many times the claim unwinds through it.
	stop()
	stop()
}

// TestLinuxSupervisorControlCancellationReportsTheGuardianHangUp proves the
// watch is driven by the control descriptor's own state, and only by that.
// While the guardian still holds the far end of the control pipe an in-flight
// identity claim must keep waiting, and the moment the guardian's end goes away
// the claim must be told to give up rather than block against a parent that no
// longer exists.
func TestLinuxSupervisorControlCancellationReportsTheGuardianHangUp(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = readEnd.Close() })

	canceled, stop := linuxSupervisorControlCancellation(readEnd)
	require.NotNil(t, canceled)

	select {
	case <-canceled:
		t.Fatal("the claim was cancelled while the guardian still held the control pipe")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, writeEnd.Close())

	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the claim was not cancelled after the guardian released the control pipe")
	}

	stop()
}

// TestLinuxSupervisorControlCancellationStopsWatchingWhenTheClaimReleasesIt
// proves the release the claim defers really does end the watch. The watch is a
// goroutine that reads the control descriptor forever, so a claim that finished
// while the guardian was still alive would otherwise leave a reader on a
// descriptor it no longer owns and report a cancellation for a claim that is
// already over. The case counts the watch's own touches of the descriptor
// through the pollFD seam, requires them to stop after the release, and only
// then hangs the guardian's end up: a released watch must let that pass without
// cancelling anything.
func TestLinuxSupervisorControlCancellationStopsWatchingWhenTheClaimReleasesIt(t *testing.T) {
	linuxSupervisorIdentitySeams(t)

	readEnd, writeEnd, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = writeEnd.Close()
		_ = readEnd.Close()
	})

	descriptor := readEnd.Fd()

	var polls atomic.Int64

	pollFDSource = func(*os.File) uintptr {
		polls.Add(1)

		return descriptor
	}

	canceled, stop := linuxSupervisorControlCancellation(readEnd)
	require.NotNil(t, canceled)
	require.Eventually(t, func() bool { return polls.Load() > 0 },
		5*time.Second, 10*time.Millisecond, "the watch never read the control descriptor")

	stop()

	require.Eventually(t, func() bool {
		before := polls.Load()
		time.Sleep(100 * time.Millisecond)

		return polls.Load() == before
	}, 10*time.Second, 10*time.Millisecond, "the released watch kept reading the control descriptor")

	quiet := polls.Load()
	require.NoError(t, writeEnd.Close())

	require.Never(t, func() bool {
		select {
		case <-canceled:
			return true
		default:
			return polls.Load() != quiet
		}
	}, 500*time.Millisecond, 25*time.Millisecond, "a released watch cancelled a claim that had already finished")
}

// TestValidateLinuxSupervisorGuardianPeerFencesTheNativeLaunch proves the fence
// the liveness supervisor puts in front of every native start. The peer pipe
// carries no traffic while the guardian lives, so anything at all on it —
// readable bytes or a hang-up — means the guardian is gone and the native
// process must not be launched into a tree nothing is left to contain. When the
// kernel will not answer for the peer the fence fails closed for the same
// reason, and in neither case may it consume the descriptor it was asked about.
func TestValidateLinuxSupervisorGuardianPeerFencesTheNativeLaunch(t *testing.T) {
	t.Run("the kernel will not answer for the peer", func(t *testing.T) {
		linuxSupervisorIdentitySeams(t)
		readEnd, writeEnd, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = writeEnd.Close()
			_ = readEnd.Close()
		})

		want := errors.New("peer will not poll")
		linuxSupervisorPeerPoll = func([]unix.PollFd, int) (int, error) { return 0, want }

		err = validateLinuxSupervisorGuardianPeer(readEnd, make(chan struct{}))
		require.ErrorIs(t, err, want)
		require.ErrorContains(t, err, "poll Codex guardian before native launch")

		linuxSupervisorPeerPoll = unix.Poll
		require.NoError(t, validateLinuxSupervisorGuardianPeer(readEnd, make(chan struct{})),
			"a failed fence must leave the peer descriptor usable for the next fence")
	})

	t.Run("the peer carries traffic", func(t *testing.T) {
		readEnd, writeEnd, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = writeEnd.Close()
			_ = readEnd.Close()
		})
		require.NoError(t, validateLinuxSupervisorGuardianPeer(readEnd, make(chan struct{})))

		_, err = writeEnd.Write([]byte("x"))
		require.NoError(t, err)
		require.EqualError(t, validateLinuxSupervisorGuardianPeer(readEnd, make(chan struct{})),
			linuxSupervisorGuardianGoneMessage)
	})

	t.Run("the peer hung up", func(t *testing.T) {
		readEnd, writeEnd, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() { _ = readEnd.Close() })
		require.NoError(t, validateLinuxSupervisorGuardianPeer(readEnd, make(chan struct{})))

		require.NoError(t, writeEnd.Close())
		require.EqualError(t, validateLinuxSupervisorGuardianPeer(readEnd, make(chan struct{})),
			linuxSupervisorGuardianGoneMessage)
	})
}

// TestAdoptLinuxAgentIdentityBindsOnlyTheInheritedDescriptors proves which
// descriptors a liveness supervisor may take its identity from, and that it
// takes it from nowhere else. supervisorCommand hands the guardian the config
// first and then the identity lock and the authority domain, which the exec puts
// at descriptors 3, 4 and 5; the adoption reads exactly 4 and 5 by that
// contract, and this case pins both ends of it — the positions the launch
// produces and the positions the adoption asks for. When the supervisor was
// started without them there is nothing to adopt, and it must refuse with no
// lock rather than proceed unbound.
func TestAdoptLinuxAgentIdentityBindsOnlyTheInheritedDescriptors(t *testing.T) {
	preserveSupervisorGlobals(t)

	inherited := supervisorInheritedFile
	t.Cleanup(func() { supervisorInheritedFile = inherited })

	identityFile, err := os.CreateTemp(t.TempDir(), "identity")
	require.NoError(t, err)
	domainFile, err := os.CreateTemp(t.TempDir(), "domain")
	require.NoError(t, err)

	isolation := testProcessIsolation()
	isolation.StandaloneOwnerID = ""
	isolation.StandaloneStateRoot = ""
	isolation.IdentityLock = supervisorIdentityCapability{file: identityFile}
	isolation.AuthorityDomain = supervisorIdentityCapability{file: domainFile}

	config := supervisorConfig{Scratch: t.TempDir(), Isolation: isolation}

	cmd, proof, err := supervisorCommand(context.Background(), config)
	require.NoError(t, err)
	require.Len(t, cmd.ExtraFiles, 3)
	require.Same(t, identityFile, cmd.ExtraFiles[1])
	require.Same(t, domainFile, cmd.ExtraFiles[2])
	require.NoError(t, proof.closeInherited())

	type inheritedRequest struct {
		fd   uintptr
		name string
	}

	var requested []inheritedRequest

	supervisorInheritedFile = func(fd uintptr, name string) *os.File {
		requested = append(requested, inheritedRequest{fd: fd, name: name})

		return nil
	}

	lock, err := adoptLinuxAgentIdentityLock(isolation.UID)
	require.EqualError(t, err, "inherited agent identity lock descriptor is unavailable")
	require.Nil(t, lock)

	domain, err := adoptLinuxAgentAuthorityDomain(isolation.UID)
	require.EqualError(t, err, "inherited agent authority domain descriptor is unavailable")
	require.Nil(t, domain)

	require.Equal(t, []inheritedRequest{
		{fd: uintptr(3 + 1), name: "codex-agent-identity-lock"},
		{fd: uintptr(3 + 2), name: "codex-agent-authority-domain"},
	}, requested, "adoption must read the descriptors the launch put the capabilities at")
}

// TestAcquireLinuxAgentIdentityAuthorityClaimsTheIdentityExclusively proves the
// standalone claim a guardian makes when no capability was handed down is real
// and exclusive. The two locks it returns are the permanent per-UID lock and the
// authority domain in /var/lib/acp-go/agent-identities, both held for as long as
// the supervisor holds them: while the claim is live no second claimant can take
// either, and once the supervisor releases them both become claimable again, so
// a supervised tree that exits cleanly does not strand the identity.
func TestAcquireLinuxAgentIdentityAuthorityClaimsTheIdentityExclusively(t *testing.T) {
	isolation := testProcessIsolation()

	identity, authority, err := acquireLinuxAgentIdentityAuthority(
		isolation.UID, isolation.GID, isolation.StandaloneOwnerID, isolation.StandaloneStateRoot,
		strings.NewReader(""),
	)
	require.NoError(t, err)
	require.NotNil(t, identity)
	require.NotNil(t, authority)
	require.NotNil(t, identity.InheritedFile())
	require.NotNil(t, authority.InheritedFile())
	require.NotEqual(t, identity.InheritedFile().Fd(), authority.InheritedFile().Fd())

	assertAgentIdentityAuthorityLocked(t, isolation.UID)

	require.NoError(t, identity.Close())
	require.NoError(t, authority.Close())
	assertAgentIdentityAuthorityReacquires(t, isolation.UID)
}

// TestAcquireLinuxAgentIdentityAuthorityRefusesAnUnbindableStateRoot proves a
// failed claim yields nothing at all. The two locks this function returns are
// what the supervisor hands its child as its whole identity, so a claim that
// could not be established must produce neither of them rather than one half of
// a pair the child would then be started with.
func TestAcquireLinuxAgentIdentityAuthorityRefusesAnUnbindableStateRoot(t *testing.T) {
	isolation := testProcessIsolation()

	identity, authority, err := acquireLinuxAgentIdentityAuthority(
		isolation.UID, isolation.GID, isolation.StandaloneOwnerID, "relative/state-root",
		strings.NewReader(""),
	)
	require.EqualError(t, err, "standalone state root must be a clean absolute path")
	require.Nil(t, identity)
	require.Nil(t, authority)
}

// TestValidateLinuxSupervisorAdoptedAuthorityAsksTheDispositionItWasToldTo
// proves the adopted-authority check the guardian runs before it dispatches
// asks about the authority the config says the supervisor holds, and never the
// other one. A standalone supervisor owns its state root and must have it
// re-proved; a borrowed supervisor was handed its capabilities by a caller that
// owns them and has no state root of its own, so the two dispositions are
// mutually exclusive and asking the wrong question would let a borrowed
// supervisor pass by presenting a standalone binding it does not hold.
func TestValidateLinuxSupervisorAdoptedAuthorityAsksTheDispositionItWasToldTo(t *testing.T) {
	isolation := testProcessIsolation()

	t.Run("standalone", func(t *testing.T) {
		err := validateLinuxSupervisorAdoptedAuthority(supervisorConfig{
			StandaloneAuthority: true,
			IsolationUID:        isolation.UID,
			IsolationGID:        isolation.GID,
			StandaloneOwnerID:   isolation.StandaloneOwnerID,
			StandaloneStateRoot: "relative/state-root",
		})
		require.EqualError(t, err, "standalone state root must be a clean absolute path",
			"a standalone supervisor's adopted authority must be re-proved against its own state root")
	})

	t.Run("borrowed", func(t *testing.T) {
		// The borrowed question can only be asked of an identity the authority
		// has already bound permanently to an owner, so the case establishes
		// that binding through the real claim and then releases it.
		identity, authority, err := acquireLinuxAgentIdentityAuthority(
			isolation.UID, isolation.GID, isolation.StandaloneOwnerID, isolation.StandaloneStateRoot,
			strings.NewReader(""),
		)
		require.NoError(t, err)
		require.NoError(t, identity.Close())
		require.NoError(t, authority.Close())

		err = validateLinuxSupervisorAdoptedAuthority(supervisorConfig{
			IsolationUID:        isolation.UID,
			IsolationGID:        isolation.GID,
			StandaloneOwnerID:   isolation.StandaloneOwnerID,
			StandaloneStateRoot: "relative/state-root",
		})
		require.ErrorContains(t, err, "has a permanent owner binding",
			"a borrowed supervisor must be refused an identity that is permanently owned")
		require.NotContains(t, err.Error(), "standalone state root",
			"a borrowed supervisor's config carries no standalone state root to consult")
	})
}
