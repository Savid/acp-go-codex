package codexacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/stretchr/testify/require"
)

type imagePromptClient struct {
	*runEventsClient
	models    []codex.Model
	runCalls  int
	lastStart codex.TurnStartRequest
}

func (c *imagePromptClient) ModelList(context.Context) ([]codex.Model, error) {
	return append([]codex.Model(nil), c.models...), nil
}

func (c *imagePromptClient) RunTurn(ctx context.Context, req codex.TurnStartRequest) (codex.Turn, error) {
	c.runCalls++
	c.lastStart = req

	return c.runEventsClient.RunTurn(ctx, req)
}

func TestPromptImageModelGatePreparationAndMirrorFailures(t *testing.T) {
	ctx := context.Background()
	png, err := os.ReadFile(filepath.Join("testdata", "valid.png"))
	if err != nil {
		t.Fatal(err)
	}
	request := TextPromptRequest("image", "turn", "describe it")
	request.Prompt = append(request.Prompt, acp.ImageBlock(base64.StdEncoding.EncodeToString(png), "image/png"))

	agent := NewAgent(WithImageLimits(ImageLimits{}))
	agent.setAgentClient(newRecordingAgentClient())
	client := &imagePromptClient{
		runEventsClient: &runEventsClient{events: []codex.Event{{Kind: codex.EventCompleted}}},
		models: []codex.Model{
			{ID: "vision", InputModalities: []string{"text", "image"}},
			{ID: "text", InputModalities: []string{"text"}},
		},
	}
	promptSession := &session{
		agent:         agent,
		id:            "image",
		cwd:           t.TempDir(),
		codexThreadID: "thread",
		model:         "vision",
		client:        client,
	}
	resp, err := promptSession.Prompt(ctx, request)
	if err != nil || resp.StopReason != acp.StopReasonEndTurn || client.runCalls != 1 {
		t.Fatalf("supported image prompt resp=%#v calls=%d err=%v", resp, client.runCalls, err)
	}
	if len(client.lastStart.Prompt) != 2 {
		t.Fatalf("native image prompt = %#v", client.lastStart.Prompt)
	}
	imageURL, ok := client.lastStart.Prompt[1]["url"].(string)
	if client.lastStart.Prompt[1]["type"] != "image" || !ok ||
		!strings.HasPrefix(imageURL, "data:image/png;base64,") {
		t.Fatalf("native image prompt = %#v", client.lastStart.Prompt)
	}

	promptSession.mu.Lock()
	promptSession.model = "text"
	promptSession.mu.Unlock()
	_, promptErr := promptSession.Prompt(ctx, request)
	if promptErr == nil || client.runCalls != 1 {
		t.Fatalf("text-only model prompt calls=%d err=%v", client.runCalls, promptErr)
	}

	promptSession.mu.Lock()
	promptSession.model = "unlisted"
	promptSession.mu.Unlock()
	_, promptErr = promptSession.Prompt(ctx, request)
	if promptErr != nil || client.runCalls != 2 {
		t.Fatalf("unknown model image prompt calls=%d err=%v", client.runCalls, promptErr)
	}

	largePNG := append(append([]byte(nil), png...), make([]byte, codexInlineImageEnvelopeSize)...)
	largeRequest := TextPromptRequest("image", "large", "describe it")
	largeRequest.Prompt = append(largeRequest.Prompt, acp.ImageBlock(base64.StdEncoding.EncodeToString(largePNG), "image/png"))
	originalCreate := createPromptImageTempDir
	createPromptImageTempDir = func(string, string) (string, error) {
		return "", errors.New("scratch unavailable")
	}
	_, err = promptSession.Prompt(ctx, largeRequest)
	createPromptImageTempDir = originalCreate
	if !isTurnFailure(err, codex.CauseTransport) {
		t.Fatalf("scratch preparation err=%v", err)
	}

	// The client is told the staging failed and nothing about where the adapter
	// stages, while the real cause stays in the chain for the adapter to inspect.
	var scratchErr *acp.RequestError
	if !errors.As(err, &scratchErr) {
		t.Fatalf("scratch preparation err=%v", err)
	}

	scratchData, _ := scratchErr.Data.(map[string]any)
	if scratchData[jsonFieldMessage] != promptImageScratchFailure {
		t.Fatalf("scratch message=%v", scratchData[jsonFieldMessage])
	}

	if strings.Contains(scratchErr.Error(), "scratch unavailable") {
		t.Fatalf("client-visible text carries the underlying failure: %s", scratchErr.Error())
	}

	rolloutDir := t.TempDir()

	writeRollout := func(name, result string) string {
		path := filepath.Join(rolloutDir, name)
		if err := os.WriteFile(path, []byte(
			`{"type":"response_item","payload":{"type":"image_generation_call","id":"img","status":"completed","result":"`+result+`"}}`+"\n",
		), 0o600); err != nil {
			t.Fatal(err)
		}

		return path
	}

	rollout := writeRollout("refused.jsonl", "!")

	newMirrorSession := func(store SessionStore) *session {
		mirrorAgent := NewAgent(WithSessionStore(store), WithImageLimits(ImageLimits{}))
		mirrorAgent.setAgentClient(newRecordingAgentClient())

		return &session{
			agent:         mirrorAgent,
			id:            "mirror",
			cwd:           t.TempDir(),
			codexThreadID: "thread",
			rolloutPath:   rollout,
			client:        &runEventsClient{events: []codex.Event{{Kind: codex.EventCompleted}}},
		}
	}

	// An image the mirror cannot materialize is a recoverable verdict: the
	// durable rollout drops the bytes and the turn still returns its answer.
	recoverable, mirrorErr := newMirrorSession(NewInMemorySessionStore()).Prompt(ctx, TextPromptRequest("mirror", "turn", "draw"))
	if mirrorErr != nil {
		t.Fatalf("recoverable image mirror verdict ended the turn: %v", mirrorErr)
	}

	if recoverable.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("mirror stop reason=%v", recoverable.StopReason)
	}

	// The adapter's own store breaking is not something the model can act on,
	// so it stays turn-fatal.
	storageSession := newMirrorSession(imageAppendErrorStore{SessionStore: NewInMemorySessionStore()})
	storageSession.rolloutPath = writeRollout("stored.jsonl", base64.StdEncoding.EncodeToString(png))

	storageErr := func() error {
		_, err := storageSession.Prompt(ctx, TextPromptRequest("mirror", "turn", "draw"))

		return err
	}()
	if !isTurnFailure(storageErr, codex.CauseTransport) {
		t.Fatalf("durable image storage failure err=%v", storageErr)
	}
}

// imageAppendErrorStore breaks only the image-artifact append, so a turn can
// reach the durable image mirror before the store fails under it.
type imageAppendErrorStore struct {
	SessionStore
}

func (s imageAppendErrorStore) Append(ctx context.Context, key SessionKey, entries []SessionStoreEntry) error {
	if strings.HasPrefix(key.Subpath, imageArtifactStorePrefix) {
		return errors.New("append failed")
	}

	return s.SessionStore.Append(ctx, key, entries)
}

