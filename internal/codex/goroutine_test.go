package codex

import (
	"context"
	"testing"

	"go.uber.org/goleak"
)

func TestRecoverCodexGoroutineCatchesPanic(t *testing.T) {
	func() {
		defer recoverCodexGoroutine(context.Background(), "test goroutine")
		panic("boom")
	}()
}

func TestHandleCodexGoroutinePanicBranches(t *testing.T) {
	handleCodexGoroutinePanic(context.Background(), "none", nil, nil)

	var recovered any
	handleCodexGoroutinePanic(context.Background(), "with shutdown", func(value any) {
		recovered = value
	}, "panic value")
	if recovered != "panic value" {
		t.Fatalf("shutdown recovered = %#v", recovered)
	}
}

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
