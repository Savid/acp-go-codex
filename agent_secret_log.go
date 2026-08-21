package codexacp

import (
	"context"
	"log/slog"
)

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
	case "capacity", "queued", "queue_len":
		switch attr.Value.Kind() {
		case slog.KindInt64, slog.KindUint64:
			return attr
		}
	}

	return slog.String(attr.Key, valueInternalFailure)
}
