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
		return append(options,
			func(options *Options) { options.ProcessIsolation = nil },
			WithDarwinBestEffortContainment(),
		)
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
	want := RuntimeContainmentSharedIdentity
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

func TestContainmentModeReportsOrdinarySharedIdentity(t *testing.T) {
	originalGOOS := containmentGOOS
	t.Cleanup(func() { containmentGOOS = originalGOOS })

	explicit := Options{ProcessIsolation: &ProcessIsolation{UID: 65534, GID: 65534}}

	containmentGOOS = "linux"
	require.Equal(t, RuntimeContainmentAuthoritative, containmentMode(explicit))
	require.Equal(t, RuntimeContainmentSharedIdentity, containmentMode(Options{}))

	containmentGOOS = "darwin"
	require.Equal(t, RuntimeContainmentSharedIdentity, containmentMode(Options{}))
	require.Equal(t, RuntimeContainmentUnavailable, containmentMode(explicit))
	require.Equal(t, RuntimeContainmentBestEffort, containmentMode(Options{DarwinBestEffortContainment: true}))
	require.Equal(t, RuntimeContainmentUnavailable, containmentMode(Options{
		DarwinBestEffortContainment: true,
		ProcessIsolation:            explicit.ProcessIsolation,
	}))
}

func TestSharedIdentityAgentSuppressesAuthoritativeLifecycleSurfaces(t *testing.T) {
	originalGOOS := containmentGOOS
	t.Cleanup(func() { containmentGOOS = originalGOOS })

	containmentGOOS = "linux"

	var observed []RuntimeContainmentMode

	snapshots := 0
	agent := NewAgent(
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
	require.Zero(t, snapshots)

	require.False(t, RuntimeContainmentSharedIdentity.provesWholeTreeLifecycle())
	require.False(t, RuntimeContainmentBestEffort.provesWholeTreeLifecycle())
	require.False(t, RuntimeContainmentUnavailable.provesWholeTreeLifecycle())
}

func TestContainmentModeSelections(t *testing.T) {
	original := containmentGOOS
	t.Cleanup(func() { containmentGOOS = original })

	containmentGOOS = "linux"
	if got := containmentMode(Options{}); got != RuntimeContainmentSharedIdentity {
		t.Fatalf("Linux mode = %q", got)
	}
	containmentGOOS = "windows"
	if got := containmentMode(Options{}); got != RuntimeContainmentSharedIdentity {
		t.Fatalf("Windows mode = %q", got)
	}

	containmentGOOS = "darwin"
	if got := containmentMode(Options{}); got != RuntimeContainmentSharedIdentity {
		t.Fatalf("Darwin default mode = %q", got)
	}
	if got := containmentMode(Options{DarwinBestEffortContainment: true}); got != RuntimeContainmentBestEffort {
		t.Fatalf("Darwin opted mode = %q", got)
	}
	if err := validateContainmentOptions(Options{DarwinBestEffortContainment: true}); err != nil {
		t.Fatal(err)
	}
	if err := validateContainmentOptions(Options{ProcessIsolation: &ProcessIsolation{}}); err == nil ||
		!strings.Contains(err.Error(), "supported only on linux") {
		t.Fatalf("Darwin explicit isolation error = %v", err)
	}
	if err := validateContainmentOptions(Options{
		DarwinBestEffortContainment: true,
		ProcessIsolation:            &ProcessIsolation{},
	}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("combined Darwin and explicit isolation error = %v", err)
	}

	containmentGOOS = "freebsd"
	if got := containmentMode(Options{}); got != RuntimeContainmentSharedIdentity {
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
