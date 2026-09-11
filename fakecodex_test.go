package codexacp

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/savid/acp-go-codex/internal/codex"
)

// The test binary doubles as a fake app-server: TestMain runs fakeCodex when
// this variable is set in the environment the adapter launched it with.
const (
	fakeCodexEnv        = "ACP_GO_CODEX_TEST_FAKE"
	fakeCodexEnvDump    = "ACP_GO_CODEX_TEST_ENV_DUMP"
	fakeCodexEnvVersion = "ACP_GO_CODEX_TEST_VERSION"
	fakeCodexVersion    = "0.154.0"

	// tinyPNG is a valid 1x1 PNG.
	tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
)

var fakeModels = []map[string]any{
	{"id": "vision", "displayName": "Fake Vision", "contextWindow": 1000, "inputModalities": []string{"text", "image"},
		"defaultReasoningEffort": "medium", "supportedReasoningEfforts": []map[string]any{{"reasoningEffort": "low"}, {"reasoningEffort": "medium"}}},
	{"id": "text-only", "displayName": "Fake Text", "contextWindow": 500, "inputModalities": []string{"text"}},
}

type fakeThread struct {
	id      string
	cwd     string
	path    string
	turn    string
	abort   chan struct{}
	done    chan struct{}
	entries int
}

type fakeCodex struct {
	home string

	writeMu sync.Mutex
	out     *bufio.Writer

	mu       sync.Mutex
	threads  map[string]*fakeThread
	pending  map[string]chan map[string]any
	nextID   int
	shellEnv map[string]any
}

func runFakeCodex(args []string) int {
	if slices.Contains(args, "--version") {
		version := os.Getenv(fakeCodexEnvVersion)
		if version == "" {
			version = fakeCodexVersion
		}

		fmt.Println("codex-cli " + version)

		return 0
	}

	if dump := os.Getenv(fakeCodexEnvDump); dump != "" {
		_ = os.WriteFile(dump, []byte(strings.Join(os.Environ(), "\n")+"\n"), 0o600)
	}

	if !slices.Contains(args, "app-server") {
		fmt.Fprintln(os.Stderr, "fake codex: unexpected arguments")

		return 2
	}

	home := os.Getenv(codex.EnvCodexHome)
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".codex")
	}

	f := &fakeCodex{
		home:    home,
		out:     bufio.NewWriter(os.Stdout),
		threads: make(map[string]*fakeThread),
		pending: make(map[string]chan map[string]any),
	}

	return f.serve(os.Stdin)
}

func fakeUUID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)

	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func (f *fakeCodex) serve(input io.Reader) int {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > 0 {
			f.dispatch(line)
		}
	}

	return 0
}

func (f *fakeCodex) write(value any) {
	encoded, _ := json.Marshal(value)

	f.writeMu.Lock()
	defer f.writeMu.Unlock()

	_, _ = f.out.Write(encoded)
	_ = f.out.WriteByte('\n')
	_ = f.out.Flush()
}

func (f *fakeCodex) respond(id json.RawMessage, result any) {
	f.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *fakeCodex) fail(id json.RawMessage, code int, message string) {
	f.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (f *fakeCodex) notify(method string, params map[string]any) {
	f.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// request sends one server request and waits for the adapter's answer.
func (f *fakeCodex) request(method string, params map[string]any) map[string]any {
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("req-%d", f.nextID)
	waiter := make(chan map[string]any, 1)
	f.pending[id] = waiter
	f.mu.Unlock()

	f.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})

	select {
	case response := <-waiter:
		return response
	case <-time.After(30 * time.Second):
		return nil
	}
}

