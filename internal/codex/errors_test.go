package codex

import (
	"errors"
	"testing"
)

func TestNormalizeThreadError(t *testing.T) {
	native := errors.New("no rollout found for thread id thread-1")
	if err := normalizeThreadError(native); !errors.Is(err, ErrThreadNotFound) || !errors.Is(err, native) {
		t.Fatalf("normalized error = %v, want ErrThreadNotFound wrapping native", err)
	}

	sentinel := normalizeThreadError(ErrThreadNotFound)
	if sentinel != ErrThreadNotFound {
		t.Fatalf("sentinel normalization = %v", sentinel)
	}
	if normalizeThreadError(nil) != nil {
		t.Fatal("nil error normalization returned non-nil")
	}
	for _, msg := range []string{
		"rollout not found",
		"thread not found",
		"unknown thread",
		"thread does not exist",
		"no such thread",
	} {
		if err := normalizeThreadError(errors.New(msg)); !errors.Is(err, ErrThreadNotFound) {
			t.Fatalf("%q did not normalize to ErrThreadNotFound: %v", msg, err)
		}
	}
	for _, msg := range []string{
		"turn is not materialized yet",
		"send failed",
	} {
		if err := normalizeThreadError(errors.New(msg)); errors.Is(err, ErrThreadNotFound) {
			t.Fatalf("%q normalized to ErrThreadNotFound: %v", msg, err)
		}
	}
}
