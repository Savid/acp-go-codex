//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	codexacp "github.com/savid/acp-go-codex"
	"github.com/savid/acp-go-core/wire"
)

const (
	envRunIntegration = "ACP_GO_CODEX_RUN_INTEGRATION"
	envRunLiveTokens  = "ACP_GO_CODEX_RUN_LIVE_TOKENS"
	envAgentBinary    = "ACP_GO_CODEX_AGENT_BINARY"
	envHome           = "ACP_GO_CODEX_HOME"
	envModel          = "ACP_GO_CODEX_MODEL"
	envHarnessPath    = "ACP_GO_CODEX_HARNESS_PATH"

	testTimeout = 120 * time.Second
)

func requireIntegration(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run integration tests", envRunIntegration)
	}
}

func requireLive(t *testing.T) {
	t.Helper()
	requireIntegration(t)

	if os.Getenv(envRunLiveTokens) != "1" {
		t.Skipf("set %s=1 to run token-spending tests", envRunLiveTokens)
	}
}

// harnessPath resolves the codex binary; a missing codex skips the smoke tier and
// fails the live tier.
func harnessPath(t *testing.T, live bool) string {
	t.Helper()

	path := os.Getenv(envHarnessPath)
	if path == "" {
		path = "codex"
	}

	resolved, err := exec.LookPath(path)
	if err != nil {
		if live {
			t.Fatalf("codex not found: %v", err)
		}

		t.Skipf("codex not installed: %v", err)
	}

	return resolved
}

// requireSessionPath permits only the installed package's own native prefix
// ahead of the session directories in an actual shell tool's output.
func requireSessionPath(t *testing.T, output, directory string) {
	t.Helper()
	_, value, found := strings.Cut(output, "ACP_PATH=")
	require.True(t, found, "marker did not report its PATH: %s", output)
	value, _, _ = strings.Cut(value, "\n")
	entries := filepath.SplitList(strings.TrimSpace(value))
	require.NotEmpty(t, entries)
	if entries[0] != directory {
		executable, err := filepath.EvalSymlinks(harnessPath(t, true))
		require.NoError(t, err)
		packageDir := filepath.Dir(filepath.Dir(executable))
		data, err := os.ReadFile(filepath.Join(packageDir, "codex-package.json"))
		require.NoError(t, err, "unexpected native PATH prefix: %s", entries[0])
		var manifest struct {
			Entrypoint string `json:"entrypoint"`
			PathDir    string `json:"pathDir"`
		}
		require.NoError(t, json.Unmarshal(data, &manifest))
		require.Equal(t, "codex-path", manifest.PathDir)
		entrypoint, err := filepath.EvalSymlinks(filepath.Join(packageDir, manifest.Entrypoint))
		require.NoError(t, err)
		require.Equal(t, executable, entrypoint)
		prefix, err := filepath.EvalSymlinks(filepath.Join(packageDir, manifest.PathDir))
		require.NoError(t, err)
		require.Equal(t, prefix, entries[0])
		entries = entries[1:]
	}
	require.NotEmpty(t, entries)
	require.Equal(t, directory, entries[0])
}

// isolatedHome copies the operator's codex home named by ACP_GO_CODEX_HOME into a
// temporary directory, so a live test never writes into the real home.
func isolatedHome(t *testing.T) string {
	t.Helper()

	home := filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	source := os.Getenv(envHome)
	if source == "" {
		return home
	}

	for _, name := range []string{"auth.json", "config.toml"} {
		data, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			continue
		}

		require.NoError(t, os.WriteFile(filepath.Join(home, name), data, 0o600))
	}

	return home
}

// recorder records updates and allows every permission.
type recorder struct {
	mu      sync.Mutex
	updates []acp.SessionNotification
}

func (r *recorder) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.updates = append(r.updates, params)

	return nil
}

func (r *recorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	text := ""

	for _, update := range r.updates {
		if chunk := update.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
			text += chunk.Content.Text.Text
		}
	}

	return text
}

func (*recorder) RequestPermission(_ context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	for _, option := range params.Options {
		if option.Kind == acp.PermissionOptionKindAllowOnce {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId)}, nil
		}
	}

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

func (*recorder) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.New("unsupported")
}

func (*recorder) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("unsupported")
}

func (*recorder) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("unsupported")
}

func (*recorder) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (*recorder) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}

func (*recorder) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (*recorder) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

// harness serves the agent to a recording client, either in-process or
// through a prebuilt binary named by ACP_GO_CODEX_AGENT_BINARY.
type harness struct {
	conn *acp.ClientSideConnection
	rec  *recorder
	home string
	stop func()
}

func newHarness(t *testing.T, live bool, extra ...codexacp.Option) *harness {
	t.Helper()

	return newHarnessAt(t, live, isolatedHome(t), extra...)
}

func newHarnessAt(t *testing.T, live bool, home string, extra ...codexacp.Option) *harness {
	t.Helper()
	codex := harnessPath(t, live)
	rec := &recorder{}

	ctx, cancel := context.WithCancel(context.Background())

	var (
		clientWriter io.WriteCloser
		clientReader io.Reader
		wait         func()
	)

	if binary := os.Getenv(envAgentBinary); binary != "" {
		cmd := exec.CommandContext(ctx, binary, "-path", codex, "-home", home, "-scratch-dir", t.TempDir())
		cmd.Stderr = os.Stderr

		stdin, err := cmd.StdinPipe()
		require.NoError(t, err)

		stdout, err := cmd.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, cmd.Start())

		clientWriter, clientReader = stdin, stdout
		wait = func() { _ = cmd.Wait() }
	} else {
		agentReader, writer := io.Pipe()
		reader, agentWriter := io.Pipe()
		served := make(chan error, 1)

		options := append([]codexacp.Option{
			codexacp.WithExecutablePath(codex),
			codexacp.WithHome(home),
			codexacp.WithScratchDir(t.TempDir()),
			codexacp.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		}, extra...)

		go func() { served <- codexacp.Serve(ctx, agentReader, agentWriter, options...) }()

		clientWriter, clientReader = writer, reader
		wait = func() { <-served }
	}

	conn := acp.NewClientSideConnection(rec, clientWriter, clientReader)
	conn.SetLogger(slog.New(slog.DiscardHandler))

	stop := sync.OnceFunc(func() {
		cancel()
		_ = clientWriter.Close()
		wait()
	})
	t.Cleanup(stop)

	return &harness{conn: conn, rec: rec, home: home, stop: stop}
}

func (h *harness) ctx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)

	return ctx
}

func liveModel() wire.SessionRequestOption {
	options := codexacp.NewCodexOptions()
	if model := os.Getenv(envModel); model != "" {
		options.Model = model
	}

	return codexacp.WithSessionCodexOptions(options)
}

func requestErrorData(t *testing.T, err error) map[string]any {
	t.Helper()

	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)

	encoded, marshalErr := json.Marshal(reqErr.Data)
	require.NoError(t, marshalErr)

	var data map[string]any
	require.NoError(t, json.Unmarshal(encoded, &data))

	return data
}

func (r *recorder) toolText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var text strings.Builder
	for _, update := range r.updates {
		if tool := update.Update.ToolCallUpdate; tool != nil {
			for _, item := range tool.Content {
				if item.Content != nil && item.Content.Content.Text != nil {
					text.WriteString(item.Content.Content.Text.Text)
				}
			}
		}
	}

	return text.String()
}
