package codex

import (
	"context"
	"log/slog"
	"time"
)

// Options configures the Codex provider boundary.
type Options struct {
	CLIPath             string
	CodexHome           string
	WritableHome        string
	SupervisorRoot      string
	DefaultModel        string
	Env                 map[string]string
	Config              map[string]any
	ExtraArgs           []string
	Logger              *slog.Logger
	RequestHandler      RequestHandler
	EventHandler        func(context.Context, Event)
	LaunchTimeout       time.Duration
	ObserveProcess      func(context.Context, string, int64)
	ObserveStartupStage func(context.Context, string, string, time.Duration, error)

	// skipSupervisor is restricted to package tests that exercise low-level
	// command error branches. Production launches always use the supervisor
	// pair and fail closed without its private scratch root.
	skipSupervisor bool
}
