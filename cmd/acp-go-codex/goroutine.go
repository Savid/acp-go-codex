package main

import (
	"context"
	"log/slog"
)

func recoverMainGoroutine(ctx context.Context, name string) {
	recovered := recover()
	if recovered == nil {
		return
	}

	slog.Default().ErrorContext(ctx, "command goroutine panic", slog.String("goroutine", name), slog.Any("panic", recovered))
}