func (f *fakeCodex) dispatch(line []byte) {
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params map[string]any  `json:"params"`
		Result map[string]any  `json:"result"`
		Error  map[string]any  `json:"error"`
	}

	if err := json.Unmarshal(line, &frame); err != nil {
		return
	}

	if frame.Method == "" {
		var id string

		_ = json.Unmarshal(frame.ID, &id)

		f.mu.Lock()
		waiter := f.pending[id]
		delete(f.pending, id)
		f.mu.Unlock()

		if waiter != nil {
			result := frame.Result
			if result == nil {
				result = map[string]any{"error": frame.Error}
			}

			waiter <- result
		}

		return
	}

	switch frame.Method {
	case "initialize":
		f.respond(frame.ID, map[string]any{"userAgent": "fake"})
	case "initialized":
	case "model/list":
		f.respond(frame.ID, map[string]any{"models": fakeModels})
	case "thread/start":
		f.startThread(frame.ID, frame.Params)
	case "thread/resume":
		f.resumeThread(frame.ID, frame.Params)
	case "thread/unsubscribe":
		f.respond(frame.ID, map[string]any{})
	case "turn/start":
		f.startTurn(frame.ID, frame.Params)
	case "turn/interrupt":
		f.interrupt(frame.ID, frame.Params)
	default:
		f.fail(frame.ID, -32601, "method not found")
	}
}

func (f *fakeCodex) startThread(id json.RawMessage, params map[string]any) {
	cwd, _ := params["cwd"].(string)
	if model, _ := params["model"].(string); model == "REFUSE" {
		f.fail(id, -32000, "unknown model")

		return
	}

	f.recordConfig(params)

	thread := &fakeThread{id: fakeUUID(), cwd: cwd}
	thread.path = codex.RolloutPath(f.home, thread.id, time.Now())

	_ = os.MkdirAll(filepath.Dir(thread.path), 0o700)

	meta := map[string]any{"type": "session_meta", "payload": map[string]any{
		"id": thread.id, "timestamp": time.Now().UTC().Format(time.RFC3339), "cwd": cwd,
	}}
	encoded, _ := json.Marshal(meta)
	_ = os.WriteFile(thread.path, append(encoded, '\n'), 0o600)
	thread.entries = 1

	f.mu.Lock()
	f.threads[thread.id] = thread
	f.mu.Unlock()

	f.respond(id, map[string]any{"thread": map[string]any{"id": thread.id, "path": thread.path, "cwd": cwd}, "model": params["model"]})
}

// recordConfig keeps the last thread config so a test can read the shell
// environment the thread was given.
func (f *fakeCodex) recordConfig(params map[string]any) {
	config, _ := params["config"].(map[string]any)
	policy, _ := config["shell_environment_policy"].(map[string]any)
	set, _ := policy["set"].(map[string]any)

	f.mu.Lock()
	f.shellEnv = set
	f.mu.Unlock()

	if dump := os.Getenv(fakeCodexEnvDump); dump != "" && set != nil {
		encoded, _ := json.Marshal(set)
		_ = os.WriteFile(dump+".thread", encoded, 0o600)
	}
}

func (f *fakeCodex) resumeThread(id json.RawMessage, params map[string]any) {
	threadID, _ := params["threadId"].(string)
	cwd, _ := params["cwd"].(string)

	f.recordConfig(params)

	matches, _ := filepath.Glob(filepath.Join(f.home, "sessions", "*", "*", "*", "rollout-*-"+threadID+".jsonl"))
	if len(matches) == 0 {
		f.fail(id, -32000, "no rollout found for thread id "+threadID)

		return
	}

	rows, err := codex.ReadRows(matches[0])
	if err != nil || len(rows) == 0 {
		f.fail(id, -32000, "rollout unreadable")

		return
	}

	thread := &fakeThread{id: threadID, cwd: cwd, path: matches[0], entries: len(rows)}

	f.mu.Lock()
	f.threads[threadID] = thread
	f.mu.Unlock()

	f.respond(id, map[string]any{"thread": map[string]any{"id": threadID, "path": matches[0], "cwd": cwd}})
}

