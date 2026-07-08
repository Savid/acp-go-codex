package codex

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrConnectionClosed = errors.New("codex app-server connection closed")
	ErrThreadNotFound   = errors.New("codex thread not found")
)

// Failure cause vocabulary for a native turn failure. These map directly onto
// the ACP failure-error `cause` field.
const (
	CauseProcessExit = "process_exit"
	CauseTransport   = "transport"
	CauseProvider    = "provider"
	CauseTimeout     = "timeout"
)

// TurnFailedError is a native turn failure surfaced by the Codex app-server. It
// carries the classified cause plus the real native cause text and, when the
// harness reports them, the upstream HTTP status and provider error code. The
// ACP layer translates it into the uniform `codex_turn_failed` wire error.
type TurnFailedError struct {
	Cause        string
	Message      string
	StatusCode   int
	ProviderCode string
}

func (e *TurnFailedError) Error() string {
	if e == nil {
		return ""
	}

	return e.Message
}

func normalizeThreadError(err error) error {
	if err == nil || errors.Is(err, ErrThreadNotFound) {
		return err
	}

	if !isNativeThreadNotFound(err) {
		return err
	}

	return fmt.Errorf("%w: %w", ErrThreadNotFound, err)
}

func isNativeThreadNotFound(err error) bool {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "not materialized yet"):
		return false
	case strings.Contains(text, "no rollout found for thread id"),
		strings.Contains(text, "no rollout found for thread"),
		strings.Contains(text, "rollout not found"),
		strings.Contains(text, "thread not found"),
		strings.Contains(text, "unknown thread"),
		strings.Contains(text, "thread does not exist"),
		strings.Contains(text, "no such thread"):
		return true
	default:
		return false
	}
}
