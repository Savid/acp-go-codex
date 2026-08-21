package codexacp

import (
	"errors"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

type hostDeliveryError struct {
	err error
}

func (e *hostDeliveryError) Error() string {
	if e == nil || e.err == nil {
		return "ACP host delivery failed"
	}

	return e.err.Error()
}

func (e *hostDeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.err
}

func wrapHostDeliveryError(err error) error {
	if err == nil {
		return nil
	}

	var delivery *hostDeliveryError
	if errors.As(err, &delivery) {
		return err
	}

	return &hostDeliveryError{err: err}
}

// turnFailureFromEvent extracts the native failure cause from a terminal event:
// a codex `error` event, or a `turn/completed` whose status is failed/errored.
func turnFailureFromEvent(event codex.Event) error {
	switch event.Kind {
	case codex.EventError, codex.EventCompleted:
		return event.Err
	default:
		return nil
	}
}

// mapTurnFailure translates a native turn failure into the uniform ACP wire
// error. A dead app-server connection marks the session for lazy relaunch but
// leaves it addressable; a native thread-id drift is a wrapper-invariant break
// and surfaces as the unknown-session error.
func (s *session) mapTurnFailure(err error) error {
	if errors.Is(err, codex.ErrThreadNotFound) {
		return newUnknownSession()
	}

	if isCodexAuthError(err) {
		return codexAuthRequiredError(err, s.accountMetaSnapshot())
	}

	data := map[string]any{
		jsonFieldError:   valueTurnFailed,
		jsonFieldCause:   codex.CauseProvider,
		jsonFieldMessage: "Codex provider turn failed",
	}

	var imageErr *imageOutputError
	if errors.As(err, &imageErr) {
		data[jsonFieldCause] = codex.CauseTransport
		data[jsonFieldMessage] = "Codex image output delivery failed"
		data["stage"] = imageOutputStage

		data[jsonFieldReason] = imageErr.reason
		if imageErr.sizeBytes > 0 || imageErr.reason == imageOutputTooLarge {
			data["sizeBytes"] = imageErr.sizeBytes
		}

		if imageErr.maxBytes > 0 || imageErr.reason == imageOutputTooLarge {
			data["maxBytes"] = imageErr.maxBytes
		}

		return acp.NewInternalError(data)
	}

	var (
		tf       *codex.TurnFailedError
		procExit *codex.ProcessExitError
	)

	switch {
	case errors.As(err, &tf):
		data[jsonFieldCause] = tf.Cause
		data[jsonFieldMessage] = turnFailureMessage(tf.Cause)

		if tf.StatusCode > 0 {
			data[jsonFieldStatusCode] = tf.StatusCode
		}

	case errors.As(err, &procExit):
		data[jsonFieldCause] = codex.CauseProcessExit
		data[jsonFieldMessage] = turnFailureMessage(codex.CauseProcessExit)

		s.markClientDead()
	case errors.Is(err, codex.ErrConnectionClosed):
		data[jsonFieldCause] = codex.CauseTransport
		data[jsonFieldMessage] = turnFailureMessage(codex.CauseTransport)

		s.markClientDead()
	}

	return acp.NewInternalError(data)
}

func turnFailureMessage(cause string) string {
	switch cause {
	case codex.CauseTransport:
		return "Codex transport failed"
	case codex.CauseProcessExit:
		return "Codex process exited"
	case codex.CauseTimeout:
		return "Codex turn timed out"
	default:
		return "Codex provider turn failed"
	}
}

// codexAuthRequiredError surfaces an authentication failure as the -32000
// auth-required error. The data carries the uniform turn-failure fields
// (`error`, `cause`, `message`) alongside the additive `_meta.codex.auth`
// block so hosts filtering on the uniform shape see a consistent envelope.
func codexAuthRequiredError(err error, account map[string]any) error {
	if err == nil || !isCodexAuthError(err) {
		return err
	}

	data := map[string]any{
		jsonFieldError:   valueTurnFailed,
		jsonFieldCause:   codex.CauseProvider,
		jsonFieldMessage: "Codex authentication is required",
		codexMetaKey: map[string]any{
			authMetaAuthKey: map[string]any{
				jsonFieldReason: "codex-auth-required",
				"methodIds":     []string{authMethodCodexLogin, authMethodChatGPTAuthTokens},
			},
		},
	}
	if len(account) > 0 {
		codexMeta, _ := data[codexMetaKey].(map[string]any)
		codexMeta[codexAccountMetaKey] = cloneAnyMap(account)
	}

	return acp.NewAuthRequired(data)
}

func isCodexAuthError(err error) bool {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "authentication required"),
		strings.Contains(text, "not authenticated"),
		strings.Contains(text, "not logged in"),
		strings.Contains(text, "login required"),
		strings.Contains(text, "unauthorized"),
		strings.Contains(text, "401"):
		return true
	default:
		return false
	}
}
