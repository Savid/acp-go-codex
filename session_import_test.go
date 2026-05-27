package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestSessionImportCommitValidation(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	agent := NewAgent()
	if _, err := agent.importCodexSession(ctx, json.RawMessage(`{bad}`)); err == nil {
		t.Fatal("importCodexSession accepted bad JSON")
	}
	if _, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"limit","sessionId":"s","cwd":`+quoteJSON(cwd)+`,"entries":[{"type":"a"}]}`)); err != nil {
		t.Fatalf("seed import returned error: %v", err)
	}
	agent.mu.Lock()
	agent.imports["limit"].count = maxSessionImportEntries
	agent.mu.Unlock()
	if _, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"limit","sessionId":"s","cwd":`+quoteJSON(cwd)+`,"offset":1,"entries":[{"type":"b"}]}`)); err == nil {
		t.Fatal("import accepted entry limit overflow")
	}
	if _, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"bytes","sessionId":"s","cwd":`+quoteJSON(cwd)+`,"entries":[{"type":"a"}]}`)); err != nil {
		t.Fatalf("seed byte import returned error: %v", err)
	}
	agent.mu.Lock()
	agent.imports["bytes"].bytes = maxSessionImportBytes
	agent.mu.Unlock()
	if _, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"bytes","sessionId":"s","cwd":`+quoteJSON(cwd)+`,"offset":1,"entries":[{"type":"b"}]}`)); err == nil {
		t.Fatal("import accepted byte limit overflow")
	}

	if _, _, err := validateSessionImportEntries([]json.RawMessage{json.RawMessage(``)}); err == nil {
		t.Fatal("validate entries accepted empty entry")
	}
	if _, _, err := validateSessionImportEntries([]json.RawMessage{json.RawMessage(`{} {}`)}); err == nil {
		t.Fatal("validate entries accepted trailing JSON")
	}
	if _, _, err := validateSessionImportEntries([]json.RawMessage{json.RawMessage(`null`)}); err == nil {
		t.Fatal("validate entries accepted null")
	}
	if err := validateRequiredAbsolutePath("cwd", " "); err == nil {
		t.Fatal("blank absolute path validated")
	}
	if isSafeSessionSubpath(`\abs`) || isSafeSessionSubpath(`a/.`) || isSafeSessionSubpath("bad\x00path") {
		t.Fatal("unsafe subpath validated")
	}

	base := &sessionImport{ImportID: "imp", SessionID: "s", Cwd: cwd, ProjectKey: "p", entries: map[SessionKey][]SessionStoreEntry{}, order: []SessionKey{{ProjectKey: "p", SessionID: "s"}}}
	if err := (&Agent{}).replaceStoreImport(ctx, loadErrorStore{}, base); err == nil {
		t.Fatal("replaceStoreImport ignored load error")
	}
	if err := (&Agent{}).replaceStoreImport(ctx, existingNoReplaceStore{}, base); err == nil {
		t.Fatal("replaceStoreImport accepted existing session without replacer")
	}
	if err := (&Agent{}).replaceStoreImport(ctx, replaceErrorStore{}, base); err == nil {
		t.Fatal("replaceStoreImport ignored replace error")
	}
	if err := (&Agent{}).replaceStoreImport(ctx, appendErrorStore{}, base); err == nil {
		t.Fatal("replaceStoreImport ignored append error")
	}

	importStoreAgent := NewAgent(WithSessionStore(appendErrorStore{}))
	chunk, err := importStoreAgent.importCodexSessionChunk(ctx, json.RawMessage(`{"sessionId":"commit","cwd":`+quoteJSON(cwd)+`,"entries":[{"type":"a"}]}`))
	if err != nil {
		t.Fatalf("commit seed returned error: %v", err)
	}
	importID := chunk[jsonFieldImportID].(string)
	if _, err := importStoreAgent.commitCodexSessionImport(ctx, json.RawMessage(`{"importId":`+quoteJSON(importID)+`}`)); err == nil {
		t.Fatal("commit ignored replaceStoreImport error")
	}
}

