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
	fakeCodexEnv     = "ACP_GO_CODEX_TEST_FAKE"
	fakeCodexEnvDump = "ACP_GO_CODEX_TEST_ENV_DUMP"
	// fakeCodexEnvRequestSent signals that a native callback is on stdout.
	fakeCodexEnvRequestSent = "ACP_GO_CODEX_TEST_REQUEST_SENT"
	// fakeCodexEnvResumeHold names a file the fake creates when a thread/resume
	// arrives that it will never answer, so a test can act while the adapter is
	// still rebinding the thread.
	fakeCodexEnvResumeHold = "ACP_GO_CODEX_TEST_RESUME_HOLD"
	// fakeCodexEnvAccount selects the account answers: a ChatGPT pro login with
	// the snapshots below, no login, an API-key login, a ChatGPT login with no
	// window, or a rate-limit read the app-server rejects.
	fakeCodexEnvAccount    = "ACP_GO_CODEX_TEST_ACCOUNT"
	fakeCodexAccountNone   = "none"
	fakeCodexAccountAPIKey = "apiKey"
	fakeCodexAccountEmpty  = "empty"
	fakeCodexAccountRefuse = "refuse"

	// The native window members of one rate-limit snapshot.
	fakeWindowPrimary   = "primary"
	fakeWindowSecondary = "secondary"

	// tinyPNG is a valid 1x1 PNG.
	tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

	// noNativeRowsDir is the workspace base name that makes the fake open a
	// thread whose rollout file exists with no rows at all.
	noNativeRowsDir = "no-native-rows"
)

var fakeModels = []map[string]any{
	{"id": "vision", "displayName": "Fake Vision", "contextWindow": 1000, "inputModalities": []string{"text", "image"},
		"defaultReasoningEffort": "medium", "supportedReasoningEfforts": []map[string]any{{"reasoningEffort": "low"}, {"reasoningEffort": "medium"}}},
	{"id": "text-only", "displayName": "Fake Text", "contextWindow": 500, "inputModalities": []string{"text"}},
}

// fakeAccountUsage is the native account/rateLimits/read shape: the per-limit
// map beside a bare snapshot and credit members the adapter never reads.
var fakeAccountUsage = map[string]any{
	"ordinaryUsageAllowed": true,
	"rateLimits":           map[string]any{"limitId": "codex", "limitName": nil, fakeWindowPrimary: map[string]any{"usedPercent": 23, "windowDurationMins": 10080, "resetsAt": 1789960938}, fakeWindowSecondary: nil, "credits": map[string]any{"hasCredits": false}, "planType": "pro"},
	"rateLimitsByLimitId": map[string]any{
		"codex":           map[string]any{"limitId": "codex", "limitName": nil, fakeWindowPrimary: map[string]any{"usedPercent": 23, "windowDurationMins": 10080, "resetsAt": 1789960938}, fakeWindowSecondary: nil, "planType": "pro"},
		"codex_bengalfox": map[string]any{"limitId": "codex_bengalfox", "limitName": "GPT-5.3-Codex-Spark", fakeWindowPrimary: map[string]any{"usedPercent": 0, "windowDurationMins": 300, "resetsAt": 1789616611}, fakeWindowSecondary: map[string]any{"usedPercent": 0, "windowDurationMins": 10080, "resetsAt": 1790203411}, "planType": "pro"},
	},
	"rateLimitResetCredits": map[string]any{"availableCount": 1},
	"accountId":             "acct-1",
}

type fakeThread struct {
	id      string
	cwd     string
	path    string
	turn    string
	abort   chan struct{}
	done    chan struct{}
	stuck   bool
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

// rawLine writes one line that is not a JSON-RPC frame onto the same stdout
// the frames use.
func (f *fakeCodex) rawLine(line string) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()

	_, _ = fmt.Fprintln(f.out, line)
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
	if path := os.Getenv(fakeCodexEnvRequestSent); path != "" {
		_ = os.WriteFile(path, nil, 0o600)
	}

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
	case "account/read":
		if frame.Params == nil {
			f.fail(frame.ID, -32602, "Invalid params: missing field `params`")

			return
		}

		switch os.Getenv(fakeCodexEnvAccount) {
		case fakeCodexAccountNone:
			f.respond(frame.ID, map[string]any{"account": nil, "requiresOpenaiAuth": true})
		case fakeCodexAccountAPIKey:
			f.respond(frame.ID, map[string]any{"account": map[string]any{"type": fakeCodexAccountAPIKey}, "requiresOpenaiAuth": true})
		default:
			f.respond(frame.ID, map[string]any{"account": map[string]any{"type": "chatgpt", "email": "fake@example.test", "planType": "pro"}, "requiresOpenaiAuth": true})
		}
	case "account/rateLimits/read":
		if exclude, _ := frame.Params["excludeResetCreditDetails"].(bool); !exclude {
			f.fail(frame.ID, -32602, "Invalid params: excludeResetCreditDetails must be true")

			return
		}

		switch os.Getenv(fakeCodexEnvAccount) {
		case fakeCodexAccountEmpty:
			// A snapshot with neither window is the only no-allowance shape the
			// app-server produces; it never answers with no snapshot at all.
			snapshot := map[string]any{"limitId": "codex", "limitName": nil, fakeWindowPrimary: nil, fakeWindowSecondary: nil, "planType": "pro"}
			f.respond(frame.ID, map[string]any{"ordinaryUsageAllowed": nil, "rateLimits": snapshot, "rateLimitsByLimitId": map[string]any{"codex": snapshot}})
		case fakeCodexAccountRefuse:
			f.fail(frame.ID, -32000, "rate limits unavailable")
		default:
			f.respond(frame.ID, fakeAccountUsage)
		}
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

	if filepath.Base(cwd) == noNativeRowsDir {
		_ = os.WriteFile(thread.path, nil, 0o600)
	} else {
		meta := map[string]any{"type": "session_meta", "payload": map[string]any{
			"id": thread.id, "timestamp": time.Now().UTC().Format(time.RFC3339), "cwd": cwd,
		}}
		encoded, _ := json.Marshal(meta)
		_ = os.WriteFile(thread.path, append(encoded, '\n'), 0o600)
		thread.entries = 1
	}

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
		f.fail(id, -32600, "no rollout found for thread id "+threadID)

		return
	}

	rows, err := codex.ReadRows(matches[0])
	if err != nil || len(rows) == 0 {
		f.fail(id, -32000, "rollout unreadable")

		return
	}

	if hold := os.Getenv(fakeCodexEnvResumeHold); hold != "" {
		_ = os.WriteFile(hold, []byte("held\n"), 0o600)

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
	thread.stuck = strings.HasPrefix(message.String(), "STUCK")

	f.respond(id, map[string]any{"turn": map[string]any{"id": thread.turn}})

	go f.runTurn(thread, thread.turn, message.String(), imageCount, params)
}

