package codexacp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func nativeContainmentTestOptions(options ...Option) []Option {
	if runtime.GOOS == "darwin" {
		return append(options, WithDarwinBestEffortContainment())
	}

	return options
}

func TestStandaloneIsolationDefaultsAndFencesDurableHome(t *testing.T) {
	const stateRoot = "/var/lib/acp-go-codex"
	isolation := ProcessIsolation{StandaloneOwnerID: "deployment-1", StandaloneStateRoot: stateRoot}

	defaulted := NewAgent(WithProcessIsolation(isolation))
	if defaulted.options.Home != stateRoot {
		t.Fatalf("default home = %q, want %q", defaulted.options.Home, stateRoot)
	}

	mismatched := NewAgent(WithHome("/var/lib/other"), WithProcessIsolation(isolation))
	if _, err := mismatched.Initialize(t.Context(), acp.InitializeRequest{}); err == nil ||
		!strings.Contains(err.Error(), "WithHome must equal ProcessIsolation.StandaloneStateRoot") {
		t.Fatalf("mismatched standalone home error = %v", err)
	}
}

// A supervised embedding owns its state root through the trusted descriptors,
// so only the standalone shape defaults or fences the durable home.
func TestStandaloneHomeNormalizationLeavesSupervisedEmbeddingsAlone(t *testing.T) {
	const stateRoot = "/var/lib/acp-go-codex"

	require.NoError(t, normalizeStandaloneHome(nil))

	noIsolation := Options{Home: "/var/lib/other"}
	require.NoError(t, normalizeStandaloneHome(&noIsolation))
	require.Equal(t, "/var/lib/other", noIsolation.Home)

	for name, isolation := range map[string]ProcessIsolation{
		"identity lock":    {StandaloneStateRoot: stateRoot, IdentityLock: unavailableIdentityLock{}},
		"authority domain": {StandaloneStateRoot: stateRoot, AuthorityDomain: unavailableIdentityLock{}},
		"no state root":    {StandaloneOwnerID: "deployment-1"},
	} {
		t.Run(name, func(t *testing.T) {
			options := Options{ProcessIsolation: &isolation}
			require.NoError(t, normalizeStandaloneHome(&options))
			require.Empty(t, options.Home)
		})
	}

	matching := Options{Home: stateRoot, ProcessIsolation: &ProcessIsolation{StandaloneStateRoot: stateRoot}}
	require.NoError(t, normalizeStandaloneHome(&matching))
	require.Equal(t, stateRoot, matching.Home)
}

type unavailableIdentityLock struct{}

func (unavailableIdentityLock) Duplicate() (*os.File, error) {
	return nil, errors.New("identity lock is unavailable")
}

func TestAgentContainmentModeAndObservation(t *testing.T) {
	if got := (*Agent)(nil).ContainmentMode(); got != RuntimeContainmentUnavailable {
		t.Fatalf("nil agent mode = %q", got)
	}

	var observed []RuntimeContainmentMode
	defaultAgent := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
		ObserveContainment: func(_ context.Context, mode RuntimeContainmentMode) {
			observed = append(observed, mode)
		},
	}))
	want := RuntimeContainmentUnavailable
	if runtime.GOOS == "linux" {
		want = RuntimeContainmentAuthoritative
	}
	if got := defaultAgent.ContainmentMode(); got != want {
		t.Fatalf("default mode = %q, want %q", got, want)
	}
	if len(observed) != 1 || observed[0] != want {
		t.Fatalf("containment observations = %v", observed)
	}

	var logs bytes.Buffer
	var snapshots int
	opted := NewAgent(
		WithDarwinBestEffortContainment(),
		WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveProcessSnapshot: func(context.Context, RuntimeProcessKind, int) { snapshots++ },
		}),
	)
	if runtime.GOOS == "darwin" {
		if opted.ContainmentMode() != RuntimeContainmentBestEffort {
			t.Fatalf("opted mode = %q", opted.ContainmentMode())
		}
		if !strings.Contains(logs.String(), `"containment":"best_effort"`) || !strings.Contains(logs.String(), "escaped descendants may survive") {
			t.Fatalf("structured best-effort warning = %q", logs.String())
		}
		observer := opted.newProcessSnapshotObserver(t.Context())
		observer.Observe(t.Context(), 7)
		observer.Quiescent(t.Context())
		observer.Unproven()
		if snapshots != 0 {
			t.Fatalf("best-effort provider snapshots = %d", snapshots)
		}

		return
	}
	if opted.ContainmentMode() != RuntimeContainmentUnavailable {
		t.Fatalf("off-Darwin opted mode = %q", opted.ContainmentMode())
	}
	if _, err := opted.Initialize(t.Context(), acp.InitializeRequest{}); err == nil || !strings.Contains(err.Error(), "supported only on darwin") {
		t.Fatalf("off-Darwin opt-in initialization error = %v", err)
	}

	originalGOOS := containmentGOOS
	containmentGOOS = "darwin"
	t.Cleanup(func() { containmentGOOS = originalGOOS })
	logs.Reset()
	opted = NewAgent(WithDarwinBestEffortContainment(), WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	if opted.ContainmentMode() != RuntimeContainmentBestEffort || !strings.Contains(logs.String(), `"containment":"best_effort"`) {
		t.Fatalf("simulated Darwin mode=%q logs=%q", opted.ContainmentMode(), logs.String())
	}
}

