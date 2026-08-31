package codexacp

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// testIsolationIdentity is the identity every fixture isolates to. Root cannot
// isolate to itself — the policy forbids UID or GID zero — and uid 1 is the
// system daemon account, which the standalone claim finds live on any host the
// suite can see through the initial PID namespace and rightly refuses as
// occupied. 65534 is the fleet-wide unprivileged stand-in, and the privileged
// lock serializes it across repos.
// The adapter package claims a different UID from the native package: go test
// runs both concurrently, and the authority admits one live claimant per UID.
// On Linux an unprivileged runner shifts off its own effective identity as
// well because explicit isolation must name a distinct identity.
func testIsolationIdentity() (uint32, uint32) {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 || gid == 0 {
		uid, gid = 65533, 65533
	}
	if runtime.GOOS == "linux" {
		if uid == os.Geteuid() {
			uid++
		}
		if gid == os.Getegid() {
			gid++
		}
	}

	return uint32(uid), uint32(gid)
}

// testStandaloneOwnerID is the claim owner this package uses. The authority
// binds an owner to a UID as tightly as it binds a UID to an owner, so a package
// that reuses another package's owner is refused even with its own UID.
const testStandaloneOwnerID = "test-owner-adapter"

// testStandaloneStateRootPath is the one state root this package claims. It has
// to be exactly one path: the authority permanently binds a UID to a single
// owner and state root for the lifetime of the authority tree.
const testStandaloneStateRootPath = "/var/lib/acp-go-codex-adapter-test"

// testStandaloneStateRoot materializes the directory a standalone identity
// claim binds: mode 0700, owned by the claimed identity, beneath root-owned
// ancestry that is neither group- nor other-writable.
var testStandaloneStateRoot = sync.OnceValue(func() string {
	uid, gid := testIsolationIdentity()
	if err := os.MkdirAll(testStandaloneStateRootPath, 0o700); err != nil {
		return testStandaloneStateRootPath
	}
	if err := os.Chown(testStandaloneStateRootPath, int(uid), int(gid)); err != nil {
		return testStandaloneStateRootPath
	}
	_ = os.Chmod(testStandaloneStateRootPath, 0o700)

	return testStandaloneStateRootPath
})

// testProcessIsolation is the policy a case needs before the product will
// launch anything native. Darwin accepts best-effort containment instead;
// everywhere else the launch is refused outright without a policy, which is why
// every case that spawns a real native process carries this one.
func testProcessIsolation() ProcessIsolation {
	uid, gid := testIsolationIdentity()
	environment := make(map[string]string)

	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			environment[key] = value
		}
	}

	if environment["PATH"] == "" {
		environment["PATH"] = "/usr/bin:/bin"
	}

	return ProcessIsolation{
		UID: uid, GID: gid, BaseEnvironment: environment,
		StandaloneOwnerID: testStandaloneOwnerID, StandaloneStateRoot: testStandaloneStateRoot(),
	}
}

// testTraversableTempDir is a parent the isolated identity can enter. t.TempDir
// nests its leaf under a 0700 directory, so every tree beneath it is refused
// for an ancestry that identity cannot traverse.
func testTraversableTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "acp-go-codex-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	require.NoError(t, os.Chmod(directory, 0o711))

	return directory
}

// testNativeOwnedTempDir is a durable home the native-owned predicate admits:
// mode exactly 0700 and owned by the isolated identity.
func testNativeOwnedTempDir(t *testing.T) string {
	t.Helper()
	directory := testTraversableTempDir(t)
	uid, gid := testIsolationIdentity()
	require.NoError(t, os.Chown(directory, int(uid), int(gid)))
	require.NoError(t, os.Chmod(directory, 0o700))

	return directory
}

// testReachableExecutable publishes the test binary where the isolated identity
// can exec it. The binary the go tool builds is a 0700 root-owned file inside a
// 0700 build directory, so a case that hands its own path to the product as the
// native executable dies with EACCES before the case begins.
func testReachableExecutable(t *testing.T) string {
	t.Helper()
	source, err := os.Executable()
	require.NoError(t, err)
	payload, err := os.ReadFile(source)
	require.NoError(t, err)
	reachable := filepath.Join(testTraversableTempDir(t), "codex")
	require.NoError(t, os.WriteFile(reachable, payload, 0o755))

	return reachable
}

func skipUnprivilegedDarwinIsolation(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("requires a privileged two-principal fixture to clear supplementary groups")
	}
}

// testNativeSharedDir is a directory the isolated native process can write into
// and the test can read back: the sentinels a fake native process publishes
// have to land somewhere that identity owns.
func testNativeSharedDir(t *testing.T, parent string) string {
	t.Helper()
	shared := filepath.Join(parent, "shared")
	require.NoError(t, os.MkdirAll(shared, 0o700))
	testHandToIsolatedIdentity(t, shared)

	return shared
}

// testHandToIsolatedIdentity gives a directory to the identity the product
// isolates to, which is the only way that identity can write inside it.
func testHandToIsolatedIdentity(t *testing.T, directory string) {
	t.Helper()
	uid, gid := testIsolationIdentity()
	if uid == uint32(os.Getuid()) && gid == uint32(os.Getgid()) {
		return
	}
	require.NoError(t, os.Chown(directory, int(uid), int(gid)))
}

// runtimeGenerationSnapshot reads the shared app-server generation a test wants
// to prove survived, or did not. The epoch is what distinguishes a generation
// that kept serving from a replacement started after one was fenced.
type runtimeGeneration struct {
	epoch uint64
	dead  bool
}

func (a *Agent) runtimeGenerationSnapshot() runtimeGeneration {
	a.mu.Lock()
	defer a.mu.Unlock()

	return runtimeGeneration{epoch: a.runtimeEpoch, dead: a.runtimeDead}
}
