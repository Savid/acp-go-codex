package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestThreadSessionConfigClonesAndComposesNativePath(t *testing.T) {
	operationFirst := filepath.Join(t.TempDir(), "first")
	operationSecond := filepath.Join(t.TempDir(), "second")
	nativeFirst := filepath.Join(t.TempDir(), "native-first")
	nativeSecond := filepath.Join(t.TempDir(), "native-second")
	separator := string(os.PathListSeparator)

	providerServer := map[string]any{"url": "https://example.test"}
	typedValues := map[string]string{"key": "value"}
	nestedStrings := []string{"one", "two"}
	config := map[string]any{
		"provider_section": map[string]any{"server": providerServer},
		"typed":            typedValues,
		"list":             []any{map[string]any{"nested": nestedStrings}},
		shellEnvironmentPolicyKey: map[string]any{
			shellEnvironmentSetKey: map[string]any{"EXISTING": "preserved"},
		},
	}
	environment := map[string]string{
		"WAGIE_API_TOKEN":    "secret-shaped-value",
		"WAGIE_OPERATION_ID": "operation-a",
	}
	extraPathDirs := []string{operationFirst, operationSecond}

	got, err := threadSessionConfig(
		config,
		environment,
		extraPathDirs,
		nativeFirst+separator+separator+nativeSecond,
	)
	require.NoError(t, err)

	set := threadEnvironmentSet(t, got)
	require.Equal(t, "preserved", set["EXISTING"])
	require.Equal(t, "secret-shaped-value", set["WAGIE_API_TOKEN"])
	require.Equal(t, "operation-a", set["WAGIE_OPERATION_ID"])
	require.Equal(t, strings.Join([]string{operationFirst, operationSecond, nativeFirst, nativeSecond}, separator), set[pathEnvKey])

	providerServer["url"] = "mutated"
	typedValues["key"] = "mutated"
	nestedStrings[0] = "mutated"
	environment["WAGIE_API_TOKEN"] = "mutated"
	extraPathDirs[0] = "mutated"

	require.Equal(t, map[string]any{"server": map[string]any{"url": "https://example.test"}}, got["provider_section"])
	require.Equal(t, map[string]string{"key": "value"}, got["typed"])
	require.Equal(t, []any{map[string]any{"nested": []string{"one", "two"}}}, got["list"])
	require.Equal(t, "secret-shaped-value", set["WAGIE_API_TOKEN"])
	require.Contains(t, set[pathEnvKey], operationFirst)

	empty, err := threadSessionConfig(nil, nil, nil, "")
	require.NoError(t, err)
	require.Nil(t, empty)

	nilPolicy, err := threadSessionConfig(map[string]any{shellEnvironmentPolicyKey: nil}, nil, nil, "")
	require.NoError(t, err)
	require.Equal(t, map[string]any{shellEnvironmentPolicyKey: map[string]any{shellEnvironmentSetKey: map[string]any{}}}, nilPolicy)
}

func TestThreadSessionConfigRejectsCompetingOwners(t *testing.T) {
	nativePath := filepath.Join(t.TempDir(), "native")

	tests := []struct {
		name        string
		config      map[string]any
		environment map[string]string
		want        string
	}{
		{
			name:   "policy shape",
			config: map[string]any{shellEnvironmentPolicyKey: "inherit"},
			want:   "shell_environment_policy must be an object",
		},
		{
			name: "set shape",
			config: map[string]any{shellEnvironmentPolicyKey: map[string]any{
				shellEnvironmentSetKey: []any{"bad"},
			}},
			want: "shell_environment_policy.set must be an object",
		},
		{
			name: "config path owner",
			config: map[string]any{shellEnvironmentPolicyKey: map[string]any{
				shellEnvironmentSetKey: map[string]any{pathEnvKey: "/operator"},
			}},
			want: "already owns the session search path",
		},
		{
			name:        "environment path owner",
			environment: map[string]string{pathEnvKey: "/operator"},
			want:        "environment must not set PATH",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := threadSessionConfig(test.config, test.environment, nil, nativePath)
			require.ErrorContains(t, err, test.want)
		})
	}

	original := caseInsensitiveEnvKeys
	caseInsensitiveEnvKeys = true
	t.Cleanup(func() { caseInsensitiveEnvKeys = original })
	_, err := threadSessionConfig(nil, map[string]string{"Path": "/operator"}, nil, nativePath)
	require.ErrorContains(t, err, "environment must not set Path")
}

func TestThreadConfigPathHelpers(t *testing.T) {
	separator := string(os.PathListSeparator)
	require.Equal(t, "", composeSearchPath([]string{""}, ""))
	require.Equal(t, "a"+separator+"b", composeSearchPath([]string{"a", ""}, separator+"b"+separator))
	require.Equal(t, "/native/path", searchPathFromEnvironment([]string{"MALFORMED", "A=1", "PATH=/native/path"}))
	require.Empty(t, searchPathFromEnvironment([]string{"A=1"}))
}

