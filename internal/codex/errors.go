package codex

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrConnectionClosed  = errors.New("codex app-server connection closed")
	ErrAppServerEvent    = errors.New("codex app-server reported an error event")
	ErrTurnEventOverflow = errors.New("codex turn event queue overflow")
	// ErrBackgroundTerminalsUnsupported reports that the running app-server does
	// not carry the thread-scoped background-terminal methods at all. It is the
	// app-server's own method-not-found answer rather than a version guess, and
	// it is a different fact from a containment attempt that failed.
	ErrBackgroundTerminalsUnsupported = errors.New("codex app-server does not expose thread background terminals")
	ErrThreadNotFound                 = errors.New("codex thread not found")
	// ErrThreadForkReferenced reports that the app-server refuses to delete a
	// thread because a forked thread still references its history. The refusal is
	// terminal rather than transient: it stands for as long as the child exists,
	// and the parent's rollout is the child's history, so deleting it is the wrong
	// outcome rather than a retryable failure.
	ErrThreadForkReferenced = errors.New("codex thread is still referenced by a forked thread")
)

const (
	appServerErrorText = "Codex app-server reported an error event"
	turnFailedText     = "Codex turn failed"
	processExitText    = "codex app-server process exited"
)

// ProcessExitError reports that the codex app-server process terminated.
type ProcessExitError struct {
	Err error
}

func (e *ProcessExitError) Error() string {
	if e == nil {
		return ""
	}

	return processExitText
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
// carries a fixed public classification plus, when the harness reports them,
// the upstream HTTP status and provider error code. Raw provider text is never
// retained here. The ACP layer translates it into the uniform
// `codex_turn_failed` wire error.
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
	if err == nil || errors.Is(err, ErrThreadNotFound) || errors.Is(err, ErrThreadForkReferenced) {
		return err
	}

	if isNativeForkReferenced(err) {
		return fmt.Errorf("%w: %w", ErrThreadForkReferenced, err)
	}

	if !isNativeThreadNotFound(err) {
		return err
	}

	return fmt.Errorf("%w: %w", ErrThreadNotFound, err)
}

// nativeErrorText is the text a native classification reads: the app-server's
// own message when the failure carries one, and the Go error text otherwise.
func nativeErrorText(err error) string {
	var nativeErr *rpcError
	if errors.As(err, &nativeErr) {
		return strings.ToLower(nativeErr.Message)
	}

	return strings.ToLower(err.Error())
}

// isNativeForkReferenced reports whether the app-server refused a thread delete
// because a fork still references that thread's history.
func isNativeForkReferenced(err error) bool {
	return strings.Contains(nativeErrorText(err), "forked history still references")
}

func isNativeThreadNotFound(err error) bool {
	text := nativeErrorText(err)

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
