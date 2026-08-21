package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func assertTurnFailureParse(t *testing.T) {
	t.Helper()

	failed := eventFromRPC(rpcEvent{Method: "turn/completed", Params: mustRaw(map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"turn": map[string]any{
			"status": "failed",
			"error": map[string]any{
				"message": "provider rate limited",
				"codexErrorInfo": map[string]any{
					"httpStatusCode": float64(429),
					"code":           "rate_limit",
				},
			},
		},
	})})
	if failed.Kind != EventCompleted || failed.StopReason != StopReasonError {
		t.Fatalf("failed turn kind=%v stop=%v", failed.Kind, failed.StopReason)
	}

	var tf *TurnFailedError
	if !errors.As(failed.Err, &tf) {
		t.Fatalf("failed turn err = %v, want TurnFailedError", failed.Err)
	}
	if tf.Cause != CauseProvider || tf.StatusCode != 429 || tf.ProviderCode != "rate_limit" || tf.Message != turnFailedText {
		t.Fatalf("turn failure = %#v", tf)
	}
	if tf.Error() != turnFailedText || (*TurnFailedError)(nil).Error() != "" {
		t.Fatalf("TurnFailedError.Error mismatch: %q", tf.Error())
	}

	bare := eventFromRPC(rpcEvent{Method: "turn/completed", Params: mustRaw(map[string]any{
		"turn": map[string]any{"status": "errored"},
	})})
	var bareErr *TurnFailedError
	if !errors.As(bare.Err, &bareErr) || bareErr.Message != turnFailedText || bareErr.StatusCode != 0 {
		t.Fatalf("bare turn failure = %#v", bare.Err)
	}

	ok := eventFromRPC(rpcEvent{Method: "turn/completed", Params: mustRaw(map[string]any{
		"turn": map[string]any{"status": statusDone},
	})})
	if ok.Err != nil {
		t.Fatalf("successful turn carried error: %v", ok.Err)
	}
}

func TestAppServerClientMethodsAndParams(t *testing.T) {
	transport := newScriptTransport()
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	ctx := context.Background()
	thread, err := client.StartThread(ctx, ThreadStartRequest{
		Cwd:                   "/repo",
		AdditionalDirectories: []string{"/extra"},
		Model:                 "gpt-test",
		ServiceTier:           "flex",
		Personality:           "pragmatic",
		Config:                map[string]any{"mcp_servers": map[string]any{"marker": map[string]any{"url": "https://a"}}},
	})
	if err != nil {
		t.Fatalf("StartThread returned error: %v", err)
	}
	if thread.ID != "thread-1" || thread.Path != "/tmp/rollout.jsonl" || thread.UpdatedAt == "" || thread.ReasoningEffort != "high" {
		t.Fatalf("thread = %#v", thread)
	}
	start := transport.sentParams(methodThreadStart)
	if start["permissions"] == nil || start["personality"] != "pragmatic" || start["serviceTier"] != "flex" || start["config"] == nil {
		t.Fatalf("thread/start params = %#v", start)
	}
	client.closeThread(thread.ID)
	if _, startErr := client.StartThread(ctx, ThreadStartRequest{Personality: ""}); startErr != nil {
		t.Fatalf("StartThread with empty personality returned error: %v", startErr)
	}
	if _, ok := transport.sentParams(methodThreadStart)["personality"]; ok {
		t.Fatalf("empty personality was sent: %#v", transport.sentParams(methodThreadStart))
	}

	if _, resumeErr := client.ResumeThread(ctx, ThreadResumeRequest{ThreadID: "thread-1", Path: "/tmp/rollout.jsonl", Cwd: "/repo", Config: map[string]any{"mcp_servers": map[string]any{"marker": map[string]any{"url": "https://a"}}}}); resumeErr != nil {
		t.Fatalf("ResumeThread returned error: %v", resumeErr)
	}
	resume := transport.sentParams(methodThreadResume)
	if resume["config"] == nil {
		t.Fatalf("thread/resume omitted config: %#v", resume)
	}
	if _, forkErr := client.ForkThread(ctx, ThreadForkRequest{ThreadID: "thread-1", Cwd: "/repo"}); forkErr != nil {
		t.Fatalf("ForkThread returned error: %v", forkErr)
	}
	threads, err := client.ListThreads(ctx, ThreadListRequest{Cwd: "/repo"})
	if err != nil || len(threads) != 1 {
		t.Fatalf("ListThreads len=%d err=%v", len(threads), err)
	}
	history, err := client.ReadThread(ctx, ThreadReadRequest{ThreadID: "thread-1"})
	if err != nil || len(history.Items) != 3 || len(history.Events) != 2 {
		t.Fatalf("ReadThread items=%d err=%v", len(history.Items), err)
	}
	historyJSON, err := json.Marshal(history.Raw)
	if err != nil || strings.Contains(string(historyJSON), "eA==") || strings.Contains(string(historyJSON), "/secret/") {
		t.Fatalf("ReadThread exposed image payload: %s err=%v", historyJSON, err)
	}
	turns, err := client.ListTurns(ctx, ThreadTurnsListRequest{ThreadID: "thread-1", Limit: 2, SortDirection: "asc"})
	if err != nil || len(turns.Turns) != 1 || turns.NextCursor != "next" {
		t.Fatalf("ListTurns = %#v err=%v", turns, err)
	}
}

func TestAppServerClientTurnAndDiscoveryMethods(t *testing.T) {
	transport := newScriptTransport()
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())

	ctx := context.Background()
	if steerErr := client.SteerTurn(ctx, TurnSteerRequest{ThreadID: "thread-1", ExpectedTurnID: "turn-1", Input: []UserInput{{"type": "text", "text": "more"}}}); steerErr != nil {
		t.Fatalf("SteerTurn returned error: %v", steerErr)
	}
	if cancelErr := client.CancelTurn(ctx, "thread-1", "turn-1"); cancelErr != nil {
		t.Fatalf("CancelTurn returned error: %v", cancelErr)
	}
	if _, compactErr := client.CompactThread(ctx, ThreadCompactRequest{ThreadID: "thread-1"}); compactErr != nil {
		t.Fatalf("CompactThread returned error: %v", compactErr)
	}
	if _, reviewErr := client.StartReview(ctx, ReviewStartRequest{ThreadID: "thread-1", Target: map[string]any{"type": "uncommittedChanges"}, Delivery: "inline"}); reviewErr != nil {
		t.Fatalf("StartReview returned error: %v", reviewErr)
	}
	modes, err := client.CollaborationModeList(ctx)
	if err != nil || len(modes.Modes) != 2 || modes.Modes[0].ID != "default" {
		t.Fatalf("CollaborationModeList = %#v err=%v", modes, err)
	}
	status, err := client.MCPServerStatusList(ctx, "thread-1")
	if err != nil || len(status.Servers) != 1 || len(status.Servers[0].Tools) != 1 {
		t.Fatalf("MCPServerStatusList = %#v err=%v", status, err)
	}
	if unsubErr := client.UnsubscribeThread(ctx, "thread-1"); unsubErr != nil {
		t.Fatalf("UnsubscribeThread returned error: %v", unsubErr)
	}
	if deleteErr := client.DeleteThread(ctx, ThreadDeleteRequest{ThreadID: "thread-1"}); deleteErr != nil {
		t.Fatalf("DeleteThread returned error: %v", deleteErr)
	}
}

