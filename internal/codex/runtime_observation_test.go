package codex

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessRuntimeObservation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var nilProcess *process
	nilProcess.markSupervisorsReady(ctx)
	nilProcess.markExited()
	(&process{}).markSupervisorsReady(ctx)
	(&process{supervisor: &supervisorProof{}}).markSupervisorsReady(ctx)

	var deltas []int64
	observed := &process{
		supervisor: &supervisorProof{},
		observeProcess: func(_ context.Context, kind string, delta int64) {
			if kind != "home_lock_supervisor" {
				t.Fatalf("kind = %q", kind)
			}
			deltas = append(deltas, delta)
		},
	}
	observed.markSupervisorsReady(ctx)
	observed.markSupervisorsReady(ctx)
	observed.markExited()
	observed.markExited()
	if len(deltas) != 2 || deltas[0] != 2 || deltas[1] != -2 {
		t.Fatalf("deltas = %v", deltas)
	}

	exited := &process{supervisor: &supervisorProof{}, observeProcess: observed.observeProcess}
	exited.markExited()
	exited.markSupervisorsReady(ctx)
}

func TestObserveCodexStartupStage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	observeCodexStartupStage(ctx, Options{}, "runtime", "spawn", time.Now(), nil)

	wantErr := errors.New("spawn failed")
	called := false
	observeCodexStartupStage(ctx, Options{ObserveStartupStage: func(gotCtx context.Context, lifecycle, stage string, elapsed time.Duration, err error) {
		called = true
		if gotCtx != ctx || lifecycle != "runtime" || stage != "spawn" || elapsed < 0 || !errors.Is(err, wantErr) {
			t.Fatalf("observation = (%v, %q, %q, %v, %v)", gotCtx, lifecycle, stage, elapsed, err)
		}
	}}, "runtime", "spawn", time.Now(), wantErr)
	if !called {
		t.Fatal("startup-stage callback was not called")
	}
}

func TestProcessProviderSnapshotLifecycle(t *testing.T) {
	var nilProcess *process
	nilProcess.finishProviderSnapshot(t.Context(), nil)
	(&process{}).finishProviderSnapshot(t.Context(), nil)
	(&process{}).finishProviderSnapshot(t.Context(), ErrProcessContainmentIncomplete)

	path := filepath.Join(t.TempDir(), "provider-snapshot")
	if err := os.WriteFile(path, []byte("3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var snapshots []int
	var quiescent, unproven int
	proc := &process{
		supervisor: &supervisorProof{providerSnapshot: path},
		processSnapshot: ProcessSnapshotObserver{
			Observe:   func(_ context.Context, count int) { snapshots = append(snapshots, count) },
			Quiescent: func(context.Context) { quiescent++ },
			Unproven:  func() { unproven++ },
		},
	}
	proc.markSupervisorsReady(t.Context())
	proc.finishProviderSnapshot(t.Context(), nil)

	if len(snapshots) != 1 || snapshots[0] != 3 || quiescent != 1 || unproven != 0 {
		t.Fatalf("proven lifecycle = snapshots %v, quiescent %d, unproven %d", snapshots, quiescent, unproven)
	}

	unprovenProc := &process{processSnapshot: proc.processSnapshot}
	unprovenProc.finishProviderSnapshot(t.Context(), errors.Join(errors.New("close"), ErrProcessContainmentIncomplete))
	if quiescent != 1 || unproven != 1 {
		t.Fatalf("unproven lifecycle = quiescent %d, unproven %d", quiescent, unproven)
	}
}

func TestProcessClosePreservesSnapshotOnUnprovenTree(t *testing.T) {
	waitDone := make(chan struct{})
	close(waitDone)

	var quiescent, unproven int
	proc := &process{
		cmd:        &exec.Cmd{Process: &os.Process{}},
		supervisor: &supervisorProof{},
		waitDone:   waitDone,
		waitErr:    ErrProcessContainmentIncomplete,
		processSnapshot: ProcessSnapshotObserver{
			Quiescent: func(context.Context) { quiescent++ },
			Unproven:  func() { unproven++ },
		},
	}
	proc.waitOnce.Do(func() {})

	err := proc.Close()
	if !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("Close error = %v, want process-tree proof sentinel", err)
	}
	if quiescent != 0 || unproven != 1 {
		t.Fatalf("Close lifecycle = quiescent %d, unproven %d", quiescent, unproven)
	}
}

func TestSupervisorProviderSnapshotRejectsUnavailableInventory(t *testing.T) {
	if count, available := (*supervisorProof)(nil).readProviderSnapshot(); available || count != 0 {
		t.Fatalf("nil proof inventory = (%d, %t), want unavailable", count, available)
	}
	if count, available := (&supervisorProof{}).readProviderSnapshot(); available || count != 0 {
		t.Fatalf("empty proof inventory = (%d, %t), want unavailable", count, available)
	}

	proof := &supervisorProof{providerSnapshot: filepath.Join(t.TempDir(), "missing")}
	if count, available := proof.readProviderSnapshot(); available || count != 0 {
		t.Fatalf("missing inventory = (%d, %t), want unavailable", count, available)
	}

	if err := os.WriteFile(proof.providerSnapshot, []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if count, available := proof.readProviderSnapshot(); available || count != 0 {
		t.Fatalf("invalid inventory = (%d, %t), want unavailable", count, available)
	}

	if err := os.WriteFile(proof.providerSnapshot, []byte("-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if count, available := proof.readProviderSnapshot(); available || count != 0 {
		t.Fatalf("negative inventory = (%d, %t), want unavailable", count, available)
	}

	original := supervisorDescendantCount
	t.Cleanup(func() { supervisorDescendantCount = original })
	publishProviderProcessSnapshot(filepath.Join(t.TempDir(), "unavailable"), &livenessContainment{})
	supervisorDescendantCount = func(*livenessContainment) (int, bool) { return 6, true }
	published := filepath.Join(t.TempDir(), "published")
	publishProviderProcessSnapshot(published, &livenessContainment{})
	if raw, err := os.ReadFile(published); err != nil || string(raw) != "6\n" {
		t.Fatalf("published inventory = %q, %v", raw, err)
	}
}