func TestAppServerThreadCarrierRPCsNeverCrossConcurrentThreads(t *testing.T) {
	transport := newScriptTransport()
	nativePath := filepath.Join(t.TempDir(), "native")
	client := &AppServerClient{rpc: newRPCConn(transport, nil), nativePath: nativePath}
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })

	type operation struct {
		method string
		label  string
	}
	operations := []operation{
		{method: methodThreadStart, label: "a-start"},
		{method: methodThreadResume, label: "b-resume"},
		{method: methodThreadFork, label: "a-fork"},
		{method: methodThreadStart, label: "b-start"},
		{method: methodThreadResume, label: "a-resume"},
		{method: methodThreadFork, label: "b-fork"},
	}

	start := make(chan struct{})
	errs := make(chan error, len(operations))
	var wg sync.WaitGroup
	for _, operation := range operations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			environment := map[string]string{
				"WAGIE_API_TOKEN":    "token-" + operation.label,
				"WAGIE_OPERATION_ID": operation.label,
			}
			dir := filepath.Join(string(filepath.Separator)+"operation", operation.label)
			switch operation.method {
			case methodThreadStart:
				_, err := client.StartThread(context.Background(), ThreadStartRequest{
					Cwd: "/work", Environment: environment, ExtraPathDirs: []string{dir},
				})
				errs <- err
			case methodThreadResume:
				_, err := client.ResumeThread(context.Background(), ThreadResumeRequest{
					ThreadID: operation.label, Cwd: "/work", Environment: environment, ExtraPathDirs: []string{dir},
				})
				errs <- err
			case methodThreadFork:
				_, err := client.ForkThread(context.Background(), ThreadForkRequest{
					ThreadID: operation.label, Cwd: "/work", Environment: environment, ExtraPathDirs: []string{dir},
				})
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	params := carrierRPCParams(t, transport)
	require.Len(t, params, len(operations))
	seen := make(map[string]struct{}, len(operations))
	for _, values := range params {
		config := requireConfigType[map[string]any](t, values["config"])
		set := threadEnvironmentSet(t, config)
		label := requireConfigType[string](t, set["WAGIE_OPERATION_ID"])
		require.Equal(t, "token-"+label, set["WAGIE_API_TOKEN"])
		require.Equal(t, filepath.Join(string(filepath.Separator)+"operation", label)+string(os.PathListSeparator)+nativePath, set[pathEnvKey])
		seen[label] = struct{}{}
	}
	require.Len(t, seen, len(operations))
}

func TestAppServerThreadCarrierRejectsConflictsBeforeRPC(t *testing.T) {
	transport := newScriptTransport()
	client := &AppServerClient{rpc: newRPCConn(transport, nil), nativePath: filepath.Join(t.TempDir(), "native")}
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	conflict := map[string]any{shellEnvironmentPolicyKey: "operator-owned"}

	_, err := client.StartThread(context.Background(), ThreadStartRequest{Config: conflict})
	require.ErrorContains(t, err, "shell_environment_policy must be an object")
	_, err = client.ResumeThread(context.Background(), ThreadResumeRequest{ThreadID: "thread", Config: conflict})
	require.ErrorContains(t, err, "shell_environment_policy must be an object")
	_, err = client.ForkThread(context.Background(), ThreadForkRequest{ThreadID: "thread", Config: conflict})
	require.ErrorContains(t, err, "shell_environment_policy must be an object")
	require.Empty(t, carrierRPCParams(t, transport))
}

const fakeWagieCarrierEnv = "CODEX_TEST_FAKE_WAGIE_CARRIER"

