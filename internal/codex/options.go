package codex

import (
	"context"
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
	RequestHandler RequestHandler
	EventHandler   func(context.Context, Event)
	LaunchTimeout  time.Duration
}