func TestImportAndMCPErrorPaths(t *testing.T) {
	if err := (&codexSessionImportParams{JSONL: strings.Repeat("x", maxSessionImportLineBytes+1)}).normalizeEntries(); err == nil {
		t.Fatal("normalizeEntries accepted overlong JSONL")
	}
	if _, err := NewAgent().importCodexSessionChunk(context.Background(), json.RawMessage(`{"sessionId":"s","cwd":"/tmp/project","jsonl":"`+strings.Repeat("x", maxSessionImportLineBytes+1)+`"}`)); err == nil {
		t.Fatal("import chunk ignored normalizeEntries error")
	}
	if entries, err := jsonlLinesToRaw("\n{}\n"); err != nil || len(entries) != 1 {
		t.Fatalf("jsonl blank handling entries=%#v err=%v", entries, err)
	}
	if err := validateSessionImportRequest(codexSessionImportParams{SessionID: "s", Cwd: "/tmp/project"}); err == nil {
		t.Fatal("validate import accepted no entries")
	}
	if _, _, err := validateSessionImportEntries([]json.RawMessage{json.RawMessage(`{bad}`)}); err == nil {
		t.Fatal("validate import entries accepted bad JSON")
	}
	if updates, err := rolloutReplayUpdates([]SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}); err != nil || len(updates) != 1 {
		t.Fatalf("rollout replay updates=%#v err=%v", updates, err)
	}
	replayAgent := NewAgent()
	replayAgent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")})
	replaySession := &Session{agent: replayAgent, id: "replay", client: newSpyCodexClient(), codexThreadID: "thread"}
	if err := replaySession.replayThreadHistory(context.Background()); err == nil {
		t.Fatal("replayThreadHistory ignored update error")
	}
	if err := replaySession.replayRollout(context.Background(), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}); err == nil {
		t.Fatal("replayRollout ignored update error")
	}
	if _, err := NewAgent().listCodexThreads(context.Background(), ListSessionsRequest(WithListSessionsCwd("relative")), nil, nil); err == nil {
		t.Fatal("listCodexThreads accepted relative cwd")
	}
	if _, err := NewAgent().loadMaterializedSession(context.Background(), LoadSessionRequest("s", "/tmp/project", WithSessionCodexOptions(CodexOptions{Effort: "bad"})), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg"}`)}); err == nil {
		t.Fatal("loadMaterializedSession accepted invalid metadata")
	}
	loadBridgeAgent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return &errorCodexClient{spyCodexClient: newSpyCodexClient(), resumeErr: errors.New("resume failed")}, nil
	}))
	loadBridgeAgent.setAgentClient(&recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := loadBridgeAgent.loadMaterializedSession(context.Background(), LoadSessionRequest("s", "/tmp/project", WithSessionMCPServers(acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp", Name: "ACP"}})), []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`)}); err == nil {
		t.Fatal("load materialized with bridge ignored resume error")
	}

	origCreateToken := mcpCreateTokenTemp
	t.Cleanup(func() { mcpCreateTokenTemp = origCreateToken })
	mcpCreateTokenTemp = func() (mcpTokenFile, error) { return nil, errors.New("create failed") }
	if _, err := NewAgent(WithMCPProxyCommand("/bin/acp-go-codex")).newMCPSessionBridge(context.Background(), "s", map[string]struct{}{"acp": {}}); err == nil {
		t.Fatal("newMCPSessionBridge ignored token file error")
	}
	mcpCreateTokenTemp = origCreateToken

	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	agent := NewAgent()
	errConn := &messageErrorMCPClient{recordingMCPAgentClient: &recordingMCPAgentClient{recordingAgentClient: newRecordingAgentClient()}}
	agent.setAgentClient(errConn)
	conn := &mcpBridgeConn{
		agent:        agent,
		session:      &mcpSessionBridge{agent: agent, done: make(chan struct{}), conns: make(map[*mcpBridgeConn]struct{})},
		conn:         left,
		enc:          json.NewEncoder(left),
		connectionID: "mcp",
		pending:      make(map[string]chan mcpRPCMessage),
		closed:       make(chan struct{}),
	}
	readCh := make(chan mcpRPCMessage, 1)
	go func() {
		var msg mcpRPCMessage
		_ = json.NewDecoder(right).Decode(&msg)
		readCh <- msg
	}()
	reqID := json.RawMessage("1")
	conn.forwardProxyMessage(context.Background(), mcpRPCMessage{ID: &reqID, Method: "tools/list", Params: json.RawMessage(`{}`)})
	if msg := <-readCh; msg.Error == nil {
		t.Fatalf("MCP message error was not forwarded: %#v", msg)
	}

	nullLeft, nullRight := net.Pipe()
	defer nullLeft.Close()
	defer nullRight.Close()
	nullConn := &mcpBridgeConn{conn: nullLeft, enc: json.NewEncoder(nullLeft), pending: make(map[string]chan mcpRPCMessage), closed: make(chan struct{})}
	done := make(chan mcpRPCMessage, 1)
	go func() {
		var msg mcpRPCMessage
		_ = json.NewDecoder(nullRight).Decode(&msg)
		done <- msg
	}()
	if err := nullConn.sendMCPResult(reqID, nil); err != nil {
		t.Fatalf("send nil MCP result returned error: %v", err)
	}
	if msg := <-done; msg.Result != nil {
		t.Fatalf("nil MCP result = %#v", msg)
	}

	closedLeft, closedRight := net.Pipe()
	defer closedLeft.Close()
	defer closedRight.Close()
	closedConn := &mcpBridgeConn{conn: closedLeft, enc: json.NewEncoder(closedLeft), pending: make(map[string]chan mcpRPCMessage), closed: make(chan struct{})}
	go func() {
		var msg mcpRPCMessage
		_ = json.NewDecoder(closedRight).Decode(&msg)
		for {
			closedConn.mu.Lock()
			for id, ch := range closedConn.pending {
				delete(closedConn.pending, id)
				close(ch)
				closedConn.mu.Unlock()
				return
			}
			closedConn.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	if _, err := closedConn.forwardACPRequest(context.Background(), acp.UnstableMessageMcpRequest{Method: "tools/list"}); err == nil {
		t.Fatal("forwardACPRequest ignored closed pending channel")
	}
	handleAgent := NewAgent()
	handleConn := &mcpBridgeConn{conn: noopConn{}, enc: json.NewEncoder(io.Discard), connectionID: "handle", pending: make(map[string]chan mcpRPCMessage), closed: make(chan struct{})}
	handleAgent.registerMCPConnection(handleConn)
	handleCtx, handleCancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer handleCancel()
	if _, reqErr := handleAgent.handleMCPMessage(handleCtx, mustJSONRaw(acp.UnstableMessageMcpRequest{ConnectionId: "handle", Method: "tools/list"})); reqErr == nil {
		t.Fatal("handleMCPMessage ignored forward request error")
	}
}

func TestSessionImportValidationAndCommitBranches(t *testing.T) {
	agent := NewAgent()
	ctx := context.Background()
	cwd := t.TempDir()

	chunk, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"imp","sessionId":"s","cwd":`+quoteJSON(cwd)+`,"entries":[{"type":"a"}]}`))
	if err != nil {
		t.Fatalf("chunk returned error: %v", err)
	}
	if chunk[jsonFieldOffset] != 1 {
		t.Fatalf("chunk = %#v", chunk)
	}
	if _, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"imp","sessionId":"s","cwd":`+quoteJSON(cwd)+`,"offset":2,"entries":[{"type":"b"}]}`)); err == nil {
		t.Fatal("bad offset succeeded")
	}
	if _, err := agent.commitCodexSessionImport(ctx, json.RawMessage(`{"importId":"imp","sha256":"bad"}`)); err == nil {
		t.Fatal("bad sha succeeded")
	}
	if _, err := agent.abortCodexSessionImport(ctx, json.RawMessage(`{"importId":"missing"}`)); err != nil {
		t.Fatalf("abort missing returned error: %v", err)
	}

	chunk, err = agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"chunked","sessionId":"chunked-session","cwd":`+quoteJSON(cwd)+`,"jsonl":"{\"type\":\"one\"}\n","lines":["{\"type\":\"two\"}"]}`))
	if err != nil {
		t.Fatalf("chunked import returned error: %v", err)
	}
	if chunk[jsonFieldOffset] != 2 {
		t.Fatalf("chunk result = %#v", chunk)
	}
	if _, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"chunked","sessionId":"other","cwd":`+quoteJSON(cwd)+`,"offset":2,"entries":[{}]}`)); err == nil {
		t.Fatal("mismatched session chunk succeeded")
	}
	if _, err := agent.abortCodexSessionImport(ctx, json.RawMessage(`{"importId":"chunked"}`)); err != nil {
		t.Fatalf("abort chunked returned error: %v", err)
	}

	replaceAgent := NewAgent(WithSessionStore(NewInMemorySessionStore()))
	first, err := replaceAgent.importCodexSessionChunk(ctx, json.RawMessage(`{"sessionId":"replace-session","cwd":`+quoteJSON(cwd)+`,"entries":[{"type":"old"}]}`))
	if err != nil {
		t.Fatalf("first replace import returned error: %v", err)
	}
	if _, err := replaceAgent.commitCodexSessionImport(ctx, json.RawMessage(`{"importId":`+quoteJSON(first[jsonFieldImportID].(string))+`}`)); err != nil {
		t.Fatalf("first replace commit returned error: %v", err)
	}
	second, err := replaceAgent.importCodexSessionChunk(ctx, json.RawMessage(`{"sessionId":"replace-session","cwd":`+quoteJSON(cwd)+`,"subpath":"related/extra.jsonl","entries":[{"type":"new"}]}`))
	if err != nil {
		t.Fatalf("second replace import returned error: %v", err)
	}
	if _, err := replaceAgent.commitCodexSessionImport(ctx, json.RawMessage(`{"importId":`+quoteJSON(second[jsonFieldImportID].(string))+`}`)); err != nil {
		t.Fatalf("second replace commit returned error: %v", err)
	}

	invalids := []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"sessionId":"s","cwd":"relative","entries":[{}]}`),
		json.RawMessage(`{"sessionId":"s","cwd":` + quoteJSON(cwd) + `,"format":"bad","entries":[{}]}`),
		json.RawMessage(`{"sessionId":"s","cwd":` + quoteJSON(cwd) + `,"subpath":"../bad","entries":[{}]}`),
		json.RawMessage(`{"sessionId":"s","cwd":` + quoteJSON(cwd) + `,"entries":[""]}`),
		json.RawMessage(`{"sessionId":"s","cwd":` + quoteJSON(cwd) + `,"entries":["{}{}"]}`),
	}
	for _, raw := range invalids {
		if _, err := agent.importCodexSessionChunk(ctx, raw); err == nil {
			t.Fatalf("invalid import succeeded: %s", raw)
		}
	}
}

func TestSessionImportAdditionalBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	agent := NewAgent()
	if _, err := agent.importCodexSessionChunk(canceledContext(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("import chunk with canceled context succeeded")
	}
	if _, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{bad}`)); err == nil {
		t.Fatal("import chunk accepted bad JSON")
	}
	if _, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"new","sessionId":"s","cwd":`+quoteJSON(cwd)+`,"offset":1,"entries":[{"type":"a"}]}`)); err == nil {
		t.Fatal("new import accepted non-zero offset")
	}
	chunk, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"same","sessionId":"s","cwd":`+quoteJSON(cwd)+`,"entries":[{"type":"a"}]}`))
	if err != nil {
		t.Fatalf("first chunk returned error: %v", err)
	}
	if _, err := agent.importCodexSessionChunk(ctx, json.RawMessage(`{"importId":"same","sessionId":"s","cwd":"/tmp/other","offset":1,"entries":[{"type":"b"}]}`)); err == nil {
		t.Fatal("chunk accepted mismatched cwd")
	}
	if _, err := agent.commitCodexSessionImport(ctx, json.RawMessage(`{bad}`)); err == nil {
		t.Fatal("commit accepted bad JSON")
	}
	if _, err := agent.commitCodexSessionImport(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("commit accepted missing import id")
	}
	if _, err := agent.commitCodexSessionImport(ctx, json.RawMessage(`{"importId":"missing"}`)); err == nil {
		t.Fatal("commit accepted unknown import")
	}
	if _, err := agent.abortCodexSessionImport(ctx, json.RawMessage(`{bad}`)); err == nil {
		t.Fatal("abort accepted bad JSON")
	}
	if _, err := agent.abortCodexSessionImport(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("abort accepted missing import id")
	}
	agent.mu.Lock()
	agent.imports["nil"] = nil
	agent.imports["stale"] = &sessionImport{ImportID: "stale", UpdatedAt: time.Now().Add(-sessionImportTTL - time.Minute)}
	agent.reapStaleSessionImportsLocked(time.Now())
	_, nilExists := agent.imports["nil"]
	_, staleExists := agent.imports["stale"]
	agent.mu.Unlock()
	if nilExists || staleExists {
		t.Fatal("reapStaleSessionImportsLocked did not remove stale imports")
	}
	if _, err := jsonlLinesToRaw(strings.Repeat("x", maxSessionImportLineBytes+1)); err == nil {
		t.Fatal("jsonlLinesToRaw accepted overlong line")
	}
	req := codexSessionImportParams{SessionID: "s", Cwd: cwd, Offset: -1, Entries: []json.RawMessage{json.RawMessage(`{}`)}}
	if err := validateSessionImportRequest(req); err == nil {
		t.Fatal("validateSessionImportRequest accepted negative offset")
	}
	req.Offset = 0
	req.Entries = make([]json.RawMessage, maxSessionImportChunkEntries+1)
	if err := validateSessionImportRequest(req); err == nil {
		t.Fatal("validateSessionImportRequest accepted too many entries")
	}
	if _, _, err := validateSessionImportEntries([]json.RawMessage{json.RawMessage(strings.Repeat("x", maxSessionImportLineBytes+1))}); err == nil {
		t.Fatal("validateSessionImportEntries accepted overlong entry")
	}
	if _, _, err := validateSessionImportEntries([]json.RawMessage{json.RawMessage(`[]`)}); err == nil {
		t.Fatal("validateSessionImportEntries accepted array")
	}
	if !isSafeSessionSubpath("a/b") || isSafeSessionSubpath("") || isSafeSessionSubpath("/abs") || isSafeSessionSubpath("a/../b") || isSafeSessionSubpath("a:b") {
		t.Fatal("isSafeSessionSubpath failed")
	}
	if chunk[jsonFieldImportID] == "" {
		t.Fatalf("chunk missing import id: %#v", chunk)
	}
}

