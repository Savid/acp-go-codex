package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	codexacp "github.com/savid/acp-go-codex"
)

type fakeAgentConnection struct {
	initErr             error
	loadErr             error
	promptErr           error
	afterFirstPromptErr error

	loadCwd   string
	prompt    string
	prompts   []string
	closed    bool
	sessionID acp.SessionId
}

func (f *fakeAgentConnection) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{}, f.initErr
}

func (f *fakeAgentConnection) LoadSession(_ context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	f.sessionID = params.SessionId
	f.loadCwd = params.Cwd

	return acp.LoadSessionResponse{}, f.loadErr
}

func (f *fakeAgentConnection) Prompt(_ context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	f.sessionID = params.SessionId
	if len(params.Prompt) > 0 && params.Prompt[0].Text != nil {
		f.prompt = params.Prompt[0].Text.Text
		f.prompts = append(f.prompts, f.prompt)
	}
	if len(f.prompts) > 1 && f.afterFirstPromptErr != nil {
		return acp.PromptResponse{}, f.afterFirstPromptErr
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, f.promptErr
}

func (f *fakeAgentConnection) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	f.closed = true

	return acp.CloseSessionResponse{}, nil
}

func TestLoadSessionFileAndSeedStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"stored"}}`+"\n"+`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	entries, err := loadSessionFile(path)
	if err != nil {
		t.Fatalf("loadSessionFile returned error: %v", err)
	}
	if got := sessionIDFromEntries(entries); got != "stored" {
		t.Fatalf("sessionIDFromEntries = %q", got)
	}

	store := codexacp.NewInMemorySessionStore()
	if seedErr := seedSessionStore(context.Background(), store, "stored", entries); seedErr != nil {
		t.Fatalf("seedSessionStore returned error: %v", seedErr)
	}
	loaded, err := store.Load(context.Background(), codexacp.SessionKey{SessionID: "stored"})
	if err != nil || len(loaded) != len(entries) {
		t.Fatalf("store Load len=%d err=%v", len(loaded), err)
	}
}

func TestRunImportedResumeLoadsAndPrompts(t *testing.T) {
	conn := &fakeAgentConnection{}
	cfg := config{sessionID: "stored", cwd: "/tmp/project", prompt: "continue"}
	var stdout bytes.Buffer

	err := runImportedResume(context.Background(), conn, cfg, []json.RawMessage{json.RawMessage(`{"type":"event_msg"}`)}, strings.NewReader("\n"), &stdout)
	if err != nil {
		t.Fatalf("runImportedResume returned error: %v", err)
	}
	if conn.sessionID != "stored" || conn.loadCwd != "/tmp/project" || conn.prompt != "continue" || !conn.closed {
		t.Fatalf("connection state = %#v", conn)
	}
	if !strings.Contains(stdout.String(), "resumed session: stored") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunImportedResumeErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		conn *fakeAgentConnection
	}{
		{name: "initialize", conn: &fakeAgentConnection{initErr: errors.New("init")}},
		{name: "load", conn: &fakeAgentConnection{loadErr: errors.New("load")}},
		{name: "prompt", conn: &fakeAgentConnection{promptErr: errors.New("prompt")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runImportedResume(context.Background(), tc.conn, config{sessionID: "s", cwd: "/tmp/project", prompt: "p"}, nil, strings.NewReader("\n"), io.Discard)
			if err == nil {
				t.Fatal("runImportedResume succeeded")
			}
		})
	}
}

func TestParseConfigRejectsRelativeCwd(t *testing.T) {
	_, err := parseConfig([]string{"-cwd", "relative"}, io.Discard)
	if err == nil {
		t.Fatal("parseConfig accepted relative cwd")
	}
}

func TestDefaultSessionFilePathFallback(t *testing.T) {
	originalRuntimeCaller := runtimeCaller
	t.Cleanup(func() { runtimeCaller = originalRuntimeCaller })

	runtimeCaller = func(int) (uintptr, string, int, bool) {
		return 0, "", 0, false
	}
	if got := defaultSessionFilePath(); got != defaultSessionFile {
		t.Fatalf("defaultSessionFilePath = %q, want %q", got, defaultSessionFile)
	}
}