func TestEmitPromptImageUpdates(t *testing.T) {
	ctx := context.Background()
	png, err := os.ReadFile(filepath.Join("testdata", "valid.png"))
	if err != nil {
		t.Fatal(err)
	}
	event := codex.Event{
		Kind: codex.EventImageCompleted,
		Image: codex.ImageEvent{
			ID:     "image",
			Status: "completed",
			Result: base64.StdEncoding.EncodeToString(png),
		},
	}

	agent := NewAgent(WithImageLimits(ImageLimits{}))
	agent.setAgentClient(newRecordingAgentClient())
	s := &session{agent: agent, id: "image"}
	state := &promptEventState{imageTools: newImageToolState()}
	if err := s.emitPromptUpdates(ctx, event, event, state); err != nil {
		t.Fatalf("emit image update: %v", err)
	}

	// A refused image output fails its own tool call, hands the model the
	// guidance it can act on, and leaves the turn running with its context.
	client := newRecordingAgentClient()
	agent.setAgentClient(client)

	invalid := codex.Event{
		Kind:  codex.EventImageCompleted,
		Image: codex.ImageEvent{ID: "invalid", Status: "completed", Result: "!"},
	}
	state = &promptEventState{imageTools: newImageToolState()}
	if err := s.emitPromptUpdates(ctx, invalid, invalid, state); err != nil {
		t.Fatalf("refused image output ended the turn: %v", err)
	}

	refusal := client.updates[len(client.updates)-1].Update.ToolCallUpdate
	if refusal == nil || refusal.Status == nil || *refusal.Status != acp.ToolCallStatusFailed {
		t.Fatalf("refused image output tool call update=%+v", refusal)
	}

	if len(refusal.Content) != 1 || refusal.Content[0].Content == nil ||
		refusal.Content[0].Content.Content.Text == nil ||
		refusal.Content[0].Content.Content.Text.Text != imageGuidanceInvalidBase64 {
		t.Fatalf("refused image output content=%+v", refusal.Content)
	}

	// The turn keeps making progress after the refusal.
	if err := s.emitPromptUpdates(ctx, event, event, &promptEventState{imageTools: newImageToolState()}); err != nil {
		t.Fatalf("turn did not continue after a refusal: %v", err)
	}

	// A storage failure is the adapter's own durability breaking: the tool call
	// still reports failed, and the turn ends.
	storageAgent := NewAgent(WithSessionStore(appendErrorStore{}), WithImageLimits(ImageLimits{}))
	storageAgent.setAgentClient(newRecordingAgentClient())
	storage := &session{agent: storageAgent, id: "storage"}

	state = &promptEventState{imageTools: newImageToolState()}
	if err := storage.emitPromptUpdates(ctx, event, event, state); !isTurnFailure(err, codex.CauseTransport) {
		t.Fatalf("image storage failure err=%v", err)
	}

	storageAgent.setAgentClient(&errorAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		updateErr:            errors.New("update failed"),
	})
	state = &promptEventState{imageTools: newImageToolState()}
	if err := storage.emitPromptUpdates(ctx, event, event, state); err == nil || err.Error() != "update failed" {
		t.Fatalf("image failure update err=%v", err)
	}

	agent.setAgentClient(&errorAgentClient{
		recordingAgentClient: newRecordingAgentClient(),
		updateErr:            errors.New("update failed"),
	})
	state = &promptEventState{imageTools: newImageToolState()}
	if err := s.emitPromptUpdates(ctx, invalid, invalid, state); err == nil || err.Error() != "update failed" {
		t.Fatalf("refusal update err=%v", err)
	}
}

