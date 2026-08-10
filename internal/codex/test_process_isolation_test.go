package codex

import (
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// testIsolationIdentity is the identity every fixture isolates to. Root cannot
// isolate to itself — the policy forbids UID or GID zero — and uid 1 is the
// system daemon account, which the standalone claim finds live on any host the
// suite can see through the initial PID namespace and rightly refuses as
// occupied. 65534 is the fleet-wide unprivileged stand-in, and the privileged
// lock serializes it across repos. On Linux an unprivileged runner shifts off
// its own effective identity as well because explicit isolation must always
// name a distinct identity. Other platforms cannot hand a directory to a
// second identity and refuse explicit isolation before launch.
func testIsolationIdentity() (uint32, uint32) {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 || gid == 0 {
		uid, gid = 65534, 65534
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

// testStandaloneStateRootPath is the one state root every fixture in this
// package claims. It has to be exactly one path: the authority permanently
// binds a UID to a single owner and state root, so a second root for the same
// identity is refused for the lifetime of the authority tree.
const testStandaloneStateRootPath = "/var/lib/acp-go-codex-test"

// testStandaloneStateRoot materializes the directory a standalone identity
// claim binds: mode 0700, owned by the claimed identity, beneath root-owned
// ancestry that is neither group- nor other-writable. The fixture used to name
// /var/lib/acp-go-test, a path nothing ever created, so every supervised launch
// died at the bind with ENOENT before the case under test began. Materializing
// it needs the privileges the claim needs anyway; an identity the caller cannot
// hand it to fails at the bind, which is the honest report.
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

func testProcessIsolation() *ProcessIsolation {
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

	return &ProcessIsolation{
		UID: uid, GID: gid, BaseEnvironment: environment,
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: testStandaloneStateRoot(),
	}
}

func withTestVersionIsolation(options VersionProbeOptions) VersionProbeOptions {
	if options.ProcessIsolation == nil {
		options.ProcessIsolation = testProcessIsolation()
	}
	if options.ScratchParent == "" {
		options.ScratchParent = os.TempDir()
	}
	if options.WritableHome == "" {
		options.WritableHome = os.TempDir()
	}

	return options
}

func testTraversableTempDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "acp-go-codex-test-")
	if err != nil {
		t.Fatalf("create traversable test directory: %v", err)
	}
	if err = os.Chmod(directory, 0o711); err != nil {
		_ = os.RemoveAll(directory)
		t.Fatalf("make test directory traversable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	return directory
}

// requireTwoPrincipalHarness skips a case whose fixture has to hand storage to
// the identity the product isolates to. Handing anything to a second identity
// is a privileged act, so an unprivileged runner cannot stage the case at all.
// That is a property of the harness, not a verdict on the code.
func requireTwoPrincipalHarness(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires a privileged two-principal fixture")
	}
}

func testNativeOwnedTempDir(t *testing.T) string {
	t.Helper()
	requireTwoPrincipalHarness(t)
	directory := testTraversableTempDir(t)
	isolation := testProcessIsolation()
	if err := os.Chown(directory, int(isolation.UID), int(isolation.GID)); err != nil {
		t.Fatalf("assign native-owned test directory: %v", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("protect native-owned test directory: %v", err)
	}

	return directory
}

func skipUnprivilegedDarwinIsolation(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("requires a privileged two-principal fixture to clear supplementary groups")
	}
}

// nativeScriptMode is the mode a fake native executable needs. The product
// launches it as the isolated identity, so a 0700 file owned by the test runner
// is unreachable and the launch dies with EACCES before the case begins.
const nativeScriptMode = 0o755
