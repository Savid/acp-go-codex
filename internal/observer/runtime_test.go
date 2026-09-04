package observer

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRuntimeStartupObserver(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var nilObserver *Observer
	nilObserver.ObserveStartupStage(ctx, "session", "spawn", time.Second, nil)
	(&Observer{}).ObserveStartupStage(ctx, "session", "spawn", time.Second, nil)

	observe := New(Config{})
	observe.ObserveStartupStage(ctx, "session", "spawn", time.Second, nil)
	observe.ObserveStartupStage(ctx, "session", "readiness", time.Second, errors.New("not ready"))
}
