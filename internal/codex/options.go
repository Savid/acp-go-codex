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
	Scratch             string
	ScratchParent       string
	NativeVersion       string
	DefaultModel        string
	Env                 map[string]string
	ImplicitEnvironment map[string]string
	HostAuthority       HostAuthority
	Config              map[string]any
	ExtraArgs           []string
	Logger              *slog.Logger
	RequestHandler      RequestHandler
	EventHandler        func(context.Context, Event)
	LaunchTimeout       time.Duration
	ObserveStartupStage func(context.Context, string, string, time.Duration, error)

	skipGuardian bool
}
