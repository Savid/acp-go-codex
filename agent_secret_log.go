package codexacp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/coder/acp-go-sdk"
)

const logRequestCancelled = "request_cancelled"

func secretSafeLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}

	return slog.New(secretSafeLogHandler{next: logger.Handler()})
}

type secretSafeLogHandler struct {
	next slog.Handler
}

func (h secretSafeLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h secretSafeLogHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, secretSafeLogMessage(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(secretSafeLogAttr(attr))

		return true
	})

	return h.next.Handle(ctx, clean)
}

func (h secretSafeLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for index := range attrs {
		clean[index] = secretSafeLogAttr(attrs[index])
	}

	return secretSafeLogHandler{next: h.next.WithAttrs(clean)}
}

func (h secretSafeLogHandler) WithGroup(name string) slog.Handler {
	return secretSafeLogHandler{next: h.next.WithGroup("acp_transport")}
}

func secretSafeLogMessage(message string) string {
	switch message {
	case "failed to parse incoming message",
		"failed to canonicalize inbound request id",
		"failed to queue notification; closing connection",
		"received message with neither id nor method",
		"connection closed",
		"failed to canonicalize response id",
		"failed to parse $/cancel_request params",
		"received $/cancel_request without requestId",
		"failed to canonicalize $/cancel_request requestId",
		"failed to handle notification",
		"failed to send $/cancel_request",
		"dropping $/cancel_request due to full queue":
		return message
	default:
		return "ACP transport diagnostic"
	}
}

func secretSafeLogAttr(attr slog.Attr) slog.Attr {
	attr.Value = attr.Value.Resolve()
	switch attr.Key {
	case authFieldMethod:
		if attr.Value.Kind() == slog.KindString {
			switch attr.Value.String() {
			case acp.AgentMethodSessionCancel, acp.AgentMethodSessionPrompt,
				acp.AgentMethodSessionClose, acp.ClientMethodSessionUpdate, "$/cancel_request":
				return attr
			}
		}
	case "err", jsonFieldError:
		if err, ok := attr.Value.Any().(error); ok {
			return slog.String(attr.Key, secretSafeLogError(err))
		}
	case "capacity", "queued", "queue_len":
		switch attr.Value.Kind() {
		case slog.KindInt64, slog.KindUint64:
			return attr
		}
	}

	return slog.String(attr.Key, valueInternalFailure)
}

func secretSafeLogError(err error) string {
	var requestErr *acp.RequestError
	if errors.As(err, &requestErr) && requestErr != nil {
		switch requestErr.Code {
		case -32700:
			return "parse_error"
		case -32600:
			return "invalid_request"
		case -32601:
			return "method_not_found"
		case -32602:
			return "invalid_params"
		case -32800:
			return logRequestCancelled
		case -32000:
			return "authentication_required"
		}
	}

	if errors.Is(err, context.Canceled) {
		return logRequestCancelled
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}

	return valueInternalFailure
}
