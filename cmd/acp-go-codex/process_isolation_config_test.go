package main

import (
	"context"
	"io"
	"strings"
	"testing"

	codexacp "github.com/savid/acp-go-codex"
)

const testProcessIsolationConfigPath = "/test/process-isolation.json"

func stubProcessIsolationConfig(t *testing.T) {
	t.Helper()

	original := processIsolationConfigLoader
	processIsolationConfigLoader = func(path string) (processIsolationConfig, error) {
		if path != testProcessIsolationConfigPath {
			t.Fatalf("process isolation config path = %q", path)
		}

		return processIsolationConfig{
			UID:                 20001,
			GID:                 20001,
			BaseEnvironment:     map[string]string{"PATH": "/usr/bin", "HOME": "/tmp/codex", "USER": "acp", "LOGNAME": "acp"},
			StandaloneOwnerID:   "test-owner",
			StandaloneStateRoot: "/tmp/codex",
		}, nil
	}
	t.Cleanup(func() { processIsolationConfigLoader = original })
}

func isolatedArgs(args ...string) []string {
	return append([]string{"-" + processIsolationConfigFlag, testProcessIsolationConfigPath}, args...)
}

func TestDecodeProcessIsolationConfigStrict(t *testing.T) {
	config, err := decodeProcessIsolationConfig([]byte(`{"uid":20001,"gid":20002,"baseEnvironment":{"PATH":"/usr/bin"},"inheritEnvironment":["AMP_API_KEY"],"standaloneOwnerId":"deployment-1","standaloneStateRoot":"/var/lib/provider"}`))
	if err != nil {
		t.Fatal(err)
	}
	if config.UID != 20001 || config.GID != 20002 || config.BaseEnvironment["PATH"] != "/usr/bin" || len(config.InheritEnvironment) != 1 ||
		config.StandaloneOwnerID != "deployment-1" || config.StandaloneStateRoot != "/var/lib/provider" {
		t.Fatalf("decoded config = %#v", config)
	}

	for _, document := range []string{
		`{"uid":1,"gid":2,"baseEnvironment":{},"unknown":true}`,
		`{"uid":1,"gid":2,"baseEnvironment":{}} {}`,
		`{"uid":1,"uid":2,"gid":2,"baseEnvironment":{}}`,
		`{"uid":1,"gid":2,"baseEnvironment":{},"standaloneOwnerId":"a","standaloneOwnerId":"b"}`,
		`{"uid":1,"gid":2,"baseEnvironment":{},"standaloneStateRoot":"/a","standaloneStateRoot":"/b"}`,
		`{"uid":1,"gid":2,"baseEnvironment":{"PATH":"/bin","PATH":"/usr/bin"}}`,
		`{"uid":1,"gid":2,"baseEnvironment":{}} @`,
		`{"uid":1,"gid":2,"baseEnvironment":{},@:1}`,
		`{"uid":1,"gid":2,"baseEnvironment":{},"inheritEnvironment":[@]}`,
		`{"uid":1,"gid":2,"baseEnvironment":{"PATH":"/bin"`,
		`[{"uid":1}]`,
		``,
	} {
		if _, err := decodeProcessIsolationConfig([]byte(document)); err == nil {
			t.Fatalf("decode unexpectedly accepted %q", document)
		}
	}
	if _, err := decodeProcessIsolationConfig([]byte{0xff}); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
}

func TestRunWithoutProcessIsolationConfigUsesOrdinaryMode(t *testing.T) {
	originalLoader := processIsolationConfigLoader
	originalServe := serve
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		processIsolationConfigLoader = originalLoader
		serve = originalServe
		shutdownOpenTelemetry = originalShutdown
	})

	processIsolationConfigLoader = func(string) (processIsolationConfig, error) {
		t.Fatal("ordinary mode loaded a process-isolation policy")

		return processIsolationConfig{}, nil
	}
	var options codexacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, opts ...codexacp.Option) error {
		for _, option := range opts {
			option(&options)
		}

		return nil
	}
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }

	var stderr strings.Builder
	if code := run(t.Context(), nil, strings.NewReader(""), &strings.Builder{}, &stderr); code != 0 {
		t.Fatalf("run code = %d, stderr = %q", code, stderr.String())
	}
	if options.ProcessIsolation != nil {
		t.Fatalf("ordinary mode process isolation = %#v", options.ProcessIsolation)
	}
	if options.Home != "" {
		t.Fatalf("ordinary mode home = %q", options.Home)
	}
}

func TestAccountSubcommandWithoutProcessIsolationConfigUsesOrdinaryMode(t *testing.T) {
	originalLoader := processIsolationConfigLoader
	originalCommand := runCodexCLICommand
	t.Cleanup(func() {
		processIsolationConfigLoader = originalLoader
		runCodexCLICommand = originalCommand
	})

	processIsolationConfigLoader = func(string) (processIsolationConfig, error) {
		t.Fatal("ordinary account mode loaded a process-isolation policy")

		return processIsolationConfig{}, nil
	}
	called := false
	runCodexCLICommand = func(
		_ context.Context,
		_, home, _ string,
		mode string,
		_ bool,
		isolation *processIsolationConfig,
		_ io.Reader,
		_, _ io.Writer,
	) error {
		called = true
		if isolation != nil {
			t.Fatalf("ordinary account process isolation = %#v", isolation)
		}
		if home != "/tmp/ordinary-codex" || mode != logoutCommand {
			t.Fatalf("ordinary account home/mode = %q %q", home, mode)
		}

		return nil
	}

	var stderr strings.Builder
	code := run(t.Context(), []string{logoutCommand, "-home", "/tmp/ordinary-codex"}, strings.NewReader(""), &strings.Builder{}, &stderr)
	if code != 0 || !called {
		t.Fatalf("ordinary account code/called/stderr = %d %t %q", code, called, stderr.String())
	}
}