func TestSessionStoreImportMaterializeAndList(t *testing.T) {
	store := NewInMemorySessionStore()
	client := newSpyCodexClient()
	agent := NewAgent(
		WithSessionStore(store),
		withClientFactory(func(context.Context, codex.Options) (codex.Client, error) { return client, nil }),
	)
	ctx := context.Background()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	if err != nil {
		t.Fatalf("project key returned error: %v", err)
	}

	importParams := json.RawMessage(`{"sessionId":"stored-session","cwd":` + quoteJSON(cwd) + `,"entries":[{"type":"turn_context","payload":{"cwd":"/repo"}},{"type":"response_item","payload":{"type":"message","role":"assistant"}}]}`)
	result, err := agent.HandleExtensionMethod(ctx, codexSessionImportMethod, importParams)
	if err != nil {
		t.Fatalf("import returned error: %v", err)
	}
	resultMap, ok := result.(map[string]any)
	if !ok || resultMap[jsonFieldEntries] != 2 {
		t.Fatalf("import result = %#v", result)
	}

	entries, err := store.Load(ctx, SessionKey{ProjectKey: projectKey, SessionID: "stored-session"})
	if err != nil || len(entries) != 2 {
		t.Fatalf("store Load entries=%d err=%v", len(entries), err)
	}
	path, err := materializeRollout(entries)
	if err != nil {
		t.Fatalf("materialize returned error: %v", err)
	}
	defer func() { _ = removeMaterializedRollout(path) }()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("materialized file missing: %v", err)
	}

	loadResp, err := agent.LoadSession(ctx, LoadSessionRequest("stored-session", cwd))
	if err != nil {
		t.Fatalf("LoadSession returned error: %v", err)
	}
	codexMeta, ok := loadResp.Meta[codexMetaKey].(map[string]any)
	if !ok || codexMeta[codexThreadIDMetaKey] == "" {
		t.Fatalf("load meta = %#v", loadResp.Meta)
	}
	if client.resume.Path == "" {
		t.Fatal("LoadSession did not resume from a materialized rollout path")
	}

	listResp, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	if err != nil {
		t.Fatalf("ListSessions returned error: %v", err)
	}
	if len(listResp.Sessions) == 0 {
		t.Fatal("stored session was not listed")
	}
}