func TestClientHelpers(t *testing.T) {
	var out bytes.Buffer
	c := &client{output: &out}
	if c.writer() != &out {
		t.Fatal("writer did not return configured output")
	}
	if (&client{}).writer() == nil {
		t.Fatal("nil output writer returned nil")
	}

	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")
	if _, err := c.WriteTextFile(ctx, acp.WriteTextFileRequest{Path: path, Content: "body"}); err != nil {
		t.Fatalf("WriteTextFile returned error: %v", err)
	}
	read, err := c.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: path})
	if err != nil || read.Content != "body" {
		t.Fatalf("ReadTextFile = %#v err=%v", read, err)
	}
	if _, readRelErr := c.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: "relative"}); readRelErr == nil {
		t.Fatal("ReadTextFile accepted relative path")
	}
	if _, writeRelErr := c.WriteTextFile(ctx, acp.WriteTextFileRequest{Path: "relative"}); writeRelErr == nil {
		t.Fatal("WriteTextFile accepted relative path")
	}
	if _, readMissingErr := c.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: filepath.Join(dir, "missing.txt")}); readMissingErr == nil {
		t.Fatal("ReadTextFile missing file succeeded")
	}
	notDir := filepath.Join(dir, "not-dir")
	if writeNotDirErr := os.WriteFile(notDir, []byte("x"), 0o600); writeNotDirErr != nil {
		t.Fatalf("write not-dir: %v", writeNotDirErr)
	}
	if _, writeChildErr := c.WriteTextFile(ctx, acp.WriteTextFileRequest{Path: filepath.Join(notDir, "child.txt"), Content: "body"}); writeChildErr == nil {
		t.Fatal("WriteTextFile under file path succeeded")
	}

	resp, err := c.RequestPermission(ctx, acp.RequestPermissionRequest{Options: []acp.PermissionOption{
		{OptionId: "allow", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: "reject", Kind: acp.PermissionOptionKindRejectOnce},
	}})
	if err != nil || resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "reject" {
		t.Fatalf("reject permission resp=%#v err=%v", resp, err)
	}
	cancelResp, err := c.RequestPermission(ctx, acp.RequestPermissionRequest{})
	if err != nil || cancelResp.Outcome.Cancelled == nil {
		t.Fatalf("cancel permission resp=%#v err=%v", cancelResp, err)
	}

	if terminal, err := c.CreateTerminal(ctx, acp.CreateTerminalRequest{}); err != nil || terminal.TerminalId == "" {
		t.Fatalf("CreateTerminal = %#v err=%v", terminal, err)
	}
	if _, err := c.KillTerminal(ctx, acp.KillTerminalRequest{}); err != nil {
		t.Fatalf("KillTerminal returned error: %v", err)
	}
	if output, err := c.TerminalOutput(ctx, acp.TerminalOutputRequest{}); err != nil || output.Output != "" || output.Truncated {
		t.Fatalf("TerminalOutput = %#v err=%v", output, err)
	}
	if _, err := c.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{}); err != nil {
		t.Fatalf("ReleaseTerminal returned error: %v", err)
	}
	if _, err := c.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{}); err != nil {
		t.Fatalf("WaitForTerminalExit returned error: %v", err)
	}
}