func TestPromptRolloutRawAndPermissionEdges(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	promptSession := &session{agent: agent, id: "s", cwd: "/tmp/project", codexThreadID: "thread", client: &runEventsClient{}}
	held := promptSession.turnQueue()
	held <- struct{}{}
	// The slot was never taken, so no turn opened and there is no terminal to
	// report: the caller's own context error is the answer, not a response.
	if resp, err := promptSession.Prompt(canceledContext(), TextPromptRequest("s", "test-turn", "hi")); !errors.Is(err, context.Canceled) ||
		resp.StopReason != "" {
		t.Fatalf("canceled acquire resp=%#v err=%v", resp, err)
	}
	<-held

	promptSession.client = &runEventsClient{runErr: errors.New("not logged in")}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt accepted RunTurn error")
	}

	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")})
	promptSession.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventAgentMessageDelta, ThreadID: "thread", TurnID: "turn", Text: "hi"}}}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt ignored update error")
	}
	promptSession.client = &runEventsClient{events: []codex.Event{{
		Kind:     codex.EventUsageUpdated,
		ThreadID: "thread",
		TurnID:   "turn",
		TokenUsage: codex.TokenUsage{
			Last: codex.Usage{InputTokens: 1, OutputTokens: 2},
		},
	}}}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt ignored usage update error")
	}

	agent.setAgentClient(newRecordingAgentClient())
	promptSession.rawMessages = rawMessageConfig{enabled: true}
	promptSession.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventRaw, ThreadID: "thread", TurnID: "turn", RawMethod: "raw", RawParams: json.RawMessage(`{"type":"event_msg"}`)}}}
	agent.setAgentClient(&extensionErrorClient{recordingAgentClient: newRecordingAgentClient()})
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt ignored raw extension error")
	}

	agent.setAgentClient(newRecordingAgentClient())
	promptSession.rawMessages = rawMessageConfig{}
	promptSession.clientDead = false
	promptSession.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventError, ThreadID: "thread", TurnID: "turn", Err: errors.New("boom")}}}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt ignored event error")
	}

	promptSession.clientDead = false
	promptSession.client = &runEventsClient{}
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); !isTurnFailure(err, codex.CauseTransport) {
		t.Fatalf("Prompt with closed event stream err=%v, want codex_turn_failed transport", err)
	}

	promptSession.rawMessages = rawMessageConfig{enabled: true}
	promptSession.clientDead = false
	promptSession.agent = NewAgent(WithSessionStore(NewInMemorySessionStore()))
	promptSession.agent.setAgentClient(newRecordingAgentClient())
	promptSession.client = &runEventsClient{events: []codex.Event{{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"}}}
	promptSession.rolloutPath = filepath.Join(t.TempDir(), "missing.jsonl")
	if _, err := promptSession.Prompt(ctx, TextPromptRequest("s", "test-turn", "hi")); err == nil {
		t.Fatal("Prompt ignored final rollout mirror error")
	}

	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}
	promptSession.rolloutPath = empty
	if err := promptSession.mirrorAndEmitRollout(ctx); err != nil {
		t.Fatalf("empty mirror returned error: %v", err)
	}

	huge := filepath.Join(t.TempDir(), "huge.jsonl")
	if err := os.WriteFile(huge, []byte(strings.Repeat("x", maxSessionImportLineBytes+1)), 0o600); err != nil {
		t.Fatalf("write huge rollout: %v", err)
	}
	promptSession.rolloutPath = huge
	if err := promptSession.mirrorAndEmitRollout(ctx); err == nil {
		t.Fatal("mirror accepted scanner error")
	}

	invalid := filepath.Join(t.TempDir(), "invalid.jsonl")
	if err := os.WriteFile(invalid, []byte("[]\n"), 0o600); err != nil {
		t.Fatalf("write invalid rollout: %v", err)
	}
	promptSession.rolloutPath = invalid
	if err := promptSession.mirrorAndEmitRollout(ctx); err == nil {
		t.Fatal("mirror accepted invalid rollout entry")
	}

	valid := filepath.Join(t.TempDir(), "valid.jsonl")
	if err := os.WriteFile(valid, []byte(`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write valid rollout: %v", err)
	}
	promptSession.rolloutPath = valid
	promptSession.cwd = "/tmp/project"
	promptSession.agent = NewAgent(WithSessionStore(appendErrorStore{}))
	withRolloutAppendSettings(t, time.Second, []time.Duration{0})
	if err := promptSession.mirrorAndEmitRollout(ctx); err == nil {
		t.Fatal("mirror ignored store append error")
	}
	promptSession.agent = NewAgent()
	promptSession.agent.setAgentClient(&extensionErrorClient{recordingAgentClient: newRecordingAgentClient()})
	promptSession.rawMessages = rawMessageConfig{enabled: true}
	if err := promptSession.mirrorAndEmitRollout(ctx); err != nil {
		t.Fatalf("mirror returned error: %v", err)
	}
}

func TestSessionPromptCancelAndUpdateEdges(t *testing.T) {
	promptSession := &session{agent: NewAgent(), id: "s"}
	promptSession.setTurnID("")
	promptSession.cancelTurn()
	if promptSession.wasTurnCancelled() {
		t.Fatal("cancel without active turn marked canceled")
	}
	promptSession.setAccount(nil)
	if len(promptSession.accountMeta) != 0 {
		t.Fatal("empty account meta was stored")
	}
	missingMaterial := &session{agent: NewAgent(), materializedPath: filepath.Join(t.TempDir(), "missing")}
	if err := missingMaterial.Close(context.Background()); err != nil {
		t.Fatalf("close missing materialized path returned error: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := (&session{agent: NewAgent(), materializedPath: dir}).Close(context.Background()); err == nil {
		t.Fatal("close ignored materialized remove error")
	}
	if update := eventUpdates(codex.Event{Kind: codex.EventWarning, Text: "warn"}); len(update) != 1 || update[0].AgentThoughtChunk == nil {
		t.Fatalf("warning update = %#v", update)
	}
	if update := completeToolUpdate(codex.ToolEvent{ID: "tool", Title: "Title", Content: "done"}); update.ToolCallUpdate == nil {
		t.Fatalf("complete update with title/content = %#v", update)
	}
}

func TestSessionPromptCancelAndAccountUpdate(t *testing.T) {
	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	cancelSession := &session{agent: agent, id: "s", cwd: "/tmp/project", codexThreadID: "thread"}
	cancelSession.client = &cancelDuringRunClient{session: cancelSession}
	resp, err := cancelSession.Prompt(context.Background(), TextPromptRequest("s", "test-turn", "hi"))
	if err != nil || resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("canceled event prompt resp=%#v err=%v", resp, err)
	}

	interactionSession := &session{agent: agent, id: "interaction"}
	interactionSession.beginTurn(context.Background(), "test-turn")
	interactionCtx, finishInteraction := interactionSession.beginInteraction(context.Background(), "input")
	interactionSession.mu.Lock()
	cancelInteractionTurn := interactionSession.cancel
	interactionSession.mu.Unlock()
	cancelInteractionTurn()
	select {
	case <-interactionCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("interaction was not canceled when turn context ended")
	}
	finishInteraction()
	interactionSession.finishTurn()

	accountSession := &session{
		agent:         agent,
		id:            "acct",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		client: &runEventsClient{events: []codex.Event{
			{Kind: codex.EventAccountUpdated, Account: codex.Account{ID: "acct", Email: "new@example.com", PlanType: "pro", Raw: map[string]any{"accessToken": "secret"}}},
			{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"},
		}},
	}
	if _, acctErr := accountSession.Prompt(context.Background(), TextPromptRequest("acct", "test-turn", "hi")); acctErr != nil {
		t.Fatalf("account update prompt returned error: %v", acctErr)
	}
	if accountSession.accountMeta["email"] != "new@example.com" || accountSession.accountMeta["accessToken"] != nil {
		t.Fatalf("account update meta = %#v", accountSession.accountMeta)
	}
}

func TestSessionPromptDedupesDeltas(t *testing.T) {
	dedupeAgent := NewAgent()
	dedupeConn := newRecordingAgentClient()
	dedupeAgent.setAgentClient(dedupeConn)
	dedupeSession := &session{
		agent:         dedupeAgent,
		id:            "dedupe",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		client: &runEventsClient{events: []codex.Event{
			{Kind: codex.EventAgentMessageDelta, ItemID: "msg", ThreadID: "thread", TurnID: "turn", Text: "hello"},
			{Kind: codex.EventAgentMessageDelta, ItemID: "msg", ThreadID: "thread", TurnID: "turn", Text: "hello", Completed: true},
			{Kind: codex.EventAgentMessageDelta, ThreadID: "thread", TurnID: "turn", Text: "world"},
			{Kind: codex.EventAgentMessageDelta, ThreadID: "thread", TurnID: "turn", Text: "helloworld", Completed: true},
			{Kind: codex.EventReasoningDelta, ItemID: "why", ThreadID: "thread", TurnID: "turn", Text: "thinking"},
			{Kind: codex.EventReasoningDelta, ItemID: "why", ThreadID: "thread", TurnID: "turn", Text: "thinking", Completed: true},
			{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"},
		}},
	}
	if _, err := dedupeSession.Prompt(context.Background(), TextPromptRequest("dedupe", "test-turn", "hi")); err != nil {
		t.Fatalf("dedupe prompt returned error: %v", err)
	}
	messageUpdates := 0
	thoughtUpdates := 0
	for _, update := range dedupeConn.updates {
		if update.Update.AgentMessageChunk != nil {
			messageUpdates++
		}
		if update.Update.AgentThoughtChunk != nil {
			thoughtUpdates++
		}
	}
	if messageUpdates != 2 || thoughtUpdates != 1 {
		t.Fatalf("deduped updates message=%d thought=%d all=%#v", messageUpdates, thoughtUpdates, dedupeConn.updates)
	}
	if event := dedupeCompletedAggregateTextEvent(codex.Event{Kind: codex.EventAgentMessageDelta, Text: "same", Completed: true}, "same"); event.Text != "" {
		t.Fatalf("aggregate duplicate was not suppressed: %#v", event)
	}
}

func TestPromptPublishesDurableNativeIdentityAndReplayMatches(t *testing.T) {
	const (
		turnID              = "019f664f-e8bb-75c2-8110-f9a9cd2b65eb"
		intermediateMessage = "msg_intermediate"
		terminalMessage     = "msg_0add2dc109f9369e016a57a208bfe48191be93609f76b2a063"
	)

	entries := []SessionStoreEntry{
		SessionStoreEntry(`{"type":"session_meta","payload":{"id":"019f664f-e81a-7f51-9641-23dea9def926"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + turnID + `"}}`),
		SessionStoreEntry(`{"type":"turn_context","payload":{"turn_id":"` + turnID + `"}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","id":"` + intermediateMessage + `","role":"assistant","content":[{"type":"output_text","text":"before tool"}]}}`),
		SessionStoreEntry(`{"type":"response_item","payload":{"type":"message","id":"` + terminalMessage + `","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"done"}}`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"` + turnID + `"}}`),
	}

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	requireNoError(t, os.WriteFile(rollout, nil, 0o600))
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	liveConn := newRecordingAgentClient()
	agent.setAgentClient(liveConn)
	live := &session{
		agent:         agent,
		id:            "native-identity",
		cwd:           t.TempDir(),
		codexThreadID: "thread",
		rolloutPath:   rollout,
		client: &rolloutWritingRunClient{
			runEventsClient: runEventsClient{events: []codex.Event{
				{Kind: codex.EventAgentMessageDelta, TurnID: turnID, ItemID: intermediateMessage, Text: "before tool", Completed: true},
				{Kind: codex.EventToolStarted, TurnID: turnID, Tool: codex.ToolEvent{ID: "tool", Title: "tool"}},
				{Kind: codex.EventToolCompleted, TurnID: turnID, Tool: codex.ToolEvent{ID: "tool", Title: "tool"}},
				{Kind: codex.EventAgentMessageDelta, TurnID: turnID, ItemID: terminalMessage, Text: "done", Completed: true},
				{Kind: codex.EventCompleted, TurnID: turnID},
			}},
			path:    rollout,
			entries: entries,
		},
	}

	response, err := live.Prompt(context.Background(), TextPromptRequest(live.id, "test-turn", "run"))
	requireNoError(t, err)
	want := nativeTurnIdentity{turnID: turnID, messageID: terminalMessage}
	if got := nativeIdentityFromMeta(response.Meta); got != want {
		t.Fatalf("prompt native identity = %#v, want %#v; meta=%#v", got, want, response.Meta)
	}
	if got := lastNotificationNativeIdentity(liveConn.updates); got != want {
		t.Fatalf("live update native identity = %#v, want %#v; updates=%#v", got, want, liveConn.updates)
	}

	durable, err := store.Load(context.Background(), SessionKey{SessionID: string(live.id)})
	requireNoError(t, err)
	if got := rolloutNativeTerminalIdentity(durable); got != want {
		t.Fatalf("durable native identity = %#v, want %#v", got, want)
	}

	replayConn := newRecordingAgentClient()
	replayAgent := NewAgent()
	replayAgent.setAgentClient(replayConn)
	replayed := &session{agent: replayAgent, id: live.id}
	requireNoError(t, replayed.replayRollout(context.Background(), durable))
	if got := lastNotificationNativeIdentity(replayConn.updates); got != want {
		t.Fatalf("replay native identity = %#v, want %#v; updates=%#v", got, want, replayConn.updates)
	}
}

func TestPromptPublishesTurnIdentityWithoutAssistantText(t *testing.T) {
	tests := []struct {
		name    string
		events  []codex.Event
		message string
	}{
		{
			name:   "completion only",
			events: []codex.Event{{Kind: codex.EventCompleted, TurnID: "turn-only"}},
		},
		{
			name: "empty terminal assistant",
			events: []codex.Event{
				{Kind: codex.EventAgentMessageDelta, TurnID: "turn-empty", ItemID: "msg-empty", Completed: true},
				{Kind: codex.EventCompleted, TurnID: "turn-empty"},
			},
			message: "msg-empty",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := NewAgent()
			conn := newRecordingAgentClient()
			agent.setAgentClient(conn)
			s := &session{
				agent: agent, id: acp.SessionId(test.name), cwd: t.TempDir(), codexThreadID: "thread",
				client: &runEventsClient{events: test.events},
			}

			response, err := s.Prompt(context.Background(), TextPromptRequest(s.id, "test-turn", "run"))
			requireNoError(t, err)
			want := nativeTurnIdentity{turnID: test.events[len(test.events)-1].TurnID, messageID: test.message}
			if got := nativeIdentityFromMeta(response.Meta); got != want {
				t.Fatalf("response identity = %#v, want %#v", got, want)
			}
			if got := lastNotificationNativeIdentity(conn.updates); got != want {
				t.Fatalf("terminal update identity = %#v, want %#v; updates=%#v", got, want, conn.updates)
			}
		})
	}
}

type rolloutWritingRunClient struct {
	runEventsClient
	path    string
	entries []SessionStoreEntry
}

type delayedTerminalRunClient struct {
	*spyCodexClient
	terminal <-chan struct{}
	started  chan<- struct{}
}

type emptyTurnIDClient struct {
	*spyCodexClient
}

type overflowTurnClient struct {
	*spyCodexClient
	dispatchFailure bool
	cancelStarted   chan [2]string
	cancelRelease   <-chan struct{}
}

func (*emptyTurnIDClient) RunTurn(context.Context, codex.TurnStartRequest) (codex.Turn, error) {
	return codex.Turn{Events: make(chan codex.Event)}, nil
}

func (c *overflowTurnClient) RunTurn(
	context.Context,
	codex.TurnStartRequest,
) (codex.Turn, error) {
	if c.dispatchFailure {
		return codex.Turn{ID: "overflow-turn"}, codex.ErrTurnEventOverflow
	}

	events := make(chan codex.Event, 1)
	events <- codex.Event{
		Kind: codex.EventError, TurnID: "overflow-turn", Err: codex.ErrTurnEventOverflow,
	}
	close(events)

	return codex.Turn{ID: "overflow-turn", Events: events}, nil
}

func (c *overflowTurnClient) CancelTurn(_ context.Context, threadID, turnID string) error {
	c.cancelStarted <- [2]string{threadID, turnID}
	<-c.cancelRelease

	return nil
}

func TestPromptRejectsAcceptedTurnWithoutNativeIdentity(t *testing.T) {
	agent := NewAgent()
	session := &session{
		agent: agent, id: "session", cwd: "/tmp/project",
		codexThreadID: "thread", client: &emptyTurnIDClient{spyCodexClient: newSpyCodexClient()},
	}

	_, err := session.Prompt(
		context.Background(),
		TextPromptRequest(session.id, "turn-nonce", "hello"),
	)
	require.ErrorContains(t, err, "without naming it")
}

func TestTurnOverflowContainsNativeTurnBeforePromptFailure(t *testing.T) {
	for _, dispatchFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("dispatch_failure_%t", dispatchFailure), func(t *testing.T) {
			cancelRelease := make(chan struct{})
			client := &overflowTurnClient{
				spyCodexClient:  newSpyCodexClient(),
				dispatchFailure: dispatchFailure,
				cancelStarted:   make(chan [2]string, 1),
				cancelRelease:   cancelRelease,
			}
			session := &session{
				agent: NewAgent(), id: "session", cwd: "/tmp/project",
				codexThreadID: "thread", client: client,
			}
			promptDone := make(chan error, 1)
			go func() {
				_, err := session.Prompt(
					context.Background(),
					TextPromptRequest(session.id, "turn-nonce", "hello"),
				)
				promptDone <- err
			}()

			require.Equal(t, [2]string{"thread", "overflow-turn"}, <-client.cancelStarted)
			select {
			case err := <-promptDone:
				t.Fatalf("overflow failed prompt before native containment completed: %v", err)
			default:
			}

			close(cancelRelease)
			require.ErrorContains(t, <-promptDone, codex.ErrTurnEventOverflow.Error())
		})
	}
}

func (c *delayedTerminalRunClient) RunTurn(
	ctx context.Context,
	req codex.TurnStartRequest,
) (codex.Turn, error) {
	close(c.started)
	events := make(chan codex.Event, 1)
	go func() {
		defer close(events)
		select {
		case <-c.terminal:
			events <- codex.Event{
				Kind: codex.EventCompleted, ThreadID: req.ThreadID,
				TurnID: "fallback-turn", StopReason: codex.StopReasonEndTurn,
			}
		case <-ctx.Done():
		}
	}()

	return codex.Turn{ID: "fallback-turn", Events: events}, nil
}

func (c *rolloutWritingRunClient) RunTurn(
	ctx context.Context,
	req codex.TurnStartRequest,
) (codex.Turn, error) {
	var content strings.Builder
	for _, entry := range c.entries {
		content.Write(entry)
		content.WriteByte('\n')
	}
	if err := os.WriteFile(c.path, []byte(content.String()), 0o600); err != nil {
		return codex.Turn{}, err
	}

	return c.runEventsClient.RunTurn(ctx, req)
}

func nativeIdentityFromMeta(meta map[string]any) nativeTurnIdentity {
	codexMeta, _ := meta[codexMetaKey].(map[string]any)

	return nativeTurnIdentity{
		turnID:    stringFromAny(codexMeta[codexTurnIDMetaKey]),
		messageID: stringFromAny(codexMeta[codexMessageIDMetaKey]),
	}
}

func lastNotificationNativeIdentity(notifications []acp.SessionNotification) nativeTurnIdentity {
	var identity nativeTurnIdentity
	for _, notification := range notifications {
		current := nativeIdentityFromMeta(notification.Meta)
		if current.turnID != "" {
			identity.turnID = current.turnID
		}
		if current.messageID != "" {
			identity.messageID = current.messageID
		}
	}

	return identity
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSessionPromptUsageUpdates(t *testing.T) {
	usageAgent := NewAgent()
	usageConn := newRecordingAgentClient()
	usageAgent.setAgentClient(usageConn)
	usageSession := &session{
		agent:         usageAgent,
		id:            "usage",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		client: &runEventsClient{events: []codex.Event{
			{
				Kind:     codex.EventUsageUpdated,
				ThreadID: "thread",
				TurnID:   "turn",
				Usage:    codex.Usage{InputTokens: 22143, CachedReadTokens: 6528, OutputTokens: 322, ReasoningOutputTokens: 157, TotalTokens: 22465},
				TokenUsage: codex.TokenUsage{
					Last:               codex.Usage{InputTokens: 22143, CachedReadTokens: 6528, OutputTokens: 322, ReasoningOutputTokens: 157, TotalTokens: 22465},
					Total:              codex.Usage{InputTokens: 23000, CachedReadTokens: 6528, OutputTokens: 400, ReasoningOutputTokens: 200, TotalTokens: 23400},
					ModelContextWindow: 258400,
				},
			},
			{Kind: codex.EventCompleted, ThreadID: "thread", TurnID: "turn"},
		}},
	}
	usageResp, err := usageSession.Prompt(context.Background(), TextPromptRequest("usage", "test-turn", "hi"))
	if err != nil {
		t.Fatalf("usage prompt returned error: %v", err)
	}
	if usageResp.Usage == nil ||
		usageResp.Usage.InputTokens != 22143 ||
		usageResp.Usage.OutputTokens != 322 ||
		usageResp.Usage.TotalTokens != 22465 ||
		usageResp.Usage.CachedReadTokens == nil ||
		*usageResp.Usage.CachedReadTokens != 6528 ||
		usageResp.Usage.ThoughtTokens == nil ||
		*usageResp.Usage.ThoughtTokens != 157 {
		t.Fatalf("prompt usage = %#v", usageResp.Usage)
	}
	if len(usageConn.updates) != 1 || usageConn.updates[0].Update.UsageUpdate == nil {
		t.Fatalf("usage updates = %#v", usageConn.updates)
	}
	usageUpdate := usageConn.updates[0].Update.UsageUpdate
	codexMeta, _ := usageUpdate.Meta[codexMetaKey].(map[string]any)
	usageMeta, _ := codexMeta[codexUsageMetaKey].(map[string]any)
	if usageUpdate.Used != 23400 ||
		usageUpdate.Size != 258400 ||
		usageMeta[usageInputTokensKey] != 22143 ||
		usageMeta[usageCachedReadTokensKey] != 6528 ||
		usageMeta[usageOutputTokensKey] != 322 ||
		usageMeta[usageReasoningOutputKey] != 157 ||
		usageMeta[usageTotalTokensKey] != 22465 {
		t.Fatalf("usage update=%#v meta=%#v", usageUpdate, usageMeta)
	}
	threadUsageMeta, _ := codexMeta[codexThreadUsageMetaKey].(map[string]any)
	if usageUpdate.Used != 23400 || threadUsageMeta[usageTotalTokensKey] != 23400 {
		t.Fatalf("thread usage update=%#v meta=%#v", usageUpdate, threadUsageMeta)
	}

	completedUsageConn := newRecordingAgentClient()
	usageAgent.setAgentClient(completedUsageConn)
	usageSession.client = &runEventsClient{events: []codex.Event{{
		Kind:     codex.EventCompleted,
		ThreadID: "thread",
		TurnID:   "turn",
		Usage:    codex.Usage{InputTokens: 1, OutputTokens: 2},
	}}}
	completedUsageResp, err := usageSession.Prompt(context.Background(), TextPromptRequest("usage", "test-turn", "hi"))
	if err != nil {
		t.Fatalf("completed usage prompt returned error: %v", err)
	}
	if completedUsageResp.Usage == nil || completedUsageResp.Usage.TotalTokens != 3 {
		t.Fatalf("completed usage response = %#v", completedUsageResp.Usage)
	}
	if len(completedUsageConn.updates) != 1 || completedUsageConn.updates[0].Update.UsageUpdate == nil {
		t.Fatalf("completed usage updates = %#v", completedUsageConn.updates)
	}
}

func TestSessionPromptRawRolloutTail(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte("\n"+`{"type":"event_msg"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	rawSession := &session{agent: NewAgent(), id: "raw", cwd: "/tmp/project", rolloutPath: rollout, rawMessages: rawMessageConfig{enabled: true}}
	if err := rawSession.mirrorAndEmitRollout(context.Background()); err != nil {
		t.Fatalf("mirror blank+valid rollout returned error: %v", err)
	}
	stop, done := rawSession.startRolloutTail(context.Background(), nil)
	time.Sleep(150 * time.Millisecond)
	stop()
	<-done
}

func TestPromptWaitsForNativeTerminalAfterRolloutTaskComplete(t *testing.T) {
	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, nil, 0o600); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}
	terminal := make(chan struct{})
	started := make(chan struct{})
	session := &session{
		agent:         agent,
		id:            "fallback",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		rolloutPath:   rollout,
		client: &delayedTerminalRunClient{
			spyCodexClient: newSpyCodexClient(),
			terminal:       terminal,
			started:        started,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan promptResult, 1)
	go func() {
		resp, err := session.Prompt(ctx, TextPromptRequest("fallback", "test-turn", "prove it"))
		result <- promptResult{response: resp, err: err}
	}()
	<-started
	require.NoError(t, os.WriteFile(rollout, []byte(
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"fallback-turn"}}`+"\n"+
			`{"type":"response_item","payload":{"type":"message","role":"assistant","id":"fallback-message","content":[{"type":"output_text","text":"1 + 1 = 2"}]}}`+"\n"+
			`{"type":"event_msg","payload":{"type":"agent_message","message":"1 + 1 = 2"}}`+"\n"+
			`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"fallback-turn"}}`+"\n",
	), 0o600))
	require.Eventually(t, func() bool {
		session.mirrorMu.Lock()
		defer session.mirrorMu.Unlock()

		return session.visibleRows == 4
	}, time.Second, 10*time.Millisecond)
	select {
	case early := <-result:
		t.Fatalf("rollout task_complete settled prompt before native terminal: %#v", early)
	default:
	}
	close(terminal)
	completed := <-result
	resp, err := completed.response, completed.err
	if err != nil || resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("native-terminal prompt resp=%#v err=%v", resp, err)
	}
	if len(conn.updates) != 3 || conn.updates[0].Update.AgentMessageChunk == nil {
		t.Fatalf("task-complete updates = %#v", conn.updates)
	}
	wantIdentity := nativeTurnIdentity{turnID: "fallback-turn", messageID: "fallback-message"}
	if got := nativeIdentityFromMeta(resp.Meta); got != wantIdentity {
		t.Fatalf("task-complete response identity = %#v, want %#v", got, wantIdentity)
	}
	if got := lastNotificationNativeIdentity(conn.updates); got != wantIdentity {
		t.Fatalf("task-complete update identity = %#v, want %#v", got, wantIdentity)
	}
	if session.visibleRows != 4 {
		t.Fatalf("rollout visible cursor=%d", session.visibleRows)
	}
}

func TestPromptReturnsRolloutEventUpdateError(t *testing.T) {
	agent := NewAgent()
	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")})

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, nil, 0o600); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}
	writeErr := make(chan error, 1)
	time.AfterFunc(20*time.Millisecond, func() {
		writeErr <- os.WriteFile(rollout, []byte(
			`{"type":"event_msg","payload":{"type":"agent_message","message":"visible"}}`+"\n",
		), 0o600)
	})
	defer func() {
		if err := <-writeErr; err != nil {
			t.Fatalf("write rollout rows: %v", err)
		}
	}()

	session := &session{
		agent:         agent,
		id:            "fallback",
		cwd:           "/tmp/project",
		codexThreadID: "thread",
		rolloutPath:   rollout,
		client:        &openRunEventsClient{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := session.Prompt(ctx, TextPromptRequest("fallback", "test-turn", "show it")); err == nil {
		t.Fatal("rollout event update error was ignored")
	}
}

func TestPromptReturnsTerminalRolloutIdentityUpdateError(t *testing.T) {
	updateErr := errors.New("identity update failed")
	agent := NewAgent()
	agent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: updateErr})

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(rollout, nil, 0o600); err != nil {
		t.Fatalf("write empty rollout: %v", err)
	}
	writeErr := make(chan error, 1)
	time.AfterFunc(20*time.Millisecond, func() {
		writeErr <- os.WriteFile(rollout, []byte(
			`{"type":"response_item","payload":{"type":"message","role":"assistant","id":"empty-message","content":[]}}`+"\n"+
				`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"identity-turn"}}`+"\n",
		), 0o600)
	})
	defer func() {
		if err := <-writeErr; err != nil {
			t.Fatalf("write rollout rows: %v", err)
		}
	}()

	s := &session{
		agent: agent, id: "identity-error", cwd: t.TempDir(), codexThreadID: "thread",
		rolloutPath: rollout, client: &openRunEventsClient{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.Prompt(ctx, TextPromptRequest(s.id, "test-turn", "run")); !errors.Is(err, updateErr) {
		t.Fatalf("Prompt error = %v, want %v", err, updateErr)
	}
}

func TestSandboxPolicyHelpers(t *testing.T) {
	if sandboxMode("workspace-write") != "workspace-write" {
		t.Fatal("string sandbox mode changed")
	}
	for _, tc := range []struct {
		policy map[string]any
		want   string
	}{
		{map[string]any{"type": "dangerFullAccess"}, "danger-full-access"},
		{map[string]any{"type": "readOnly"}, "read-only"},
		{map[string]any{"type": "workspaceWrite"}, "workspace-write"},
	} {
		if got := sandboxMode(tc.policy); got != tc.want {
			t.Fatalf("sandboxMode(%#v) = %#v", tc.policy, got)
		}
	}
	if sandboxMode(map[string]any{"type": "unknown"}) != nil || sandboxMode(123) != nil {
		t.Fatal("sandboxMode accepted unknown policy")
	}

	danger, _ := sandboxPolicy("danger-full-access").(map[string]any)
	if danger["type"] != "dangerFullAccess" {
		t.Fatalf("danger policy = %#v", danger)
	}
	readOnly, _ := sandboxPolicy("read-only").(map[string]any)
	if readOnly["type"] != "readOnly" || readOnly["networkAccess"] != false {
		t.Fatalf("read-only policy = %#v", readOnly)
	}
	workspace, _ := sandboxPolicy("workspace-write").(map[string]any)
	if workspace["type"] != "workspaceWrite" || workspace["writableRoots"] == nil {
		t.Fatalf("workspace policy = %#v", workspace)
	}
	if sandboxPolicy("custom") != "custom" || sandboxPolicy(123) != 123 {
		t.Fatal("sandboxPolicy did not preserve custom policies")
	}
	nilMap, _ := sandboxPolicy(map[string]any(nil)).(map[string]any)
	if nilMap != nil {
		t.Fatal("nil map policy was not preserved")
	}
	defaulted, _ := sandboxPolicy(map[string]any{"type": "workspaceWrite"}).(map[string]any)
	if defaulted["writableRoots"] == nil ||
		defaulted["networkAccess"] != false ||
		defaulted["excludeTmpdirEnvVar"] != false ||
		defaulted["excludeSlashTmp"] != false {
		t.Fatalf("workspace defaults = %#v", defaulted)
	}
	alreadyComplete := map[string]any{
		"type":                "workspace-write",
		"writableRoots":       []string{"/repo"},
		"networkAccess":       true,
		"excludeTmpdirEnvVar": true,
		"excludeSlashTmp":     true,
	}
	normalized, _ := sandboxPolicy(alreadyComplete).(map[string]any)
	if normalized["type"] != "workspaceWrite" || normalized["networkAccess"] != true {
		t.Fatalf("normalized policy = %#v", normalized)
	}
	otherMap := map[string]any{"type": "custom"}
	if got, _ := sandboxPolicy(otherMap).(map[string]any); got["type"] != "custom" {
		t.Fatalf("custom map policy = %#v", got)
	}
}

func TestEventUpdateHelpers(t *testing.T) {
	updates := eventUpdates(codex.Event{Kind: codex.EventToolStarted, Tool: codex.ToolEvent{ID: "tool", Kind: "commandExecution", Title: "Run"}})
	if len(updates) != 1 || updates[0].ToolCall == nil {
		t.Fatalf("tool start updates = %#v", updates)
	}
	updates = eventUpdates(codex.Event{Kind: codex.EventToolDelta, Tool: codex.ToolEvent{ID: "tool"}, Text: "out"})
	if len(updates) != 1 || updates[0].ToolCallUpdate == nil {
		t.Fatalf("tool delta updates = %#v", updates)
	}
	updates = eventUpdates(codex.Event{Kind: codex.EventToolCompleted, Tool: codex.ToolEvent{ID: "tool", Kind: "fileChange", Content: "done"}})
	if len(updates) != 1 || updates[0].ToolCallUpdate == nil {
		t.Fatalf("tool complete updates = %#v", updates)
	}
	updates = eventUpdates(codex.Event{Kind: codex.EventDiffUpdated, Diff: "diff"})
	if len(updates) != 1 || updates[0].ToolCallUpdate == nil {
		t.Fatalf("diff updates = %#v", updates)
	}
	if toolKind(codex.ToolEvent{Kind: "shell"}) != acp.ToolKindExecute || toolKind(codex.ToolEvent{Kind: "patch"}) != acp.ToolKindEdit {
		t.Fatal("tool kind mapping failed")
	}
	meta := structuredOutputMeta(`{"ok":true}`, map[string]any{"type": "object"})
	if meta[codexMetaKey] == nil || meta["claude"] != nil {
		t.Fatalf("structured meta = %#v", meta)
	}
	if structuredOutputMeta("not-json", map[string]any{"type": "object"}) != nil {
		t.Fatal("invalid structured output emitted meta")
	}
}

func TestEventUpdateEmptyAndFallbackBranches(t *testing.T) {
	if eventUpdates(codex.Event{Kind: codex.EventAgentMessageDelta}) != nil {
		t.Fatal("empty agent delta emitted update")
	}
	if eventUpdates(codex.Event{Kind: codex.EventReasoningDelta}) != nil {
		t.Fatal("empty reasoning delta emitted update")
	}
	if eventUpdates(codex.Event{Kind: codex.EventDiffUpdated}) != nil {
		t.Fatal("empty diff emitted update")
	}
	if eventUpdates(codex.Event{Kind: codex.EventWarning}) != nil {
		t.Fatal("empty warning emitted update")
	}
	if eventUpdates(codex.Event{Kind: codex.EventError}) != nil {
		t.Fatal("error event emitted direct update")
	}
	if planUpdate(nil) != nil {
		t.Fatal("empty plan emitted update")
	}
	if updates := toolDeltaUpdate(codex.ToolEvent{ID: "tool", Content: "fallback"}, ""); len(updates) != 1 {
		t.Fatalf("tool content fallback updates = %#v", updates)
	}
	if toolDeltaUpdate(codex.ToolEvent{}, "") != nil {
		t.Fatal("empty tool delta emitted update")
	}
	if update := completeToolUpdate(codex.ToolEvent{ID: "tool"}); update.ToolCallUpdate == nil {
		t.Fatalf("complete tool update = %#v", update)
	}
	if planStatus(codex.PlanStepPending) != acp.PlanEntryStatusPending {
		t.Fatal("pending plan status did not map")
	}
	if stop, clean := promptStopReason(codex.StopReasonCancelled); !clean || stop != acp.StopReasonCancelled {
		t.Fatalf("cancelled stop reason = %q clean=%v", stop, clean)
	}
	if stop, clean := promptStopReason(codex.StopReasonError); clean || stop != "" {
		t.Fatalf("a native failure reported stop reason %q clean=%v", stop, clean)
	}
	if stop, clean := promptStopReason(codex.StopReasonEndTurn); !clean || stop != acp.StopReasonEndTurn {
		t.Fatalf("end-turn stop reason = %q clean=%v", stop, clean)
	}
	if usageFromCodex(codex.Usage{}) != nil {
		t.Fatal("zero usage emitted usage")
	}
	usage := usageFromCodex(codex.Usage{InputTokens: 1, CachedReadTokens: 2, CachedWriteTokens: 3, OutputTokens: 4, ReasoningOutputTokens: 5})
	if usage.TotalTokens != 5 ||
		usage.CachedReadTokens == nil ||
		*usage.CachedReadTokens != 2 ||
		usage.CachedWriteTokens == nil ||
		*usage.CachedWriteTokens != 3 ||
		usage.ThoughtTokens == nil ||
		*usage.ThoughtTokens != 5 {
		t.Fatal("usage mapping failed")
	}
	if updates := usageUpdateFromCodex(codex.Usage{}); updates != nil {
		t.Fatalf("zero usage update = %#v", updates)
	}
	updates := usageUpdateFromCodex(codex.Usage{InputTokens: 1, CachedWriteTokens: 2, OutputTokens: 3})
	if len(updates) != 1 {
		t.Fatalf("usage update = %#v", updates)
	}
	updateMeta, _ := updates[0].UsageUpdate.Meta[codexMetaKey].(map[string]any)
	updateUsage, _ := updateMeta[codexUsageMetaKey].(map[string]any)
	if updateUsage[usageCachedWriteTokensKey] != 2 {
		t.Fatalf("cached write usage meta = %#v", updateUsage)
	}
}

func TestTokenUsageAndObserverBranches(t *testing.T) {
	usage := usageFromCodex(codex.Usage{InputTokens: 1, CachedReadTokens: 2, CachedWriteTokens: 3, OutputTokens: 4, ReasoningOutputTokens: 5})
	tokenUpdates := tokenUsageUpdateFromCodex(codex.TokenUsage{
		Last:               codex.Usage{InputTokens: 1, OutputTokens: 2},
		Total:              codex.Usage{InputTokens: 3, OutputTokens: 4},
		ModelContextWindow: 100,
	})
	if len(tokenUpdates) != 1 || tokenUpdates[0].UsageUpdate.Used != 7 || tokenUpdates[0].UsageUpdate.Size != 100 {
		t.Fatalf("token usage update = %#v", tokenUpdates)
	}
	var streamedUsage codex.Usage
	var streamedThreadUsage codex.Usage
	var streamedWindow int64
	firstUsageUpdates := usageUpdatesForEvent(codex.Event{Kind: codex.EventUsageUpdated, TokenUsage: codex.TokenUsage{Last: codex.Usage{InputTokens: 1, OutputTokens: 2}}}, &streamedUsage, &streamedThreadUsage, &streamedWindow)
	duplicateUsageUpdates := usageUpdatesForEvent(codex.Event{Kind: codex.EventUsageUpdated, TokenUsage: codex.TokenUsage{Last: codex.Usage{InputTokens: 1, OutputTokens: 2}}}, &streamedUsage, &streamedThreadUsage, &streamedWindow)
	if len(firstUsageUpdates) != 1 || duplicateUsageUpdates != nil {
		t.Fatalf("usage update dedupe first=%#v duplicate=%#v", firstUsageUpdates, duplicateUsageUpdates)
	}
	observerResult := promptResultForObserver(acp.PromptResponse{Usage: usage}, nil, "gpt")
	if observerResult.CachedReadTokens != 2 ||
		observerResult.CachedWriteTokens != 3 ||
		observerResult.ThoughtTokens != 5 ||
		observerResult.InputTokens != 1 ||
		observerResult.OutputTokens != 4 ||
		observerResult.TotalTokens != 5 {
		t.Fatalf("observer result = %#v", observerResult)
	}
	if structuredOutputMeta("", map[string]any{"type": "object"}) != nil || structuredOutputMeta(`{"ok":true}`, nil) != nil {
		t.Fatal("structured output emitted without schema/text")
	}
}

func TestPromptValueHelpers(t *testing.T) {
	if nullableString("") != nil || nullableString("x") != "x" {
		t.Fatal("nullableString failed")
	}
	if toolKind(codex.ToolEvent{Kind: "mcpToolCall"}) != acp.ToolKindOther || toolKind(codex.ToolEvent{Kind: "unknown"}) != acp.ToolKindOther {
		t.Fatal("toolKind special cases failed")
	}
	if _, clean := promptStopReason(codex.StopReasonError); clean {
		t.Fatal("a native failure was reported as a clean stop")
	}
}

func TestRecordRawEmitFailure(t *testing.T) {
	recordSession := &session{agent: NewAgent(), id: "record"}
	recordSession.recordRawEmitFailure(context.Background(), nil)
	if recordSession.rawEmitFailures != 0 {
		t.Fatal("nil raw emit error advanced the counter")
	}

	recordSession.recordRawEmitFailure(context.Background(), errors.New("emit failed"))
	if recordSession.rawEmitFailures != 1 {
		t.Fatalf("raw emit failure counter = %d, want 1", recordSession.rawEmitFailures)
	}
}

// TestPromptContentFailsClosed pins the fail-closed prompt-content contract:
// empty prompts, audio blocks, and unknown or empty content blocks reject with
// the uniform unsupported/prompt shape, and images without embedded data
// reject with the uniform prompt.image shape. Nothing is silently dropped.
func TestPromptContentFailsClosed(t *testing.T) {
	ctx := context.Background()
	promptSession := &session{agent: NewAgent(), id: "s", cwd: "/tmp/project", codexThreadID: "thread", client: &runEventsClient{}}

	for _, tt := range []struct {
		name      string
		prompt    []acp.ContentBlock
		wantField string
		wantError string
	}{
		{name: "audio block", prompt: []acp.ContentBlock{acp.AudioBlock("x", "audio/wav")}, wantField: "prompt", wantError: "unsupported"},
		{name: "empty prompt", prompt: nil, wantField: "prompt", wantError: "unsupported"},
		{name: "unknown block", prompt: []acp.ContentBlock{{}}, wantField: "prompt", wantError: "unsupported"},
		{name: "data-less image", prompt: []acp.ContentBlock{acp.ImageBlock("", "image/png")}, wantField: "prompt.image", wantError: "missing_data"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := promptSession.Prompt(ctx, acp.PromptRequest{SessionId: "s", Prompt: tt.prompt, Meta: inboundRouteMeta("turn-1")})

			var reqErr *acp.RequestError
			if !errors.As(err, &reqErr) || reqErr.Code != -32602 {
				t.Fatalf("prompt error = %#v, want -32602 invalid params", err)
			}
			if data, ok := reqErr.Data.(map[string]any); !ok || data["error"] != tt.wantError || data["field"] != tt.wantField {
				t.Fatalf("prompt data = %#v, want %s/%s", reqErr.Data, tt.wantError, tt.wantField)
			}
		})
	}
}

// heldTurnClient parks inside the native turn until the test releases it, so a
// second prompt arrives while the first still holds the session's single turn
// slot.
type heldTurnClient struct {
	*spyCodexClient
	started chan struct{}
	release chan struct{}
}

func (c *heldTurnClient) RunTurn(context.Context, codex.TurnStartRequest) (codex.Turn, error) {
	close(c.started)
	<-c.release

	return codex.Turn{}, errors.New("native turn released")
}

// Prompt turns are serialized per session, so a second prompt arriving while one
// is in flight is answered with the `session_prompt` backpressure invalid
// request. It is an error and never a response, because the second prompt never
// opened a turn and so has no terminal to report — a successful `cancelled`
// there would invent one and hide the only signal the host can retry on.
func TestConcurrentPromptIsRefusedWithSessionPromptBackpressure(t *testing.T) {
	client := &heldTurnClient{
		spyCodexClient: newSpyCodexClient(),
		started:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	agent := NewAgent(withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))
	created, err := agent.NewSession(context.Background(), NewSessionRequest("/tmp/project"))
	require.NoError(t, err)

	firstDone := make(chan error, 1)
	go func() {
		_, promptErr := agent.Prompt(
			context.Background(),
			TextPromptRequest(created.SessionId, "first-turn", "hold the slot"),
		)
		firstDone <- promptErr
	}()
	<-client.started

	resp, err := agent.Prompt(
		context.Background(),
		TextPromptRequest(created.SessionId, "second-turn", "concurrent"),
	)
	require.Equal(t, acp.PromptResponse{}, resp, "a refused prompt returns no response at all")

	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32600, reqErr.Code)
	require.Equal(t,
		map[string]any{jsonFieldError: valueBackpressure, jsonFieldLimit: limitSessionPrompt},
		reqErr.Data,
	)

	close(client.release)
	require.Error(t, <-firstDone)
	require.NoError(t, agent.Close())
}

// TestPromptSettlementKeepsTheNativeCauseWhenTheCommitAlsoFails pins the wire
// answer to a double fault. The native turn failed and the durable
// foreground-prefix commit failed behind it: the host is owed the turn's own
// cause, because a store fault belongs to the adapter and names nothing a host
// can classify or decide a retry against.
func TestPromptSettlementKeepsTheNativeCauseWhenTheCommitAlsoFails(t *testing.T) {
	ctx := context.Background()
	promptSession := &session{
		agent: NewAgent(WithSessionStore(appendErrorStore{})),
		id:    "s",
		cwd:   "/tmp/project",
	}

	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(rollout, []byte(`{"type":"event_msg"}`+"\n"), 0o600))
	promptSession.rolloutPath = rollout

	withRolloutAppendSettings(t, time.Second, []time.Duration{0})

	nativeFailure := promptSession.mapTurnFailure(&codex.TurnFailedError{
		Cause:   codex.CauseProvider,
		Message: "provider refused the turn",
	})

	_, err := promptSession.settlePrompt(ctx, ctx, nil, promptTurnResult{
		state:    &promptEventState{},
		failure:  nativeFailure,
		accepted: true,
	}, nil)
	require.ErrorIs(t, err, nativeFailure)

	var requestErr *acp.RequestError

	require.ErrorAs(t, err, &requestErr)

	data := asType[map[string]any](t, requestErr.Data)
	require.Equal(t, valueTurnFailed, data[jsonFieldError])
	require.Equal(t, codex.CauseProvider, data[jsonFieldCause])
	require.Equal(t, "provider refused the turn", data[jsonFieldMessage])
}
