package codexacp

import (
	"errors"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

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
		jsonFieldMessage: err.Error(),
	}

	var (
		tf       *codex.TurnFailedError
		procExit *codex.ProcessExitError
	)

	switch {
	case errors.As(err, &tf):
		data[jsonFieldCause] = tf.Cause
		data[jsonFieldMessage] = tf.Message

		if tf.StatusCode > 0 {
			data[jsonFieldStatusCode] = tf.StatusCode
		}

		if tf.ProviderCode != "" {
			data[jsonFieldProviderCode] = tf.ProviderCode
		}
	case errors.As(err, &procExit):
		// The app-server process died mid-turn: name the real exit status and
		// stderr tail instead of a bare transport EOF.
		data[jsonFieldCause] = codex.CauseProcessExit
		data[jsonFieldMessage] = procExit.Error()

		s.markClientDead()
	case errors.Is(err, codex.ErrConnectionClosed):
		data[jsonFieldCause] = codex.CauseTransport

		s.markClientDead()
	}

	return acp.NewInternalError(data)
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
		jsonFieldMessage: err.Error(),
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