func TestFakeAppServerResolvesThreadWagie(t *testing.T) {
	if os.Getenv(fakeWagieCarrierEnv) == "1" {
		os.Exit(runFakeWagieAppServer())
	}
	if runtime.GOOS == "windows" {
		t.Skip("marker executable fixture uses a POSIX shell")
	}

	originalCommand := ordinaryExecCommand
	t.Cleanup(func() { ordinaryExecCommand = originalCommand })
	ordinaryExecCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command(os.Args[0], "-test.run=^TestFakeAppServerResolvesThreadWagie$")
	}

	nativeDir := writeWagieMarker(t, "native")
	operationA := writeWagieMarker(t, "operation-a")
	operationB := writeWagieMarker(t, "operation-b")
	client, err := NewAppServerClient(context.Background(), Options{
		CLIPath:       os.Args[0],
		NativeVersion: minCodexVersion,
		ImplicitEnvironment: map[string]string{
			"PATH":              nativeDir,
			fakeWagieCarrierEnv: "1",
		},
		skipHomeLock:  true,
		LaunchTimeout: time.Second * 5,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	require.Equal(t, nativeDir, client.nativePath)

	start, err := client.StartThread(context.Background(), ThreadStartRequest{
		Cwd: "/work",
		Environment: map[string]string{
			"WAGIE_API_TOKEN": "token-a",
		},
		ExtraPathDirs: []string{operationA, operationB},
	})
	require.NoError(t, err)
	require.Equal(t, "operation-a|token-a", start.Title)

	resume, err := client.ResumeThread(context.Background(), ThreadResumeRequest{
		ThreadID: start.ID,
		Cwd:      "/work",
		Environment: map[string]string{
			"WAGIE_API_TOKEN": "token-b",
		},
		ExtraPathDirs: []string{operationB, operationA},
	})
	require.NoError(t, err)
	require.Equal(t, "operation-b|token-b", resume.Title)
	require.NotContains(t, resume.Title, "token-a")

	fork, err := client.ForkThread(context.Background(), ThreadForkRequest{
		ThreadID: start.ID,
		Cwd:      "/work",
		Environment: map[string]string{
			"WAGIE_API_TOKEN": "token-a-fork",
		},
		ExtraPathDirs: []string{operationA},
	})
	require.NoError(t, err)
	require.Equal(t, "operation-a|token-a-fork", fork.Title)
}

func threadEnvironmentSet(t *testing.T, config map[string]any) map[string]any {
	t.Helper()

	policy := requireConfigType[map[string]any](t, config[shellEnvironmentPolicyKey])

	return requireConfigType[map[string]any](t, policy[shellEnvironmentSetKey])
}

func requireConfigType[T any](t *testing.T, value any) T {
	t.Helper()

	typed, ok := value.(T)
	require.True(t, ok, "value has type %T", value)

	return typed
}

func carrierRPCParams(t *testing.T, transport *scriptTransport) []map[string]any {
	t.Helper()

	transport.mu.Lock()
	defer transport.mu.Unlock()

	params := make([]map[string]any, 0)
	for _, message := range transport.sent {
		switch message.Method {
		case methodThreadStart, methodThreadResume, methodThreadFork:
			var values map[string]any
			require.NoError(t, json.Unmarshal(message.Params, &values))
			params = append(params, values)
		}
	}

	return params
}

func writeWagieMarker(t *testing.T, marker string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "wagie")
	contents := fmt.Sprintf("#!/bin/sh\nprintf '%s|%%s' \"$WAGIE_API_TOKEN\"\n", marker)
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o700))

	return dir
}

func runFakeWagieAppServer() int {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	sequence := 0

	for scanner.Scan() {
		var request rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return 2
		}
		if len(request.ID) == 0 {
			continue
		}

		response := rpcMessage{JSONRPC: jsonRPCVersion, ID: request.ID}
		switch request.Method {
		case methodInitialize:
			response.Result = mustRaw(map[string]any{})
		case methodThreadStart, methodThreadResume, methodThreadFork:
			var params map[string]any
			if err := json.Unmarshal(request.Params, &params); err != nil {
				return 3
			}
			config, _ := params["config"].(map[string]any)
			policy, _ := config[shellEnvironmentPolicyKey].(map[string]any)
			set, _ := policy[shellEnvironmentSetKey].(map[string]any)
			path, _ := set[pathEnvKey].(string)
			resolved, err := resolveOrdinaryProcessExecutable("wagie", []string{"PATH=" + path})
			if err != nil {
				response.Error = &rpcError{Code: -32000, Message: err.Error()}

				break
			}

			command := exec.Command(resolved)
			command.Env = []string{
				"PATH=" + path,
				"WAGIE_API_TOKEN=" + fmt.Sprint(set["WAGIE_API_TOKEN"]),
			}
			output, err := command.Output()
			if err != nil {
				response.Error = &rpcError{Code: -32000, Message: err.Error()}

				break
			}

			sequence++
			threadID, _ := params[fieldThreadID].(string)
			if request.Method == methodThreadStart || request.Method == methodThreadFork {
				threadID = fmt.Sprintf("thread-%d", sequence)
			}
			response.Result = mustRaw(map[string]any{
				"thread": map[string]any{
					fieldID: threadID, "sessionId": threadID, fieldName: strings.TrimSpace(string(output)),
				},
				"cwd": params["cwd"],
			})
		default:
			response.Error = &rpcError{Code: -32601, Message: "method not found"}
		}

		if err := encoder.Encode(response); err != nil {
			return 4
		}
	}
	if err := scanner.Err(); err != nil {
		return 5
	}

	return 0
}