func (f *fakeCodex) startTurn(id json.RawMessage, params map[string]any) {
	threadID, _ := params["threadId"].(string)

	f.mu.Lock()
	thread := f.threads[threadID]
	f.mu.Unlock()

	if thread == nil {
		f.fail(id, -32000, "thread not found")

		return
	}

	var message strings.Builder
	imageCount := 0

	if input, ok := params["input"].([]any); ok {
		for _, item := range input {
			block, _ := item.(map[string]any)
			switch block["type"] {
			case "text":
				text, _ := block["text"].(string)
				message.WriteString(text)
			case "image":
				imageCount++
			}
		}
	}

	if strings.HasPrefix(message.String(), "REJECT") {
		f.fail(id, -32000, "no provider key")

		return
	}

	thread.turn = fakeUUID()
	thread.abort = make(chan struct{})
	thread.done = make(chan struct{})

	f.respond(id, map[string]any{"turn": map[string]any{"id": thread.turn}})

	go f.runTurn(thread, thread.turn, message.String(), imageCount, params)
}

func (f *fakeCodex) interrupt(id json.RawMessage, params map[string]any) {
	threadID, _ := params["threadId"].(string)

	f.mu.Lock()
	thread := f.threads[threadID]
	f.mu.Unlock()

	if thread != nil && thread.abort != nil {
		select {
		case <-thread.abort:
		default:
			close(thread.abort)
		}

		<-thread.done
	}

	f.respond(id, map[string]any{})
}