func TestAppServerClientModelAndAccountMethods(t *testing.T) {
	transport := newScriptTransport()
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())

	ctx := context.Background()
	models, err := client.ModelList(ctx)
	if err != nil || len(models) != 2 || models[0].ID != "gpt-a" || models[0].Name != "GPT A" || models[0].Description != "A model" || models[0].DefaultReasoningEffort != "medium" || len(models[0].ReasoningEfforts) != 2 || len(models[0].InputModalities) != 2 {
		t.Fatalf("ModelList = %#v err=%v", models, err)
	}
	account, err := client.AccountRead(ctx)
	if err != nil || account.ID != "acct" || account.PlanType != "plus" {
		t.Fatalf("AccountRead = %#v err=%v", account, err)
	}
	if loginErr := client.LoginWithChatGPTTokens(ctx, ChatGPTAuthTokens{AccessToken: "a", RefreshToken: "r", AccountID: "acct", PlanType: "plus", ExpiresAtUnixSec: 123}); loginErr != nil {
		t.Fatalf("LoginWithChatGPTTokens returned error: %v", loginErr)
	}
	if logoutErr := client.Logout(ctx); logoutErr != nil {
		t.Fatalf("Logout returned error: %v", logoutErr)
	}
}

func TestNewAppServerClientLaunchesCLI(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	script := filepath.Join(testTraversableTempDir(t), "codex")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "--version" ]; then
  echo codex-cli 0.144.1
  exit 0
