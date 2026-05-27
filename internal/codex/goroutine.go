package codex

import (
	"context"
	"log/slog"
)

func recoverCodexGoroutine(ctx context.Context, name string) {
	handleCodexGoroutinePanic(ctx, name, nil, recover())
}

func handleCodexGoroutinePanic(ctx context.Context, name string, shutdown func(any), recovered any) {
	if recovered == nil {
		return
	}

	slog.Default().ErrorContext(ctx, "codex goroutine panic", slog.String("goroutine", name), slog.Any("panic", recovered))
	if shutdown != nil {
		shutdown(recovered)
	}
}
