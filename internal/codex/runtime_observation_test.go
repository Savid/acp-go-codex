package codex

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