fi
read line || exit 0
echo '{"jsonrpc":"2.0","id":1,"result":{}}'
read line || true
while read line; do :; done
`), nativeScriptMode); err != nil {
		t.Fatalf("write codex script: %v", err)
	}
	client, err := NewAppServerClient(context.Background(), Options{
		CLIPath: script, CodexHome: testNativeOwnedTempDir(t), SupervisorRoot: testTraversableTempDir(t),
		SupervisorParent: os.TempDir(),
		NativeVersion:    minCodexVersion, LaunchTimeout: 5 * time.Second,
		ProcessIsolation: testProcessIsolation(),
	})
	if err != nil {
		t.Fatalf("NewAppServerClient returned error: %v", err)
	}
	if err := client.Close(context.Background()); err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("Close returned error: %v", err)
	}
}

func TestAppServerRunTurnMapsEvents(t *testing.T) {
	transport := newScriptTransport()
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	feed := claimRoutedThread(t, client, "thread-1")

	_, err := client.RunTurn(context.Background(), TurnStartRequest{
		ThreadID:          "thread-1",
		Prompt:            []UserInput{{"type": "text", "text": "hello"}},
		Model:             "gpt-a",
		ReasoningEffort:   "high",
		ServiceTier:       "flex",
		Personality:       "pragmatic",
		CollaborationMode: map[string]any{"mode": "plan", "settings": map[string]any{"model": "gpt-a"}},
		OutputSchema:      map[string]any{"type": "object"},
	})
	if err != nil {
		t.Fatalf("RunTurn returned error: %v", err)
	}
	params := transport.sentParams(methodTurnStart)
	if params["effort"] != "high" || params["serviceTier"] != "flex" || params["collaborationMode"] == nil {
		t.Fatalf("turn/start params = %#v", params)
	}

	got := make([]Event, 0, 6)
	for range 6 {
		event, open := awaitEvent(t, feed.Events)
		if !open {
			t.Fatal("thread broker closed at turn completion")
		}
		got = append(got, event)
	}
	if len(got) != 6 {
		t.Fatalf("events = %#v", got)
	}
	if got[0].Kind != EventPlanUpdated || got[1].Kind != EventReasoningDelta || got[2].Kind != EventAgentMessageDelta || got[3].Kind != EventDiffUpdated || got[4].Kind != EventUsageUpdated || got[5].Kind != EventCompleted {
		t.Fatalf("unexpected event order: %#v", got)
	}
	if got[4].Usage.TotalTokens != 3 ||
		got[4].TokenUsage.Last.CachedReadTokens != 1 ||
		got[4].TokenUsage.Last.ReasoningOutputTokens != 1 ||
		got[4].TokenUsage.Total.TotalTokens != 9 ||
		got[4].TokenUsage.ModelContextWindow != 100 {
		t.Fatalf("usage = %#v tokenUsage=%#v", got[4].Usage, got[4].TokenUsage)
	}
}

func TestAppServerRunTurnEmitsConnectionError(t *testing.T) {
	transport := newAbruptCloseTransport()
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	feed := claimRoutedThread(t, client, "thread-1")

	_, err := client.RunTurn(context.Background(), TurnStartRequest{ThreadID: "thread-1"})
	if err != nil {
		t.Fatalf("RunTurn returned error: %v", err)
	}
	transport.CloseWithError(errors.New("boom"))
	event, ok := <-feed.Events
	if !ok {
		t.Fatal("turn stream closed without an error event")
	}
	if event.Kind != EventError || !errors.Is(event.Err, ErrConnectionClosed) {
		t.Fatalf("event = %#v, want connection error", event)
	}
	if _, ok := <-feed.Events; ok {
		t.Fatal("turn stream stayed open after connection error")
	}
}

func TestAppServerMappingHelpers(t *testing.T) {
	thread, err := threadFromResponse(map[string]any{"thread": map[string]any{
		"id":        "id",
		"sessionId": "sid",
		"path":      "/tmp/a",
		"updatedAt": float64(10),
	}})
	if err != nil {
		t.Fatalf("thread decode: %v", err)
	}
	if thread.ID != "id" || thread.SessionID != "sid" || thread.UpdatedAt == "" {
		t.Fatalf("thread = %#v", thread)
	}

	if planStatusFromString("running") != PlanStepInProgress || planStatusFromString("done") != PlanStepCompleted || planStatusFromString("other") != PlanStepPending {
		t.Fatal("plan status mapping failed")
	}
	if stopReasonFromTurn(map[string]any{"status": "cancelled"}) != StopReasonCancelled || stopReasonFromTurn(map[string]any{"status": "failed"}) != StopReasonError {
		t.Fatal("stop reason mapping failed")
	}

	assertTurnFailureParse(t)
	usage := usageFromParams(map[string]any{"usage": map[string]any{
		"inputTokens":              float64(1),
		"completionTokens":         float64(2),
		"cachedReadTokens":         float64(3),
		"cacheCreationInputTokens": float64(4),
		"reasoning_output_tokens":  float64(5),
	}})
	if usage.TotalTokens != 3 || usage.CachedReadTokens != 3 || usage.CachedWriteTokens != 4 || usage.ReasoningOutputTokens != 5 {
		t.Fatalf("usage = %#v", usage)
	}
	tokenUsage := tokenUsageFromParams(map[string]any{"tokenUsage": map[string]any{
		"last":               map[string]any{"input_tokens": float64(1), "output_tokens": float64(2)},
		"total":              map[string]any{"inputTokens": float64(4), "outputTokens": float64(5)},
		"modelContextWindow": float64(200),
	}})
	if tokenUsage.Last.TotalTokens != 3 || tokenUsage.Total.TotalTokens != 9 || tokenUsage.ModelContextWindow != 200 {
		t.Fatalf("token usage = %#v", tokenUsage)
	}
	flatTokenUsage := tokenUsageFromParams(map[string]any{"usage": map[string]any{"inputTokens": float64(4), "outputTokens": float64(5)}})
	if flatTokenUsage.Last.TotalTokens != 9 || flatTokenUsage.Total.TotalTokens != 9 {
		t.Fatalf("flat token usage = %#v", flatTokenUsage)
	}
	if firstInt64(map[string]any{"fallback": float64(4)}, "missing", "fallback") != 4 || firstInt64(map[string]any{}, "missing") != 0 {
		t.Fatal("firstInt64 fallback failed")
	}
}

func TestAppServerItemMappingHelpers(t *testing.T) {
	if completedItemEvent(Event{}, map[string]any{"item": map[string]any{"type": "agentMessage", "text": "done"}}).Kind != EventAgentMessageDelta {
		t.Fatal("agent completed item did not map to agent delta")
	}
	if event := completedItemEvent(Event{}, map[string]any{"item": map[string]any{"type": "agent_message", "content": []any{map[string]any{"text": "done"}}}}); event.Kind != EventAgentMessageDelta || event.Text != "done" {
		t.Fatalf("agent snake-case completed item = %#v", event)
	}
	if completedItemEvent(Event{}, map[string]any{"item": map[string]any{"type": "reasoning", "summary": "why"}}).Kind != EventReasoningDelta {
		t.Fatal("reasoning completed item did not map")
	}
	if completedItemEvent(Event{}, map[string]any{"item": map[string]any{"type": "userMessage", "text": "hi"}}).Kind != EventRaw {
		t.Fatal("completed user message item should stay raw")
	}
	if startedItemEvent(Event{}, map[string]any{"item": map[string]any{"type": "agentMessage"}}).Kind != EventRaw {
		t.Fatal("started agent message item should stay raw")
	}

	if !toolLikeItemType("commandExecution") || toolLikeItemType("agentMessage") {
		t.Fatal("tool-like item type classification failed")
	}

	tool := toolEventFromItem(map[string]any{"itemId": "outer", "item": map[string]any{"id": "inner", "type": "commandExecution", "title": "Run", "status": "done", "output": "ok", "locations": []any{map[string]any{"path": "/tmp/a"}}}}, "completed")
	if tool.ID != "inner" || tool.Kind != "commandExecution" || tool.Content != "ok" || len(tool.Locations) != 1 {
		t.Fatalf("tool event = %#v", tool)
	}
	if tool := toolEventFromItem(map[string]any{"item": map[string]any{"result": map[string]any{"content": []any{map[string]any{"text": "mcp ok"}}}}}, "completed"); tool.Content != "mcp ok" {
		t.Fatalf("nested tool content = %#v", tool)
	}
	if tool := toolEventFromItem(map[string]any{"item": map[string]any{"type": "mcpToolCall", "server": "remote", "tool": "echo"}}, "completed"); tool.Title != "remote echo" {
		t.Fatalf("MCP tool title = %#v", tool)
	}
	if title := toolTitleFromItem(map[string]any{"tool": "echo"}); title != "echo" {
		t.Fatalf("tool-only title = %q", title)
	}
	if toolEventFromItem(map[string]any{"itemId": "outer"}, "running").ID != "outer" {
		t.Fatal("tool event did not fall back to outer item id")
	}
	if locations := itemLocations(map[string]any{"files": []any{"a.go"}, "path": "b.go"}); len(locations) != 2 {
		t.Fatalf("locations = %#v", locations)
	}
	if locations := stringSliceValue([]string{"a.go"}); len(locations) != 1 || locations[0] != "a.go" {
		t.Fatalf("string locations = %#v", locations)
	}
}

func TestAppServerImageItemMapping(t *testing.T) {
	startedImage := eventFromRPC(rpcEvent{Method: notifyItemStarted, Params: mustRaw(map[string]any{
		"itemId": "outer",
		"item": map[string]any{
			"type":          itemTypeImageGeneration,
			"status":        "inProgress",
			"result":        "eA==",
			"savedPath":     "/secret/generated.png",
			"revisedPrompt": "draw it",
		},
	})})
	if startedImage.Kind != EventImageStarted || startedImage.Image.ID != "outer" ||
		startedImage.Image.SavedPath != "/secret/generated.png" ||
		strings.Contains(string(startedImage.RawParams), "eA==") ||
		strings.Contains(string(startedImage.RawParams), "/secret/") {
		t.Fatalf("started image item = %#v raw=%s", startedImage, startedImage.RawParams)
	}
	completedImage := eventFromRPC(rpcEvent{Method: notifyItemCompleted, Params: mustRaw(map[string]any{
		"item": map[string]any{
			"id":            "image",
			"type":          itemTypeImageView,
			"status":        "completed",
			"result":        "eA==",
			"path":          "/secret/view.png",
			"revisedPrompt": "view it",
		},
	})})
	if completedImage.Kind != EventImageCompleted || completedImage.Image.ID != "image" ||
		completedImage.Image.Kind != itemTypeImageView || completedImage.Image.RevisedPrompt != "view it" ||
		completedImage.Image.Raw["resultBytes"] == nil || completedImage.Image.Raw["path"] != "view.png" {
		t.Fatalf("completed image item = %#v", completedImage)
	}
	topLevelImage := imageEventFromItem(map[string]any{
		"itemId": "top",
		"type":   itemTypeImageGeneration,
		"result": 42,
		"path":   42,
	})
	if topLevelImage.ID != "top" {
		t.Fatalf("top-level image item = %#v", topLevelImage)
	}
	if raw := sanitizedImageEventParams(map[string]any{
		"type":       itemTypeImageGeneration,
		"result":     "eA==",
		"savedPath":  "/secret/top.png",
		"otherValue": "kept",
	}); strings.Contains(string(raw), "eA==") || strings.Contains(string(raw), "/secret/") {
		t.Fatalf("top-level image params were not sanitized: %s", raw)
	}
	safe := sanitizedImageItem(map[string]any{
		"type":       itemTypeImageGeneration,
		"result":     "",
		"savedPath":  "",
		"path":       42,
		"revised":    true,
		"otherValue": "kept",
	})
	if _, ok := safe["result"]; ok || safe["otherValue"] != "kept" {
		t.Fatalf("sanitized image item = %#v", safe)
	}
}

func TestAppServerValueMappingHelpers(t *testing.T) {
	if text := contentText([]any{"a", map[string]any{"content": []any{map[string]any{"summary_text": "b"}}}}); text != "ab" {
		t.Fatalf("content text = %q", text)
	}
	if text := contentText(map[string]any{"result": map[string]any{"text": "nested"}}); text != "nested" {
		t.Fatalf("result content text = %q", text)
	}
	if text := contentText(123); text != "" {
		t.Fatalf("default content text = %q", text)
	}
	if int64Value(map[string]any{"x": int64(4)}, "x") != 4 || int64Value(map[string]any{"x": 5}, "x") != 5 || int64Value(nil, "x") != 0 {
		t.Fatal("int64Value branches failed")
	}
	if firstNonEmptyMapSlice(map[string]any{"templates": []any{map[string]any{"name": "t"}}}, "resourceTemplates", "templates")[0]["name"] != "t" {
		t.Fatal("firstNonEmptyMapSlice fallback failed")
	}
	if mapValue(nil, "x") != nil {
		t.Fatal("mapValue nil failed")
	}
	if stringValue(map[string]any{"s": stringerForCodexTest("x")}, "s") != "x" {
		t.Fatal("stringValue did not use Stringer")
	}
	if timestampValue(map[string]any{"t": "now"}, "t") != "now" {
		t.Fatal("timestampValue string branch failed")
	}
	modifications, ok := permissionProfile([]string{"", "/repo"})["modifications"].([]map[string]any)
	if !ok || len(modifications) != 1 {
		t.Fatal("permissionProfile did not skip empty roots")
	}
	if _, err := threadsFromResponse(map[string]any{"threads": []any{map[string]any{"id": "missing-session-id"}}}); err == nil {
		t.Fatal("invalid thread list item was accepted")
	}
}

func TestAppServerEventMappingVariants(t *testing.T) {
	cases := []struct {
		method string
		params map[string]any
		kind   EventKind
	}{
		{"item/agentMessage/delta", map[string]any{"delta": "hi"}, EventAgentMessageDelta},
		{"item/reasoning/summaryTextDelta", map[string]any{"delta": "why"}, EventReasoningDelta},
		{"item/plan/delta", map[string]any{"delta": "step"}, EventPlanUpdated},
		{"item/started", map[string]any{"itemId": "tool", "type": "tool"}, EventToolStarted},
		{"item/started", map[string]any{"itemId": "message", "type": "agentMessage"}, EventRaw},
		{"item/completed", map[string]any{"itemId": "tool", "item": map[string]any{"type": "commandExecution", "result": "ok"}}, EventToolCompleted},
		{"item/completed", map[string]any{"itemId": "user", "item": map[string]any{"type": "userMessage", "text": "hi"}}, EventRaw},
		{"item/agentMessage/completed", map[string]any{"content": "done"}, EventAgentMessageDelta},
		{"item/reasoning/completed", map[string]any{"content": "done"}, EventReasoningDelta},
		{"enteredReviewMode", map[string]any{}, EventReasoningDelta},
		{"exitedReviewMode", map[string]any{"message": "done reviewing"}, EventReasoningDelta},
		{"command/exec/outputDelta", map[string]any{"itemId": "cmd", "text": "out"}, EventToolDelta},
		{"turn/diff/updated", map[string]any{"patch": "diff"}, EventDiffUpdated},
		{"turn/completed", map[string]any{"turn": map[string]any{"status": "interrupted"}}, EventCompleted},
		{"account/updated", map[string]any{"account": map[string]any{"chatgptAccountId": "acct", "email": "u@example.com", "chatgptPlanType": "plus"}}, EventAccountUpdated},
		{"warning", map[string]any{"message": "warn"}, EventWarning},
		{"error", map[string]any{"error": "boom"}, EventError},
		{"rawResponseItem/completed", map[string]any{}, EventRaw},
		{"unknown", map[string]any{}, EventRaw},
	}
	for _, tc := range cases {
		event := eventFromRPC(rpcEvent{Method: tc.method, Params: mustRaw(tc.params), Raw: "raw"})
		wantRaw := "raw"
		if tc.kind == EventError {
			wantRaw = ""
		}
		if event.Kind != tc.kind || event.RawMethod != tc.method || event.RawJSON != wantRaw {
			t.Fatalf("%s mapped to %#v", tc.method, event)
		}
	}
	if eventFromRPC(rpcEvent{Method: "error", Params: mustRaw(map[string]any{"message": "boom"})}).Err == nil {
		t.Fatal("error event did not include error")
	}
	if event := eventFromRPC(rpcEvent{Method: "error", Params: mustRaw(map[string]any{"error": ""})}); event.Text != appServerErrorText || len(event.RawParams) != 0 {
		t.Fatalf("empty error event text = %q", event.Text)
	}
	if event := eventFromRPC(rpcEvent{Method: "error"}); event.Text != appServerErrorText {
		t.Fatalf("missing error event text = %q", event.Text)
	}
	const secretErrorSentinel = "native-error-secret-sentinel"
	secretEvent := eventFromRPC(rpcEvent{
		Method: "error",
		Params: mustRaw(map[string]any{
			"message": secretErrorSentinel,
			"error":   map[string]any{"body": secretErrorSentinel},
		}),
		Raw: `{"method":"error","params":{"message":"` + secretErrorSentinel + `"}}`,
	})
	if !errors.Is(secretEvent.Err, ErrAppServerEvent) ||
		strings.Contains(secretEvent.Text, secretErrorSentinel) ||
		strings.Contains(secretEvent.Err.Error(), secretErrorSentinel) ||
		strings.Contains(string(secretEvent.RawParams), secretErrorSentinel) ||
		strings.Contains(secretEvent.RawJSON, secretErrorSentinel) {
		t.Fatalf("secret error event escaped fixed classification: %#v", secretEvent)
	}
	if account := eventFromRPC(rpcEvent{Method: "account/updated", Params: mustRaw(map[string]any{"email": "plain@example.com"})}).Account; account.Email != "plain@example.com" {
		t.Fatalf("plain account event = %#v", account)
	}
}

type stringerForCodexTest string

func (s stringerForCodexTest) String() string { return string(s) }

func TestRPCConnCallsNotificationsAndRequests(t *testing.T) {
	handlerCalled := make(chan ServerRequest, 1)
	transport := newScriptTransport()
	conn := newRPCConn(transport, func(_ context.Context, req ServerRequest) (any, error) {
		handlerCalled <- req

		return map[string]any{"ok": true}, nil
	})
	defer conn.Close()

	var result map[string]any
	if err := conn.Call(context.Background(), "custom", map[string]any{"x": true}, &result); err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if result["ok"] != true {
		t.Fatalf("result = %#v", result)
	}
	transport.recv <- rpcMessage{JSONRPC: jsonRPCVersion, Method: "note", Params: mustRaw(map[string]any{"a": 1})}
	event := <-conn.Events()
	if event.Method != "note" {
		t.Fatalf("event = %#v", event)
	}
	transport.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: json.RawMessage("99"), Method: "server/request", Params: mustRaw(map[string]any{"p": 1})}
	req := <-handlerCalled
	if req.Method != "server/request" {
		t.Fatalf("handler req = %#v", req)
	}
}

func TestAppServerClientRPCErrorBranches(t *testing.T) {
	client := &AppServerClient{rpc: newRPCConn(&failingSendTransport{done: make(chan struct{}), err: errors.New("send failed")}, nil)}
	defer client.Close(context.Background())
	ctx := context.Background()

	if err := client.initialize(ctx); err == nil {
		t.Fatal("initialize with RPC error succeeded")
	}
	if _, err := client.StartThread(ctx, ThreadStartRequest{}); err == nil {
		t.Fatal("StartThread with RPC error succeeded")
	}
	if _, err := client.ResumeThread(ctx, ThreadResumeRequest{}); err == nil {
		t.Fatal("ResumeThread with RPC error succeeded")
	}
	if _, err := client.ForkThread(ctx, ThreadForkRequest{}); err == nil {
		t.Fatal("ForkThread with RPC error succeeded")
	}
	if _, err := client.ListThreads(ctx, ThreadListRequest{}); err == nil {
		t.Fatal("ListThreads with RPC error succeeded")
	}
	if _, err := client.ReadThread(ctx, ThreadReadRequest{}); err == nil {
		t.Fatal("ReadThread with RPC error succeeded")
	}
	if _, err := client.ListTurns(ctx, ThreadTurnsListRequest{}); err == nil {
		t.Fatal("ListTurns with RPC error succeeded")
	}
	if _, err := client.RunTurn(ctx, TurnStartRequest{}); err == nil {
		t.Fatal("RunTurn with RPC error succeeded")
	}
	if _, err := client.CompactThread(ctx, ThreadCompactRequest{}); err == nil {
		t.Fatal("CompactThread with RPC error succeeded")
	}
	if _, err := client.StartReview(ctx, ReviewStartRequest{}); err == nil {
		t.Fatal("StartReview with RPC error succeeded")
	}
	if _, err := client.CollaborationModeList(ctx); err == nil {
		t.Fatal("CollaborationModeList with RPC error succeeded")
	}
	if _, err := client.MCPServerStatusList(ctx, "thread"); err == nil {
		t.Fatal("MCPServerStatusList with RPC error succeeded")
	}
	if err := client.UnsubscribeThread(ctx, "thread"); err == nil {
		t.Fatal("UnsubscribeThread with RPC error succeeded")
	}
	if err := client.UnsubscribeThread(ctx, ""); err != nil {
		t.Fatalf("UnsubscribeThread empty returned error: %v", err)
	}
	if err := client.DeleteThread(ctx, ThreadDeleteRequest{ThreadID: "thread"}); err == nil {
		t.Fatal("DeleteThread with RPC error succeeded")
	}
	if err := client.DeleteThread(ctx, ThreadDeleteRequest{}); err != nil {
		t.Fatalf("DeleteThread empty returned error: %v", err)
	}
	if _, err := client.ModelList(ctx); err == nil {
		t.Fatal("ModelList with RPC error succeeded")
	}
	if _, err := client.AccountRead(ctx); err == nil {
		t.Fatal("AccountRead with RPC error succeeded")
	}
}

func TestAppServerClientNormalizesMissingThreadRPCError(t *testing.T) {
	transport := newScriptTransport()
	transport.fail(methodThreadResume, "no rollout found for thread id thread-1")
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())

	_, err := client.ResumeThread(context.Background(), ThreadResumeRequest{ThreadID: "thread-1"})
	if !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("ResumeThread error = %v, want ErrThreadNotFound", err)
	}
}

func TestAppServerEventPumpAccountAndClose(t *testing.T) {
	updated := make(chan Event, 1)
	accountClient := &AppServerClient{
		options: Options{EventHandler: func(_ context.Context, event Event) { updated <- event }},
		rpc:     newRPCConn(newScriptTransport(), nil),
	}
	accountClient.ensureEventPump()
	accountClient.dispatchEvent(Event{Kind: EventAccountUpdated, Account: Account{ID: "acct"}})
	if event := <-updated; event.Account.ID != "acct" {
		t.Fatalf("account update event = %#v", event)
	}
	accountClient.setAccount(Account{})
	_ = accountClient.Close(context.Background())

	closedTransport := newScriptTransport()
	closedClient := &AppServerClient{rpc: newRPCConn(closedTransport, nil)}
	if err := closedClient.Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := closedClient.registerThread("thread-1"); err == nil {
		t.Fatal("registerThread accepted closed client")
	}
}

type scriptTransport struct {
	mu      sync.Mutex
	sent    []rpcMessage
	recv    chan rpcMessage
	closed  bool
	errs    map[string]*rpcError
	threads int
}

type abruptCloseTransport struct {
	mu     sync.Mutex
	sent   []rpcMessage
	recv   chan rpcMessage
	done   chan struct{}
	err    error
	closed bool
}

type failingSendTransport struct {
	done chan struct{}
	err  error
}

type hookedResponseTransport struct {
	*responseTransport
	hook func()
}

func (t *hookedResponseTransport) Send(ctx context.Context, msg rpcMessage) error {
	if t.hook != nil {
		t.hook()
	}

	return t.responseTransport.Send(ctx, msg)
}

func (t *failingSendTransport) Send(context.Context, rpcMessage) error { return t.err }

func (t *failingSendTransport) Recv() (rpcMessage, string, error) {
	<-t.done

	return rpcMessage{}, "", errors.New("closed")
}

func (t *failingSendTransport) Close() error {
	select {
	case <-t.done:
	default:
		close(t.done)
	}

	return nil
}

func newScriptTransport() *scriptTransport {
	return &scriptTransport{recv: make(chan rpcMessage, 64)}
}

func newAbruptCloseTransport() *abruptCloseTransport {
	return &abruptCloseTransport{
		recv: make(chan rpcMessage, 1),
		done: make(chan struct{}),
	}
}

func (t *abruptCloseTransport) Send(_ context.Context, msg rpcMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("closed")
	}
	t.sent = append(t.sent, msg)
	if msg.Method == methodTurnStart {
		t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Result: mustRaw(map[string]any{"turn": map[string]any{"id": "turn-1"}})}
	}

	return nil
}

func (t *abruptCloseTransport) Recv() (rpcMessage, string, error) {
	select {
	case msg := <-t.recv:
		return msg, string(mustRaw(msg)), nil
	case <-t.done:
		t.mu.Lock()
		err := t.err
		t.mu.Unlock()
		if err == nil {
			err = errors.New("closed")
		}

		return rpcMessage{}, "", err
	}
}

func (t *abruptCloseTransport) Close() error {
	t.CloseWithError(errors.New("closed"))

	return nil
}

func (t *abruptCloseTransport) CloseWithError(err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()

		return
	}
	t.closed = true
	t.err = err
	close(t.done)
	t.mu.Unlock()
}

func (t *scriptTransport) Send(_ context.Context, msg rpcMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("closed")
	}
	t.sent = append(t.sent, msg)

	if len(msg.ID) > 0 {
		if errResp := t.errs[msg.Method]; errResp != nil {
			t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Error: errResp}

			return nil
		}
		response := t.response(msg.Method)
		switch msg.Method {
		case methodThreadStart, methodThreadFork:
			t.threads++
			response = scriptThreadResponse(fmt.Sprintf("thread-%d", t.threads))
		case methodThreadResume:
			var params map[string]any
			_ = json.Unmarshal(msg.Params, &params)
			response = scriptThreadResponse(stringValue(params, fieldThreadID))
		}
		t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: msg.ID, Result: mustRaw(response)}
		if msg.Method == methodTurnStart {
			t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, Method: "turn/plan/updated", Params: mustRaw(map[string]any{"threadId": "thread-1", "turnId": "turn-1", "items": []any{map[string]any{"text": "plan", "status": "running"}}})}
			t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, Method: "item/reasoning/textDelta", Params: mustRaw(map[string]any{"threadId": "thread-1", "turnId": "turn-1", "delta": "why"})}
			t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, Method: "item/completed", Params: mustRaw(map[string]any{"threadId": "thread-1", "turnId": "turn-1", "item": map[string]any{"type": "agentMessage", "text": "hi"}})}
			t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, Method: "item/fileChange/patchUpdated", Params: mustRaw(map[string]any{"threadId": "thread-1", "turnId": "turn-1", "diff": "diff"})}
			t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, Method: "thread/tokenUsage/updated", Params: mustRaw(map[string]any{"threadId": "thread-1", "turnId": "turn-1", "tokenUsage": map[string]any{
				"last":               map[string]any{"inputTokens": 1, "cachedInputTokens": 1, "outputTokens": 2, "reasoningOutputTokens": 1},
				"total":              map[string]any{"inputTokens": 6, "outputTokens": 3, "totalTokens": 9},
				"modelContextWindow": 100,
			}})}
			t.recv <- rpcMessage{JSONRPC: jsonRPCVersion, Method: "turn/completed", Params: mustRaw(map[string]any{"threadId": "thread-1", "turnId": "turn-1", "turn": map[string]any{"status": "completed"}})}
		}
	}

	return nil
}

func (t *scriptTransport) Recv() (rpcMessage, string, error) {
	msg, ok := <-t.recv
	if !ok {
		return rpcMessage{}, "", errors.New("closed")
	}
	raw := string(mustRaw(msg))

	return msg, raw, nil
}

func (t *scriptTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		close(t.recv)
	}

	return nil
}

func (t *scriptTransport) sentParams(method string) map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.sent) - 1; i >= 0; i-- {
		msg := t.sent[i]
		if msg.Method != method {
			continue
		}
		var params map[string]any
		_ = json.Unmarshal(msg.Params, &params)

		return params
	}

	return nil
}

func (t *scriptTransport) fail(method string, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.errs == nil {
		t.errs = make(map[string]*rpcError)
	}
	t.errs[method] = &rpcError{Code: -32000, Message: message}
}

func (t *scriptTransport) response(method string) any {
	switch method {
	case methodThreadStart, methodThreadResume, methodThreadFork:
		return scriptThreadResponse("thread-1")
	case methodThreadList:
		return map[string]any{"data": []any{map[string]any{"id": "thread-1", "sessionId": "thread-1"}}}
	case methodThreadRead:
		items := []any{
			map[string]any{"id": "item-1"},
			map[string]any{
				"id":        "image-1",
				"type":      itemTypeImageGeneration,
				"status":    "completed",
				"result":    "eA==",
				"savedPath": "/secret/generated.png",
			},
			map[string]any{"id": "message-1", "type": itemTypeAgentMessage, "text": "done"},
		}

		return map[string]any{
			"thread":   map[string]any{"id": "thread-1", "sessionId": "thread-1"},
			"items":    items,
			"messages": items,
		}
	case methodThreadTurnsList:
		return map[string]any{"data": []any{map[string]any{"id": "turn-1"}}, "nextCursor": "next"}
	case methodTurnStart:
		return map[string]any{"turn": map[string]any{"id": "turn-1"}}
	case methodTurnSteer, methodTurnInterrupt, methodThreadDelete, methodThreadUnsubscribe, methodAccountLoginStart, methodAccountLogout:
		return map[string]any{}
	case methodThreadCompact:
		return map[string]any{"status": "compacted"}
	case methodReviewStart:
		return map[string]any{"status": "reviewing"}
	case methodCollaborationList:
		return map[string]any{"data": []any{map[string]any{"id": "default", "name": "Default"}, map[string]any{"mode": "plan"}}}
	case methodMCPStatusList:
		return map[string]any{"servers": []any{map[string]any{"name": "mcp", "status": "ready", "tools": []any{map[string]any{"name": "echo"}}}}}
	case methodModelList:
		return map[string]any{"data": []any{
			map[string]any{
				"id":                     "gpt-a",
				"displayName":            "GPT A",
				"description":            "A model",
				"defaultReasoningEffort": "medium",
				"inputModalities":        []any{"text", "image"},
				"supportedReasoningEfforts": []any{
					map[string]any{"description": "ignored"},
					map[string]any{"reasoningEffort": "low", "description": "fast"},
					map[string]any{"reasoningEffort": "medium", "description": "balanced"},
				},
			},
			map[string]any{"model": "gpt-b"},
		}}
	case methodAccountRead:
		return map[string]any{"account": map[string]any{"chatgptAccountId": "acct", "email": "u@example.com", "chatgptPlanType": "plus"}}
	case methodAccountRateLimitsRead:
		return map[string]any{"rateLimits": map[string]any{
			"planType":  "pro",
			"primary":   map[string]any{"usedPercent": float64(12), "windowDurationMins": float64(300), "resetsAt": float64(1_000_000)},
			"secondary": map[string]any{"usedPercent": float64(80)},
		}}
	default:
		return map[string]any{"ok": true}
	}
}

func scriptThreadResponse(threadID string) map[string]any {
	return map[string]any{
		"thread": map[string]any{
			"id": threadID, "sessionId": "session-1", "path": "/tmp/rollout.jsonl",
			"cwd": "/repo", "model": "gpt-a", "modelProvider": "openai", "updatedAt": float64(10),
		},
		"reasoningEffort": "high",
	}
}

func mustRaw(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}

	return raw
}

func TestAppServerLifecycleMappingEdges(t *testing.T) {
	if _, err := NewAppServerClient(context.Background(), Options{CLIPath: filepath.Join(t.TempDir(), "missing"), NativeVersion: minCodexVersion}); err == nil {
		t.Fatal("NewAppServerClient accepted missing CLI")
	}

	initErrorScript := filepath.Join(testTraversableTempDir(t), "codex-init-error")
	if err := os.WriteFile(initErrorScript, []byte(`#!/bin/sh
