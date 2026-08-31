package codexacp

import (
	"context"
	"time"

	"github.com/savid/acp-go-codex/internal/observer"
)

func instrumentRuntimeResourceHooks(hooks RuntimeResourceHooks, observe *observer.Observer) RuntimeResourceHooks {
	externalReserve := hooks.ReserveScratchRoot
	hooks.ReserveScratchRoot = func(ctx context.Context, kind RuntimeResourceKind) (func(), error) {
		if externalReserve == nil {
			return func() {}, nil
		}

		return externalReserve(ctx, kind)
	}

	externalStartup := hooks.ObserveStartupStage
	hooks.ObserveStartupStage = func(
		ctx context.Context,
		kind RuntimeResourceKind,
		stage RuntimeStartupStage,
		elapsed time.Duration,
		err error,
	) {
		if externalStartup != nil {
			externalStartup(ctx, kind, stage, elapsed, err)
		}

		observe.ObserveRuntimeStartupStage(ctx, string(kind), string(stage), elapsed, err)
	}

	return hooks
}

func observeRuntimeStartupStage(
	ctx context.Context,
	hooks RuntimeResourceHooks,
	kind RuntimeResourceKind,
	stage RuntimeStartupStage,
	started time.Time,
	err error,
) {
	if hooks.ObserveStartupStage != nil {
		hooks.ObserveStartupStage(ctx, kind, stage, time.Since(started), err)
	}
}