func TestClientSessionUpdateRendering(t *testing.T) {
	var out bytes.Buffer
	c := &client{output: &out}
	status := acp.ToolCallStatusCompleted
	messageID := "message-1"

	updates := []acp.SessionNotification{
		{Update: acp.SessionUpdate{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: acp.TextBlock("")}}},
		{Update: acp.SessionUpdate{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: acp.TextBlock("user text")}}},
		{Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("")}}},
		{Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{MessageId: &messageID, Content: acp.TextBlock("agent")}}},
		{Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{MessageId: &messageID, Content: acp.TextBlock("agent text")}}},
		{Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("repeatrepeat")}}},
		{Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("repeat")}}},
		{Update: acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("")}}},
		{Update: acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("thinking")}}},
		{Update: acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tool-1", Title: "Read"}}},
		{Update: acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "tool-1"}}},
		{Update: acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "tool-1", Status: &status}}},
		{},
	}
	for i, update := range updates {
		if err := c.SessionUpdate(context.Background(), update); err != nil {
			t.Fatalf("SessionUpdate[%d] returned error: %v", i, err)
		}
	}

	text := out.String()
	for _, want := range []string{"[user] user text", "agent text", "[thought] thinking", "[tool] tool-1 Read", "[tool] tool-1 completed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %q does not contain %q", text, want)
		}
	}

	var suppressOut bytes.Buffer
	suppressClient := &client{output: &suppressOut}
	otherMessageID := "other-message"
	if err := suppressClient.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("same")}},
	}); err != nil {
		t.Fatalf("initial suppress SessionUpdate returned error: %v", err)
	}
	if err := suppressClient.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{MessageId: &otherMessageID, Content: acp.TextBlock("same")}},
	}); err != nil {
		t.Fatalf("suppress SessionUpdate returned error: %v", err)
	}
	if got := suppressOut.String(); got != "same" {
		t.Fatalf("suppressed output = %q, want one copy", got)
	}
}

func TestMessageDisplayHelpers(t *testing.T) {
	var out bytes.Buffer
	display := &messageDisplay{}
	for _, tc := range []struct {
		name        string
		input       string
		wantPrinted string
		wantOK      bool
	}{
		{name: "empty", input: "", wantPrinted: "", wantOK: false},
		{name: "initial", input: "hello", wantPrinted: "hello", wantOK: true},
		{name: "same", input: "hello", wantPrinted: "", wantOK: false},
		{name: "prefix", input: "hello world", wantPrinted: " world", wantOK: true},
		{name: "reset", input: "bye", wantPrinted: "bye", wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := out.String()
			printed, ok := display.writeText(&out, tc.input)
			if printed != tc.wantPrinted || ok != tc.wantOK {
				t.Fatalf("writeText = (%q, %v), want (%q, %v)", printed, ok, tc.wantPrinted, tc.wantOK)
			}
			if got := out.String()[len(before):]; got != tc.wantPrinted {
				t.Fatalf("output delta = %q, want %q", got, tc.wantPrinted)
			}
		})
	}

	c := &client{}
	emptyID := ""
	if got := c.messageDisplay(nil); got != &c.fallback {
		t.Fatal("nil message id did not use fallback")
	}
	if got := c.messageDisplay(&emptyID); got != &c.fallback {
		t.Fatal("empty message id did not use fallback")
	}
	messageID := "message"
	if got := c.messageDisplay(&messageID); got == nil || c.messageDisplay(&messageID) != got {
		t.Fatal("message display was not cached")
	}
}

func TestSuppressRepeatedAgentText(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(*client)
		display messageDisplay
		text    string
		want    bool
	}{
		{name: "agent run exact", setup: func(c *client) { c.agentRunText = "abc" }, text: "abc", want: true},
		{name: "agent run doubled", setup: func(c *client) { c.agentRunText = "abc" }, text: "abcabc", want: true},
		{name: "not previous agent", text: "abc", want: false},
		{name: "last agent repeated into empty display", setup: func(c *client) {
			c.lastUpdateWasAgent = true
			c.lastAgentText = "abc"
		}, text: "abc", want: true},
		{name: "display doubled", setup: func(c *client) { c.lastUpdateWasAgent = true }, display: messageDisplay{text: "abc"}, text: "abcabc", want: true},
		{name: "last agent doubled", setup: func(c *client) {
			c.lastUpdateWasAgent = true
			c.lastAgentText = "abc"
		}, text: "abcabc", want: true},
		{name: "new agent text", setup: func(c *client) {
			c.lastUpdateWasAgent = true
			c.lastAgentText = "abc"
		}, display: messageDisplay{text: "def"}, text: "ghi", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &client{}
			if tc.setup != nil {
				tc.setup(c)
			}
			got := c.suppressRepeatedAgentText(&tc.display, tc.text)
			if got != tc.want {
				t.Fatalf("suppressRepeatedAgentText = %v, want %v", got, tc.want)
			}
		})
	}

	c := &client{lastUpdateWasAgent: true, agentRunText: "text", fallback: messageDisplay{text: "fallback"}}
	c.resetAgentRun()
	if c.lastUpdateWasAgent || c.agentRunText != "" || c.fallback.text != "" {
		t.Fatalf("resetAgentRun left state: %#v", c)
	}
}