if [ "$1" = "--version" ]; then
  echo codex-cli 0.144.1
  exit 0
fi
read line || exit 0
echo '{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"init failed"}}'
while read line; do :; done
`), nativeScriptMode); err != nil {
		t.Fatalf("write init error script: %v", err)
	}
	if _, err := NewAppServerClient(context.Background(), Options{
		CLIPath: initErrorScript, CodexHome: t.TempDir(), SupervisorRoot: testTraversableTempDir(t),
		SupervisorParent: os.TempDir(),
		NativeVersion:    minCodexVersion, LaunchTimeout: time.Second,
	}); err == nil {
		t.Fatal("NewAppServerClient accepted initialize failure")
	}

	transport := newScriptTransport()
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	defer client.Close(context.Background())
	ephemeral := true
	if _, err := client.StartThread(context.Background(), ThreadStartRequest{
		Cwd:       "/repo",
		Ephemeral: &ephemeral,
		Config:    map[string]any{"sandbox_mode": "workspace-write"},
	}); err != nil {
		t.Fatalf("StartThread returned error: %v", err)
	}
	params := transport.sentParams(methodThreadStart)
	if params["ephemeral"] != true || params["config"] == nil {
		t.Fatalf("thread/start params = %#v", params)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}

	malformedResume := &AppServerClient{rpc: newRPCConn(&responseTransport{responses: map[string]any{
		methodThreadResume: map[string]any{},
	}}, nil)}
	if _, err := malformedResume.ResumeThread(context.Background(), ThreadResumeRequest{ThreadID: "requested"}); err == nil {
		t.Fatal("resume accepted a response without the returned thread identity")
	}
	_ = malformedResume.Close(context.Background())

	mapping := &AppServerClient{rpc: newRPCConn(&responseTransport{responses: map[string]any{
		methodCollaborationList: map[string]any{"data": []any{map[string]any{}, map[string]any{"name": "named"}}},
		methodModelList:         map[string]any{"data": []any{map[string]any{}, map[string]any{"name": "named-model"}}},
		methodAccountRead:       map[string]any{"email": "user@example.com"},
	}}, nil)}
	modes, err := mapping.CollaborationModeList(context.Background())
	if err != nil || len(modes.Modes) != 1 || modes.Modes[0].ID != "named" {
		t.Fatalf("modes=%#v err=%v", modes, err)
	}
	models, err := mapping.ModelList(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "named-model" {
		t.Fatalf("models=%#v err=%v", models, err)
	}
	account, err := mapping.AccountRead(context.Background())
	if err != nil || account.Email != "user@example.com" {
		t.Fatalf("account=%#v err=%v", account, err)
	}
	_ = mapping.Close(context.Background())

	if completedItemEvent(Event{}, map[string]any{"itemId": "outer"}).Kind != EventRaw {
		t.Fatal("completed item without a tool-like type should stay raw")
	}
}

func TestAppServerThreadCallsFailClosedAtEveryAcceptanceBoundary(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	transport := &responseTransport{responses: map[string]any{}}
	client := &AppServerClient{rpc: newRPCConn(transport, nil)}
	if _, err := client.StartThread(cancelled, ThreadStartRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled StartThread error = %v", err)
	}
	if _, err := client.ResumeThread(cancelled, ThreadResumeRequest{ThreadID: "thread"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ResumeThread error = %v", err)
	}
	if _, err := client.ForkThread(cancelled, ThreadForkRequest{ThreadID: "thread"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ForkThread error = %v", err)
	}
	_ = client.Close(context.Background())

	closedTransport := &responseTransport{responses: map[string]any{}}
	closed := &AppServerClient{rpc: newRPCConn(closedTransport, nil), closed: true}
	if _, err := closed.StartThread(context.Background(), ThreadStartRequest{}); err == nil {
		t.Fatal("closed StartThread succeeded")
	}
	if _, err := closed.ForkThread(context.Background(), ThreadForkRequest{ThreadID: "thread"}); err == nil {
		t.Fatal("closed ForkThread succeeded")
	}
	_ = closedTransport.Close()
	_ = closed.rpc.Close()

	for method, call := range map[string]func(*AppServerClient) error{
		methodThreadStart: func(client *AppServerClient) error {
			_, err := client.StartThread(context.Background(), ThreadStartRequest{})

			return err
		},
		methodThreadFork: func(client *AppServerClient) error {
			_, err := client.ForkThread(context.Background(), ThreadForkRequest{ThreadID: "source"})

			return err
		},
		methodThreadRead: func(client *AppServerClient) error {
			_, err := client.ReadThread(context.Background(), ThreadReadRequest{ThreadID: "thread"})

			return err
		},
	} {
		t.Run(method, func(t *testing.T) {
			transport := &responseTransport{responses: map[string]any{method: map[string]any{}}}
			client := &AppServerClient{rpc: newRPCConn(transport, nil)}
			if err := call(client); err == nil {
				t.Fatal("malformed response succeeded")
			}
			_ = client.Close(context.Background())
		})
	}

	resumeTransport := &responseTransport{responses: map[string]any{
		methodThreadResume: scriptThreadResponse("different"),
	}}
	resume := &AppServerClient{rpc: newRPCConn(resumeTransport, nil)}
	if _, err := resume.ResumeThread(context.Background(), ThreadResumeRequest{ThreadID: "requested"}); err == nil {
		t.Fatal("ResumeThread accepted a different returned identity")
	}
	_ = resume.Close(context.Background())

	sendErr := errors.New("send failed")
	failing := &AppServerClient{rpc: newRPCConn(&failingSendTransport{done: make(chan struct{}), err: sendErr}, nil)}
	if _, err := failing.ForkThread(context.Background(), ThreadForkRequest{ThreadID: "source"}); !errors.Is(err, sendErr) {
		t.Fatalf("ForkThread send error = %v", err)
	}
	_ = failing.Close(context.Background())
}

func TestAppServerTurnAndSubscriptionHonorCallAndContextFailure(t *testing.T) {
	sendErr := errors.New("send failed")
	transport := &failingSendTransport{done: make(chan struct{}), err: sendErr}
	client := &AppServerClient{rpc: newRPCConn(transport, nil), threads: map[string]*threadStream{}}
	stream := newThreadStream("thread")
	stream.claimed = true
	client.threads["thread"] = stream
	if _, err := client.RunTurn(context.Background(), TurnStartRequest{ThreadID: "thread"}); !errors.Is(err, sendErr) {
		t.Fatalf("RunTurn send error = %v", err)
	}
	_ = client.Close(context.Background())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&AppServerClient{}).SubscribeThread(cancelled, "thread"); !errors.Is(err, context.Canceled) {
		t.Fatalf("SubscribeThread canceled error = %v", err)
	}

	client = &AppServerClient{threads: map[string]*threadStream{"thread": newThreadStream("thread")}}
	watch, cancelWatch := context.WithCancel(context.Background())
	feed, err := client.SubscribeThread(watch, "thread")
	if err != nil {
		t.Fatal(err)
	}
	cancelWatch()
	select {
	case <-client.eventPumpDone():
	default:
	}
	select {
	case _, open := <-feed.Events:
		if open {
			t.Fatal("canceled subscription remained open")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled subscription did not release")
	}
}

func TestThreadAcceptanceReturnsRoutingFailureAfterValidNativeResponse(t *testing.T) {
	for method, call := range map[string]func(*AppServerClient) error{
		methodThreadStart: func(client *AppServerClient) error {
			_, err := client.StartThread(context.Background(), ThreadStartRequest{})

			return err
		},
		methodThreadResume: func(client *AppServerClient) error {
			_, err := client.ResumeThread(context.Background(), ThreadResumeRequest{ThreadID: "thread"})

			return err
		},
		methodThreadFork: func(client *AppServerClient) error {
			_, err := client.ForkThread(context.Background(), ThreadForkRequest{ThreadID: "source"})

			return err
		},
	} {
		t.Run(method, func(t *testing.T) {
			base := &responseTransport{responses: map[string]any{method: scriptThreadResponse("thread")}}
			client := &AppServerClient{}
			transport := &hookedResponseTransport{responseTransport: base, hook: func() {
				client.mu.Lock()
				client.routingFailure = errors.New("routing failed during acknowledgement")
				client.mu.Unlock()
			}}
			client.rpc = newRPCConn(transport, nil)
			if err := call(client); err == nil {
				t.Fatal("thread acceptance ignored routing failure")
			}
			_ = client.Close(context.Background())
		})
	}
}

const (
	envRunIntegration       = "ACP_GO_CODEX_RUN_INTEGRATION"
	envIntegrationCodexPath = "ACP_GO_CODEX_HARNESS_PATH"
	envLiveTurn             = "ACP_GO_CODEX_RUN_LIVE_TOKENS"
)

func TestIntegrationAppServerSmoke(t *testing.T) {
	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run live Codex app-server integration tests", envRunIntegration)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	codexPath := os.Getenv(envIntegrationCodexPath)
	if codexPath == "" {
		codexPath = "codex"
	}
	client, err := NewAppServerClient(ctx, Options{
		CLIPath:          codexPath,
		CodexHome:        t.TempDir(),
		SupervisorRoot:   t.TempDir(),
		DarwinBestEffort: true,
		NativeVersion:    minCodexVersion,
	})
	if err != nil {
		t.Fatalf("NewAppServerClient returned error: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(context.Background()); closeErr != nil {
			t.Fatalf("Close returned error: %v", closeErr)
		}
	})

	models, err := client.ModelList(ctx)
	if err != nil {
		t.Fatalf("ModelList returned error: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("ModelList returned no models")
	}
	if _, collabErr := client.CollaborationModeList(ctx); collabErr != nil {
		t.Fatalf("CollaborationModeList returned error: %v", collabErr)
	}
	thread, err := client.StartThread(ctx, ThreadStartRequest{
		Cwd:   t.TempDir(),
		Model: models[0].ID,
	})
	if err != nil {
		t.Fatalf("StartThread returned error: %v", err)
	}
	if thread.ID == "" {
		t.Fatalf("thread missing ID: %#v", thread)
	}
	if _, mcpErr := client.MCPServerStatusList(ctx, thread.ID); mcpErr != nil {
		t.Fatalf("MCPServerStatusList returned error: %v", mcpErr)
	}
	terminals, terminalsErr := client.ListBackgroundTerminals(ctx, BackgroundTerminalListRequest{ThreadID: thread.ID})
	if terminalsErr != nil {
		t.Fatalf("ListBackgroundTerminals returned error: %v", terminalsErr)
	}
	if len(terminals.Terminals) != 0 {
		t.Fatalf("new thread unexpectedly has background terminals: %#v", terminals.Terminals)
	}
	t.Cleanup(func() {
		if unsubErr := client.UnsubscribeThread(context.Background(), thread.ID); unsubErr != nil {
			t.Fatalf("UnsubscribeThread returned error: %v", unsubErr)
		}
	})

	if _, readErr := client.ReadThread(ctx, ThreadReadRequest{ThreadID: thread.ID}); readErr != nil {
		t.Fatalf("ReadThread returned error: %v", readErr)
	}
	if os.Getenv(envLiveTurn) == "1" {
		runLiveTurn(ctx, t, client, thread.ID)
		if _, listErr := client.ListTurns(ctx, ThreadTurnsListRequest{ThreadID: thread.ID, Limit: 10}); listErr != nil {
			t.Fatalf("ListTurns after live turn returned error: %v", listErr)
		}
	} else if _, listErr := client.ListTurns(ctx, ThreadTurnsListRequest{ThreadID: thread.ID, Limit: 10}); listErr != nil && !strings.Contains(listErr.Error(), "not materialized yet") {
		t.Fatalf("ListTurns before materialization returned unexpected error: %v", listErr)
	}
}

func runLiveTurn(ctx context.Context, t *testing.T, client Client, threadID string) {
	t.Helper()
	feed, err := client.SubscribeThread(ctx, threadID)
	if err != nil {
		t.Fatalf("SubscribeThread returned error: %v", err)
	}
	defer feed.Release()

	_, err = client.RunTurn(ctx, TurnStartRequest{
		ThreadID: threadID,
		Prompt: []UserInput{{
			"type": "text",
			"text": "Reply with exactly CODEX_INTEGRATION_OK and do not use tools.",
		}},
	})
	if err != nil {
		t.Fatalf("RunTurn returned error: %v", err)
	}

	for {
		event := <-feed.Events
		if event.Kind == EventError {
			t.Fatalf("live turn event error: %v", event.Err)
		}
		if event.Kind == EventCompleted {
			return
		}
	}
}

func TestDispatchEventForwardsRuntimeErrors(t *testing.T) {
	called := false
	client := &AppServerClient{options: Options{EventHandler: func(_ context.Context, event Event) {
		called = event.Kind == EventError
	}}}
	client.dispatchEvent(Event{Kind: EventError, Err: errors.New("runtime failed")})
	if !called {
		t.Fatal("runtime error was not forwarded to the event handler")
	}
}

func TestAppServerCloseOwnedCancellationJoinsAllConcurrentCallers(t *testing.T) {
	transport := &manualTransport{recv: make(chan rpcMessage, 2)}
	handlerStarted := make(chan struct{})
	handlerExited := make(chan struct{})
	rpc := newRPCConn(transport, func(ctx context.Context, _ ServerRequest) (any, error) {
		close(handlerStarted)
		<-ctx.Done()
		close(handlerExited)

		return nil, ctx.Err()
	})
	var cancelOnce sync.Once
	processCancelled := make(chan struct{})
	client := &AppServerClient{
		rpc: rpc,
		procCancel: func() {
			cancelOnce.Do(func() { close(processCancelled) })
		},
	}
	client.ensureEventPump()
	transport.recv <- rpcMessage{JSONRPC: jsonRPCVersion, ID: json.RawMessage("1"), Method: RequestCommandApproval, Params: json.RawMessage(`{}`)}
	<-handlerStarted

	const callers = 32
	results := make(chan error, callers)
	for range callers {
		go func() { results <- client.Close(t.Context()) }()
	}

	<-processCancelled
	<-handlerExited
	for range callers {
		require.NoError(t, <-results)
	}
}
