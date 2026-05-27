package main

import (
	"context"
	"testing"
)

func TestRecoverMainGoroutineCatchesPanic(t *testing.T) {
	func() {
		defer recoverMainGoroutine(context.Background(), "test goroutine")
		panic("boom")
	}()
	recoverMainGoroutine(context.Background(), "none")
}
