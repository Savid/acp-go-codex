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
