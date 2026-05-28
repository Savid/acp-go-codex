package codex

import (
	"context"
	"log/slog"
	"time"
)

// Options configures the Codex provider boundary.
type Options struct {
	CLIPath        string
	CodexHome      string
	DefaultModel   string
	Env            map[string]string
	Config         map[string]any
	ExtraArgs      []string
	Logger         *slog.Logger
	RequestHandler RequestHandler
	EventHandler   func(context.Context, Event)
	LaunchTimeout  time.Duration
}