func (f *fakeCodex) appendRow(thread *fakeThread, row map[string]any) {
	thread.entries++
	row["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, _ := json.Marshal(row)

	file, err := os.OpenFile(thread.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}

	_, _ = file.Write(append(encoded, '\n'))
	_ = file.Close()
}

func eventRow(kind string, fields map[string]any) map[string]any {
	payload := map[string]any{"type": kind}
	maps.Copy(payload, fields)

	return map[string]any{"type": "event_msg", "payload": payload}
}

// runTurn drives one scripted turn: the keyword at the start of the message
// selects the script.
func (f *fakeCodex) runTurn(thread *fakeThread, turnID string, message string, imageCount int, params map[string]any) {
	defer close(thread.done)

	base := map[string]any{"threadId": thread.id, "turnId": turnID}
	scoped := func(fields map[string]any) map[string]any {
		out := map[string]any{}
		maps.Copy(out, base)

		maps.Copy(out, fields)

		return out
	}

	f.appendRow(thread, eventRow("user_message", map[string]any{"message": message}))
	f.notify("turn/started", scoped(map[string]any{"turn": map[string]any{"id": turnID}}))

	if strings.HasPrefix(message, "DIE") {
		fmt.Fprintln(os.Stderr, "fatal: dead")
		os.Exit(3)
	}

	status := "completed"
	text := "Hello world"
	itemID := "msg-" + fakeUUID()

	delta := func(chunk string) {
		f.notify("item/agentMessage/delta", scoped(map[string]any{"itemId": itemID, "delta": chunk}))
	}

	switch {
	case strings.HasPrefix(message, "SUFFIX"):
		delta("Hel")
		text = "Hello"
	case strings.HasPrefix(message, "THINK"):
		f.notify("item/reasoning/textDelta", scoped(map[string]any{"itemId": "r1", "delta": "hmm"}))
		f.notify("item/completed", scoped(map[string]any{"item": map[string]any{"id": "r1", "type": "reasoning", "text": "hmm"}}))
		delta("ok")
		text = "ok"
	case strings.HasPrefix(message, "TOOL"):
		f.tool(thread, scoped, strings.HasPrefix(message, "TOOLIMAGE"))
		text = "done"
	case strings.HasPrefix(message, "ASK"):
		answer := f.request("item/tool/requestUserInput", scoped(map[string]any{"itemId": "ask-1", "questions": []map[string]any{{"id": "name", "header": "Name", "question": "Your name?"}}}))
		answers, _ := answer["answers"].(map[string]any)
		name, _ := answers["name"].(map[string]any)
		values, _ := name["answers"].([]any)

		text = "declined"
		if len(values) > 0 {
			text = fmt.Sprintf("hi %v", values[0])
		}
	case strings.HasPrefix(message, "ERROR"):
		status = "failed"
	case strings.HasPrefix(message, "SLOW"):
		select {
		case <-thread.abort:
			status = "interrupted"
		case <-time.After(30 * time.Second):
		}
	case strings.HasPrefix(message, "JSON"):
		text = `{"answer": 42}`
	case strings.HasPrefix(message, "PLAN"):
		f.notify("turn/plan/updated", scoped(map[string]any{"plan": []map[string]any{{"step": "one", "status": "completed"}, {"step": "two", "status": "inProgress"}}}))
	case strings.HasPrefix(message, "ECHO"):
		text = message
	default:
		delta("Hello")
		delta(" world")
	}

	if imageCount > 0 {
		text += fmt.Sprintf(" images:%d", imageCount)
	}

	if schema, ok := params["outputSchema"]; ok && schema != nil && !strings.HasPrefix(message, "JSON") {
		text = `{"echo": true}`
	}

	if status == "completed" {
		f.notify("item/completed", scoped(map[string]any{"item": map[string]any{"id": itemID, "type": "agentMessage", "text": text}}))
		f.appendRow(thread, eventRow("agent_message", map[string]any{"message": text}))
	}

	f.notify("thread/tokenUsage/updated", scoped(map[string]any{"tokenUsage": map[string]any{
		"last": map[string]any{"inputTokens": 10, "outputTokens": 5, "totalTokens": 15}, "modelContextWindow": 1000,
	}}))

	turn := map[string]any{"id": turnID, "status": status}
	if status == "failed" {
		turn["error"] = map[string]any{"message": "boom", "codexErrorInfo": map[string]any{"httpStatusCode": 429, "code": "rate_limited"}}
	}

	f.notify("turn/completed", scoped(map[string]any{"turn": turn}))
}

func (f *fakeCodex) tool(thread *fakeThread, scoped func(map[string]any) map[string]any, withImage bool) {
	answer := f.request("item/commandExecution/requestApproval", scoped(map[string]any{"itemId": "call-1", "command": []string{"ls"}, "cwd": thread.cwd}))

	decision, _ := answer["decision"].(string)
	allowed := decision == "accept" || decision == "acceptForSession"

	f.notify("item/started", scoped(map[string]any{"item": map[string]any{"id": "call-1", "type": "commandExecution", "command": []string{"ls"}, "status": "inProgress"}}))

	status := "declined"
	output := "Denied by ACP client"

	if allowed {
		status = "completed"
		output = "out\n"

		f.notify("item/commandExecution/outputDelta", scoped(map[string]any{"itemId": "call-1", "delta": "out"}))
	}

	f.notify("item/completed", scoped(map[string]any{"item": map[string]any{"id": "call-1", "type": "commandExecution", "command": []string{"ls"}, "status": status, "aggregatedOutput": output}}))
	f.appendRow(thread, map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "shell", "call_id": "call-1", "arguments": `{"command":["ls"]}`}})
	f.appendRow(thread, map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "call-1", "output": output}})

	if withImage {
		f.notify("item/started", scoped(map[string]any{"item": map[string]any{"id": "img-1", "type": "imageGeneration", "status": "inProgress"}}))
		f.notify("item/completed", scoped(map[string]any{"item": map[string]any{"id": "img-1", "type": "imageGeneration", "status": "completed", "result": tinyPNG}}))
		f.appendRow(thread, map[string]any{"type": "response_item", "payload": map[string]any{"type": "image_generation_call", "id": "img-1", "status": "completed", "result": tinyPNG}})
	}
}