func TestCollapseRepeatedExactText(t *testing.T) {
	for _, tc := range []struct {
		text string
		want string
	}{
		{text: "", want: ""},
		{text: "abc", want: "abc"},
		{text: "abab", want: "ab"},
		{text: "abca", want: "abca"},
	} {
		if got := collapseRepeatedExactText(tc.text); got != tc.want {
			t.Fatalf("collapseRepeatedExactText(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestRunUsesFakeAgent(t *testing.T) {
	originalStartAgent := startAgent
	originalGetwd := getwd
	t.Cleanup(func() {
		startAgent = originalStartAgent
		getwd = originalGetwd
	})

	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"session_id":"stored"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	conn := &fakeAgentConnection{}
	closed := false
	waited := false
	startAgent = func(context.Context, io.Writer, io.Writer, codexacp.SessionStore) (*startedAgent, error) {
		return &startedAgent{
			conn:  conn,
			close: func() { closed = true },
			wait: func() error {
				waited = true

				return nil
			},
		}, nil
	}
	getwd = func() (string, error) { return dir, nil }

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-session-file", sessionFile, "typed prompt"}, strings.NewReader("\n"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if !closed || !waited || conn.prompt != "typed prompt" {
		t.Fatalf("closed=%v waited=%v conn=%#v", closed, waited, conn)
	}
}

func TestRunErrorBranches(t *testing.T) {
	originalStartAgent := startAgent
	originalGetwd := getwd
	t.Cleanup(func() {
		startAgent = originalStartAgent
		getwd = originalGetwd
	})

	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	cases := []struct {
		name       string
		args       []string
		startErr   error
		canceled   bool
		wantStatus int
	}{
		{name: "parse error", args: []string{"-bad"}, wantStatus: 1},
		{name: "load error", args: []string{"-session-file", filepath.Join(dir, "missing.jsonl"), "-session-id", "s"}, wantStatus: 1},
		{name: "missing session id", args: []string{"-session-file", sessionFile}, wantStatus: 1},
		{name: "start error", args: []string{"-session-file", sessionFile, "-session-id", "s"}, startErr: errors.New("start"), wantStatus: 1},
		{name: "seed error", args: []string{"-session-file", sessionFile, "-session-id", "s"}, canceled: true, wantStatus: 1},
		{name: "resume error", args: []string{"-session-file", sessionFile, "-session-id", "s"}, startErr: errors.New("resume"), wantStatus: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			startAgent = func(context.Context, io.Writer, io.Writer, codexacp.SessionStore) (*startedAgent, error) {
				if tc.name == "resume error" {
					return &startedAgent{conn: &fakeAgentConnection{initErr: tc.startErr}}, nil
				}

				return nil, tc.startErr
			}
			getwd = func() (string, error) { return dir, nil }
			var stderr bytes.Buffer
			ctx := context.Background()
			if tc.canceled {
				ctx = canceledContext()
			}
			code := run(ctx, tc.args, strings.NewReader("\n"), io.Discard, &stderr)
			if code != tc.wantStatus {
				t.Fatalf("run exit = %d, want %d stderr=%q", code, tc.wantStatus, stderr.String())
			}
		})
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx
}

func TestRunContextCanceled(t *testing.T) {
	originalStartAgent := startAgent
	originalGetwd := getwd
	t.Cleanup(func() {
		startAgent = originalStartAgent
		getwd = originalGetwd
	})

	dir := t.TempDir()
	sessionFile := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(`{"session_id":"stored"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &fakeAgentConnection{promptErr: context.Canceled}
	startAgent = func(context.Context, io.Writer, io.Writer, codexacp.SessionStore) (*startedAgent, error) {
		cancel()

		return &startedAgent{conn: conn}, nil
	}
	getwd = func() (string, error) { return dir, nil }
	var stderr bytes.Buffer
	if code := run(ctx, []string{"-session-file", sessionFile}, strings.NewReader("\n"), io.Discard, &stderr); code != 130 {
		t.Fatalf("run exit = %d, want 130 stderr=%q", code, stderr.String())
	}
}

func TestParseConfigBranches(t *testing.T) {
	originalGetwd := getwd
	t.Cleanup(func() { getwd = originalGetwd })

	getwd = func() (string, error) { return "", errors.New("cwd") }
	if _, err := parseConfig(nil, io.Discard); err == nil {
		t.Fatal("parseConfig accepted getwd error")
	}

	dir := t.TempDir()
	getwd = func() (string, error) { return dir, nil }
	cfg, err := parseConfig([]string{"-session-id", "s", "positional", "prompt"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig returned error: %v", err)
	}
	if cfg.cwd != dir || cfg.prompt != "positional prompt" || cfg.sessionID != "s" {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestLoadSessionFileErrorsAndSessionIDFallbacks(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "invalid json", body: "{"},
		{name: "json null", body: "null\n"},
		{name: "trailing json", body: `{"ok":true} {"extra":true}` + "\n"},
		{name: "empty", body: "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".jsonl")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			if _, err := loadSessionFile(path); err == nil {
				t.Fatal("loadSessionFile succeeded")
			}
		})
	}

	entries := []json.RawMessage{
		json.RawMessage(`{"type":`),
		json.RawMessage(`{"type":"session_meta","payload":{"id":"  meta-id  "}}`),
	}
	if got := sessionIDFromEntries(entries); got != "meta-id" {
		t.Fatalf("sessionIDFromEntries meta = %q", got)
	}
	entries = []json.RawMessage{
		json.RawMessage(`{"session_id":"  snake  "}`),
		json.RawMessage(`{"sessionId":"camel"}`),
	}
	if got := sessionIDFromEntries(entries); got != "snake" {
		t.Fatalf("sessionIDFromEntries snake = %q", got)
	}
	entries = []json.RawMessage{json.RawMessage(`{"sessionId":" camel "}`)}
	if got := sessionIDFromEntries(entries); got != "camel" {
		t.Fatalf("sessionIDFromEntries camel = %q", got)
	}
	if got := sessionIDFromEntries([]json.RawMessage{json.RawMessage(`{"type":"other"}`)}); got != "" {
		t.Fatalf("sessionIDFromEntries missing = %q", got)
	}
}

func TestStartAgentProcessAndGate(t *testing.T) {
	agent, err := startAgentProcess(context.Background(), io.Discard, io.Discard, codexacp.NewInMemorySessionStore())
	if err != nil {
		t.Fatalf("startAgentProcess returned error: %v", err)
	}
	agent.close()
	select {
	case err := <-func() <-chan error {
		ch := make(chan error, 1)
		go func() { ch <- agent.wait() }()

		return ch
	}():
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "closed") {
			t.Fatalf("agent wait returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent wait timed out")
	}

	gate := newConnectionInputGate(strings.NewReader("ok"))
	readDone := make(chan string, 1)
	go func() {
		buf := make([]byte, 2)
		n, err := gate.Read(buf)
		if err != nil {
			readDone <- err.Error()

			return
		}
		readDone <- string(buf[:n])
	}()
	select {
	case got := <-readDone:
		t.Fatalf("gate read before open: %q", got)
	default:
	}
	gate.open()
	gate.open()
	if got := <-readDone; got != "ok" {
		t.Fatalf("gate read = %q, want ok", got)
	}
}

func TestMainUsesRunExitCode(t *testing.T) {
	originalArgs := os.Args
	originalStdin := os.Stdin
	originalStartAgent := startAgent
	originalGetwd := getwd
	originalExit := exit
	t.Cleanup(func() {
		os.Args = originalArgs
		os.Stdin = originalStdin
		startAgent = originalStartAgent
		getwd = originalGetwd
		exit = originalExit
	})

	dir := t.TempDir()
	sessionFile := filepath.Join(dir, defaultSessionFile)
	if err := os.WriteFile(sessionFile, []byte(`{"session_id":"stored"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	stdinFile, err := os.CreateTemp(dir, "stdin")
	if err != nil {
		t.Fatalf("create stdin: %v", err)
	}
	if _, err := stdinFile.WriteString("\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	if _, err := stdinFile.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek stdin: %v", err)
	}
	os.Stdin = stdinFile
	os.Args = []string{"resume-from-file", "-session-file", sessionFile}
	getwd = func() (string, error) { return dir, nil }
	startAgent = func(context.Context, io.Writer, io.Writer, codexacp.SessionStore) (*startedAgent, error) {
		return &startedAgent{conn: &fakeAgentConnection{}}, nil
	}
	var code int
	exit = func(got int) { code = got }

	main()
	if code != 0 {
		t.Fatalf("main exit = %d, want 0", code)
	}
}

func TestRunImportedResumeTypedPromptBranches(t *testing.T) {
	t.Run("typed prompt sent", func(t *testing.T) {
		conn := &fakeAgentConnection{}
		var stdout bytes.Buffer
		err := runImportedResume(context.Background(), conn, config{sessionID: "stored", cwd: "/tmp/project", prompt: "continue"}, nil, strings.NewReader("typed\n"), &stdout)
		if err != nil {
			t.Fatalf("runImportedResume returned error: %v", err)
		}
		if conn.prompt != "typed" {
			t.Fatalf("last prompt = %q, want typed", conn.prompt)
		}
	})

	t.Run("typed prompt read error", func(t *testing.T) {
		err := runImportedResume(context.Background(), &fakeAgentConnection{}, config{sessionID: "stored", cwd: "/tmp/project", prompt: "continue"}, nil, errReader{}, io.Discard)
		if err == nil {
			t.Fatal("runImportedResume accepted read error")
		}
	})

	t.Run("typed prompt send error", func(t *testing.T) {
		err := runImportedResume(context.Background(), &fakeAgentConnection{afterFirstPromptErr: errors.New("typed prompt")}, config{sessionID: "stored", cwd: "/tmp/project", prompt: "continue"}, nil, strings.NewReader("typed\n"), io.Discard)
		if err == nil {
			t.Fatal("runImportedResume accepted typed prompt error")
		}
	})

	t.Run("interrupted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout bytes.Buffer
		err := runImportedResume(ctx, &fakeAgentConnection{}, config{sessionID: "stored", cwd: "/tmp/project", prompt: "continue"}, nil, strings.NewReader("typed\n"), &stdout)
		if err != nil {
			t.Fatalf("runImportedResume returned error: %v", err)
		}
		if !strings.Contains(stdout.String(), "interrupted") {
			t.Fatalf("stdout = %q", stdout.String())
		}
	})
}

func TestReadTypedPrompt(t *testing.T) {
	var stdout bytes.Buffer
	got, err := readTypedPrompt(context.Background(), strings.NewReader(" hello \n"), &stdout)
	if err != nil || got != "hello" {
		t.Fatalf("readTypedPrompt = %q err=%v", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := readTypedPrompt(ctx, strings.NewReader("hello\n"), io.Discard); !errors.Is(err, errPromptInterrupted) || got != "" {
		t.Fatalf("canceled readTypedPrompt = %q err=%v", got, err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	blocking := &blockingReader{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := readTypedPrompt(ctx, blocking, io.Discard)
		done <- err
	}()
	<-blocking.ready
	cancel()
	if err := <-done; !errors.Is(err, errPromptInterrupted) {
		t.Fatalf("blocked canceled readTypedPrompt err=%v", err)
	}
}

func TestPrintError(t *testing.T) {
	var stderr bytes.Buffer
	printError(&stderr, errors.New("boom"))
	if !strings.Contains(stderr.String(), "resume-from-file: boom") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

type blockingReader struct {
	ready chan struct{}
	once  sync.Once
}

func (r *blockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.ready) })
	select {}
}
