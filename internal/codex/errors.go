package codex

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrConnectionClosed = errors.New("codex app-server connection closed")
	// ErrBackgroundTerminalsUnsupported reports that the running app-server does
	// not carry the thread-scoped background-terminal methods at all. It is the
	// app-server's own method-not-found answer rather than a version guess, and
	// it is a different fact from a containment attempt that failed.
	ErrBackgroundTerminalsUnsupported = errors.New("codex app-server does not expose thread background terminals")
	ErrThreadNotFound                 = errors.New("codex thread not found")
	ErrProcessContainmentIncomplete   = errors.New("codex process containment incomplete")
	ErrPackageStageCleanupIncomplete  = errors.New("codex package stage cleanup incomplete")
)

// ProcessExitError reports that the codex app-server process terminated. It
// carries the process exit status and a bounded tail of the process stderr so a
// mid-turn transport death caused by the process exiting is classified as
// cause:"process_exit" with the real exit/stderr detail instead of a bare EOF.
// Transport death while the process is still alive is not a ProcessExitError and
// stays cause:"transport".
type ProcessExitError struct {
	Status     string
	StderrTail string
	Err        error
}

func (e *ProcessExitError) Error() string {
	if e == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("codex app-server process exited")

	if e.Status != "" {
		b.WriteString(" (")
		b.WriteString(e.Status)
		b.WriteString(")")
	}

	if e.StderrTail != "" {
		b.WriteString(": ")
		b.WriteString(e.StderrTail)
	}

	return b.String()
}

func (e *ProcessExitError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

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

// jsonRPCMethodNotFound is the JSON-RPC code an app-server answers a method it
// does not implement with. It is the structured capability signal: the protocol
// says which methods exist, so nothing here reads a version number.
const jsonRPCMethodNotFound = -32601

// methodNotFoundMessage is the text that accompanies that code on both sides of
// the connection.
const methodNotFoundMessage = "method not found"

// isMethodNotFound reports whether the app-server answered that it does not
// implement the method.
func isMethodNotFound(err error) bool {
	var rpcErr *rpcError

	return errors.As(err, &rpcErr) && rpcErr.Code == jsonRPCMethodNotFound
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
