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

func TestNormalizeThreadErrorForkReferenced(t *testing.T) {
	native := errors.New("cannot delete thread thread-1: forked history still references it")
	err := normalizeThreadError(native)
	if !errors.Is(err, ErrThreadForkReferenced) || !errors.Is(err, native) {
		t.Fatalf("normalized error = %v, want ErrThreadForkReferenced wrapping native", err)
	}
	if errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("fork refusal normalized to ErrThreadNotFound: %v", err)
	}
	if sentinel := normalizeThreadError(ErrThreadForkReferenced); sentinel != ErrThreadForkReferenced {
		t.Fatalf("sentinel normalization = %v", sentinel)
	}

	rpc := &rpcError{Code: -32600, Message: "cannot delete thread thread-2: forked history still references it"}
	if err := normalizeThreadError(rpc); !errors.Is(err, ErrThreadForkReferenced) {
		t.Fatalf("native rpc refusal = %v, want ErrThreadForkReferenced", err)
	}
	if err := normalizeThreadError(errors.New("forked history is fine")); errors.Is(err, ErrThreadForkReferenced) {
		t.Fatal("unrelated fork text normalized to ErrThreadForkReferenced")
	}
}