// TestContainmentModeReportsASharedAgentIdentity proves the report names the
// boundary the launch actually has. A supervisor that runs the agent under its
// own identity still proves whole-tree lifecycle, but there is no credential
// separation between it and the agent, so reporting "authoritative" would
// overstate what an operator is being given. Root can never reach the shared
// report, and no platform but Linux has the boundary to weaken.
func TestContainmentModeReportsASharedAgentIdentity(t *testing.T) {
	originalGOOS, originalUID := containmentGOOS, containmentEffectiveUID
	t.Cleanup(func() {
		containmentGOOS = originalGOOS
		containmentEffectiveUID = originalUID
	})

	containmentEffectiveUID = func() int { return 1000 }
	shared := Options{ProcessIsolation: &ProcessIsolation{UID: 1000, GID: 1000}}
	distinct := Options{ProcessIsolation: &ProcessIsolation{UID: 65534, GID: 65534}}

	containmentGOOS = "linux"
	require.Equal(t, RuntimeContainmentSharedIdentity, containmentMode(shared))
	require.Equal(t, RuntimeContainmentAuthoritative, containmentMode(distinct))
	require.Equal(t, RuntimeContainmentAuthoritative, containmentMode(Options{}))

	// Decision 2 judges the boundary on the UID alone, so an equal uid with a
	// different gid is still the shared shape.
	require.Equal(t, RuntimeContainmentSharedIdentity,
		containmentMode(Options{ProcessIsolation: &ProcessIsolation{UID: 1000, GID: 1001}}))

	containmentEffectiveUID = func() int { return 0 }
	require.Equal(t, RuntimeContainmentAuthoritative, containmentMode(shared))
	require.Equal(t, RuntimeContainmentAuthoritative,
		containmentMode(Options{ProcessIsolation: &ProcessIsolation{UID: 0, GID: 0}}))

	containmentEffectiveUID = func() int { return 1000 }
	containmentGOOS = "darwin"
	require.Equal(t, RuntimeContainmentUnavailable, containmentMode(shared))
	require.Equal(t, RuntimeContainmentBestEffort,
		containmentMode(Options{ProcessIsolation: shared.ProcessIsolation, DarwinBestEffortContainment: true}))
}

// TestSharedIdentityAgentKeepsItsLifecycleSurfaces proves the new report is not
// read as a lost boundary. The provider inventory is published only where the
// containment proof accounts for every descendant, and the shared arm's
// subreaper tree accounts for exactly the same set as the trusted one.
func TestSharedIdentityAgentKeepsItsLifecycleSurfaces(t *testing.T) {
	originalGOOS, originalUID := containmentGOOS, containmentEffectiveUID
	t.Cleanup(func() {
		containmentGOOS = originalGOOS
		containmentEffectiveUID = originalUID
	})

	containmentGOOS = "linux"
	containmentEffectiveUID = func() int { return 1000 }

	var observed []RuntimeContainmentMode

	snapshots := 0
	agent := NewAgent(
		WithProcessIsolation(ProcessIsolation{UID: 1000, GID: 1000}),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveContainment: func(_ context.Context, mode RuntimeContainmentMode) {
				observed = append(observed, mode)
			},
			ObserveProcessSnapshot: func(context.Context, RuntimeProcessKind, int) { snapshots++ },
		}),
	)

	require.Equal(t, []RuntimeContainmentMode{RuntimeContainmentSharedIdentity}, observed)
	require.Equal(t, RuntimeContainmentSharedIdentity, agent.ContainmentMode())

	observer := agent.newProcessSnapshotObserver(t.Context())
	observer.Observe(t.Context(), 7)
	require.Equal(t, 1, snapshots)

	require.False(t, RuntimeContainmentBestEffort.provesWholeTreeLifecycle())
	require.False(t, RuntimeContainmentUnavailable.provesWholeTreeLifecycle())
}

func TestContainmentModeSelections(t *testing.T) {
	original := containmentGOOS
	t.Cleanup(func() { containmentGOOS = original })

	containmentGOOS = "linux"
	if got := containmentMode(Options{}); got != RuntimeContainmentAuthoritative {
		t.Fatalf("Linux mode = %q", got)
	}
	containmentGOOS = "windows"
	if got := containmentMode(Options{}); got != RuntimeContainmentUnavailable {
		t.Fatalf("Windows mode = %q", got)
	}

	containmentGOOS = "darwin"
	if got := containmentMode(Options{}); got != RuntimeContainmentUnavailable {
		t.Fatalf("Darwin default mode = %q", got)
	}
	if got := containmentMode(Options{DarwinBestEffortContainment: true}); got != RuntimeContainmentBestEffort {
		t.Fatalf("Darwin opted mode = %q", got)
	}
	if err := validateContainmentOptions(Options{DarwinBestEffortContainment: true}); err != nil {
		t.Fatal(err)
	}

	containmentGOOS = "freebsd"
	if got := containmentMode(Options{}); got != RuntimeContainmentUnavailable {
		t.Fatalf("unsupported mode = %q", got)
	}
	if got := containmentMode(Options{DarwinBestEffortContainment: true}); got != RuntimeContainmentUnavailable {
		t.Fatalf("off-Darwin opted mode = %q", got)
	}
	if err := validateContainmentOptions(Options{DarwinBestEffortContainment: true}); err == nil {
		t.Fatal("off-Darwin opt-in was accepted")
	}
	invalidAgent := NewAgent(WithDarwinBestEffortContainment())
	if _, err := invalidAgent.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "supported only on darwin") {
		t.Fatalf("embedded lifecycle accepted off-Darwin opt-in: %v", err)
	}
	for _, key := range []string{
		"acp_go_codex_internal_spoof",
		"acp_go_codex_runtime_id",
		"ACP_GO_CODEX_SCRATCH_ROOT",
		"CODEX_HOME",
		"HOME",
		"XDG_CACHE_HOME",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"XDG_RUNTIME_DIR",
		"XDG_STATE_HOME",
	} {
		if err := validateContainmentOptions(Options{Env: map[string]string{key: "1"}}); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("reserved environment validation error for %q = %v", key, err)
		}
	}
}