func (f *fakeCodex) interrupt(id json.RawMessage, params map[string]any) {
	threadID, _ := params["threadId"].(string)

	f.mu.Lock()
	thread := f.threads[threadID]
	f.mu.Unlock()

	if thread != nil && thread.abort != nil && !thread.stuck {
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
	case strings.HasPrefix(message, "STALE"):
		f.notify("item/agentMessage/delta", scoped(map[string]any{"turnId": "previous-turn", "itemId": "old-message", "delta": "stale text"}))
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
	case strings.HasPrefix(message, "NOISE"):
		fmt.Fprintln(os.Stderr, "chatter on stderr")
		f.rawLine("not a json record at all")
		text = "quiet"
	case f.isServerRequestScript(message):
		text = f.serverRequestTurn(message, thread, scoped)
	case strings.HasPrefix(message, "ERROR"):
		status = "failed"
	case strings.HasPrefix(message, "NOTIFYERROR"):
		f.notify("error", scoped(map[string]any{"message": "provider exploded", "willRetry": false}))
	case strings.HasPrefix(message, "RETRYERROR"):
		f.notify("error", scoped(map[string]any{"message": "transient", "willRetry": true}))
	case strings.HasPrefix(message, "SLOW"):
		select {
		case <-thread.abort:
			status = "interrupted"
		case <-time.After(30 * time.Second):
		}
	case strings.HasPrefix(message, "STUCK"):
		time.Sleep(30 * time.Second)
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
		if strings.HasPrefix(message, "DUPLICATE") {
			f.notify("item/completed", scoped(map[string]any{"item": map[string]any{"id": itemID, "type": "agentMessage", "text": text}}))
		}
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

	switch {
	case strings.HasPrefix(message, "TAIL"):
		// State the harness writes after its own completion signal, still
		// naming the turn that just ended.
		f.notify("item/agentMessage/delta", scoped(map[string]any{"itemId": "tail-1", "delta": "tail"}))
	}
}

// fakeModeKey is the native field naming an MCP elicitation's mode.
const fakeModeKey = "mode"

// isServerRequestScript reports whether the message selects one of the server
// request scripts below.
func (f *fakeCodex) isServerRequestScript(message string) bool {
	for _, prefix := range []string{"FILECHANGE", "PERMISSIONS", "MCPTOOL", "MCPELICIT"} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}

	return false
}

// serverRequestTurn raises one server request the keyword names and renders
// the answer as the turn's text.
func (f *fakeCodex) serverRequestTurn(message string, thread *fakeThread, scoped func(map[string]any) map[string]any) string {
	switch {
	case strings.HasPrefix(message, "FILECHANGE"):
		answer := f.request(codex.RequestFileChangeApproval, scoped(map[string]any{"itemId": "fc-1", "grantRoot": thread.cwd, "reason": "Edit files"}))
		if decision, _ := answer["decision"].(string); strings.HasPrefix(decision, "accept") {
			return "edited"
		}

		return "declined"
	case strings.HasPrefix(message, "PERMISSIONS"):
		answer := f.request(codex.RequestPermissionsApproval, scoped(map[string]any{"itemId": "perm-1", "permissions": map[string]any{"network": true}}))
		scope, _ := answer["scope"].(string)

		if granted, _ := answer["permissions"].(map[string]any); len(granted) > 0 {
			return "granted " + scope
		}

		return "declined"
	case strings.HasPrefix(message, "MCPTOOL"):
		answer := f.request(codex.RequestMCPElicitation, scoped(map[string]any{"message": "Call fetch?", "_meta": map[string]any{"codex_approval_kind": "mcp_tool_call", "tool_name": "fetch"}}))
		action, _ := answer["action"].(string)

		return action
	case strings.HasPrefix(message, "MCPELICIT_URL"):
		answer := f.request(codex.RequestMCPElicitation, scoped(map[string]any{"message": "Open the page", fakeModeKey: "url", "url": "https://example.test/authorize", "elicitationId": "url-1"}))
		action, _ := answer["action"].(string)

		return action
	default:
		answer := f.request(codex.RequestMCPElicitation, scoped(map[string]any{"message": "Pick a color", fakeModeKey: "form", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"color": map[string]any{"type": "string"}}, "required": []string{"color"}}}))
		action, _ := answer["action"].(string)

		if content, ok := answer["content"].(map[string]any); ok {
			if color, ok := content["color"].(string); ok {
				action += " " + color
			}
		}

		return action
	}
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
