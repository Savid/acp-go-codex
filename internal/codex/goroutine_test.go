package codex

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"

	"go.uber.org/goleak"
)

func TestRecoverCodexGoroutineCatchesPanic(t *testing.T) {
	func() {
		defer recoverCodexGoroutine(context.Background(), "test goroutine")
		panic("boom")
	}()
}

func TestHandleCodexGoroutinePanicBranches(t *testing.T) {
	handleCodexGoroutinePanic(context.Background(), "none", nil, nil)

	var recovered any
	handleCodexGoroutinePanic(context.Background(), "with shutdown", func(value any) {
		recovered = value
	}, "panic value")
	if recovered != "panic value" {
		t.Fatalf("shutdown recovered = %#v", recovered)
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("ACP_GO_CODEX_WINDOWS_EXECUTABLE_CHILD") == "1" {
		if len(os.Args) < 2 {
			os.Exit(2)
		}

		switch os.Args[1] {
		case codexVersionArgument:
			_, _ = fmt.Fprintln(os.Stdout, "codex-cli "+minCodexVersion)
		case accountCommandLogout:
		case appServerCommand:
			_, _ = io.Copy(io.Discard, os.Stdin)
		default:
			os.Exit(2)
		}

		os.Exit(0)
	}

	processIsolationGOOS = processIsolationLinux
	goleak.VerifyTestMain(m)
}
