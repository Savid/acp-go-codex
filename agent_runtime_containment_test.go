package codexacp

import (
	"bytes"
	"context"
	"log/slog"
	"runtime"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func nativeContainmentTestOptions(options ...Option) []Option {
	if runtime.GOOS == "darwin" {
		return append(options, WithDarwinBestEffortContainment())
	}

	return options
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
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
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
}

func TestContainmentModeSelections(t *testing.T) {
	original := containmentGOOS
	t.Cleanup(func() { containmentGOOS = original })

	for _, platform := range []string{"linux", "windows"} {
		containmentGOOS = platform
		if got := containmentMode(Options{}); got != RuntimeContainmentAuthoritative {
			t.Fatalf("%s mode = %q", platform, got)
		}
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
	} {
		if err := validateContainmentOptions(Options{Env: map[string]string{key: "1"}}); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("reserved environment validation error for %q = %v", key, err)
		}
	}
}
