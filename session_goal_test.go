package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

func TestGoalMetaParsingValidationAndCanonicalOutput(t *testing.T) {
	budget := float64(5000)
	input, err := parseGoalFromMeta(map[string]any{
		codexMetaKey: map[string]any{
			codexGoalMetaKey: map[string]any{
				goalFieldObjective:       " Ship goals ",
				goalFieldStatus:          CodexGoalStatusBudgetLimited,
				goalFieldTokenBudget:     budget,
				goalFieldTokensUsed:      float64(99),
				goalFieldTimeUsedSeconds: float64(12),
				goalFieldCreatedAt:       float64(10),
				goalFieldUpdatedAt:       float64(11),
				goalFieldThreadID:        "ignored",
				goalFieldSource:          "ignored",
			},
		},
	})
	if err != nil {
		t.Fatalf("parseGoalFromMeta returned error: %v", err)
	}
	if !input.present || input.clear || input.goal.Objective != "Ship goals" || input.goal.Status != CodexGoalStatusBudgetLimited || input.goal.TokenBudget == nil || *input.goal.TokenBudget != 5000 {
		t.Fatalf("input = %#v", input)
	}
	if input.goal.TokensUsed != 0 || input.goal.ThreadID != "" || input.goal.Source != "" {
		t.Fatalf("client input kept Codex-owned fields: %#v", input.goal)
	}

	meta := canonicalGoalMeta(CodexGoal{
		Objective:       "Ship goals",
		Status:          CodexGoalStatusActive,
		TokenBudget:     input.goal.TokenBudget,
		TokensUsed:      1,
		TimeUsedSeconds: 2,
		CreatedAt:       3,
		UpdatedAt:       4,
		ThreadID:        "thread",
		Source:          CodexGoalSourceClient,
	})
	if meta[goalFieldObjective] != "Ship goals" || meta[goalFieldTokenBudget] != int64(5000) || meta[goalFieldThreadID] != "thread" {
		t.Fatalf("canonical meta = %#v", meta)
	}
	if canonicalGoalMeta(CodexGoal{Objective: "x", Status: CodexGoalStatusActive})[goalFieldTokenBudget] != nil {
		t.Fatal("nil token budget was not explicit null")
	}

	cases := []any{
		"goal",
		map[string]any{goalFieldStatus: CodexGoalStatusActive},
		map[string]any{goalFieldObjective: 123},
		map[string]any{goalFieldObjective: "   "},
		map[string]any{goalFieldObjective: strings.Repeat("x", maxGoalObjectiveBytes+1)},
		map[string]any{goalFieldObjective: "Ship", goalFieldStatus: 123},
		map[string]any{goalFieldObjective: "Ship", goalFieldStatus: "blocked"},
		map[string]any{goalFieldObjective: "Ship", goalFieldTokenBudget: "bad"},
		map[string]any{goalFieldObjective: "Ship", goalFieldTokenBudget: -1},
		map[string]any{goalFieldObjective: "Ship", goalFieldTokenBudget: 1.5},
		map[string]any{goalFieldObjective: "Ship", "unexpected": true},
	}
	for _, tc := range cases {
		if _, err := parseGoalValue(tc); err == nil {
			t.Fatalf("parseGoalValue accepted %#v", tc)
		}
	}

	input, err = parseGoalFromMeta(nil)
	if err != nil || input.present {
		t.Fatalf("nil meta input=%#v err=%v", input, err)
	}
	input, err = parseGoalFromMeta(map[string]any{codexMetaKey: map[string]any{codexGoalMetaKey: nil}})
	if err != nil || !input.present || !input.clear {
		t.Fatalf("clear meta input=%#v err=%v", input, err)
	}
	input, err = parseGoalValue(map[string]any{goalFieldObjective: "Ship"})
	if err != nil || input.goal.Status != CodexGoalStatusActive {
		t.Fatalf("default status input=%#v err=%v", input, err)
	}
	if input, err = parseGoalRaw(json.RawMessage(`{"objective":"raw","tokenBudget":42}`)); err != nil || input.goal.Objective != "raw" || input.goal.TokenBudget == nil || *input.goal.TokenBudget != 42 {
		t.Fatalf("parseGoalRaw input=%#v err=%v", input, err)
	}
	if _, err = parseGoalRaw(json.RawMessage(``)); err == nil {
		t.Fatal("parseGoalRaw accepted empty raw message")
	}

	number := json.Number("42")
	parsed, err := goalInt64(number)
	if err != nil || parsed != 42 {
		t.Fatalf("goalInt64 json.Number parsed=%d err=%v", parsed, err)
	}
	if _, err := goalInt64(json.Number("bad")); err == nil {
		t.Fatal("goalInt64 accepted bad json.Number")
	}
	if parsed, err := goalInt64(int64(7)); err != nil || parsed != 7 {
		t.Fatalf("goalInt64 int64 parsed=%d err=%v", parsed, err)
	}
	if _, err := goalInt64(true); err == nil {
		t.Fatal("goalInt64 accepted bool")
	}
	if value, ok := goalInt64WithDefault(nil); ok || value != 0 {
		t.Fatalf("goalInt64WithDefault nil value=%d ok=%v", value, ok)
	}
	if value, ok := goalInt64WithDefault("bad"); ok || value != 0 {
		t.Fatalf("goalInt64WithDefault bad value=%d ok=%v", value, ok)
	}
	if clientSettableGoalStatus("blocked") || !readableGoalStatus("blocked") || readableGoalStatus("bogus") {
		t.Fatal("goal status helpers failed")
	}
}

func TestGoalRequestBuildersOptionsAndCapability(t *testing.T) {
	budget := int64(123)
	goal := CodexGoal{
		Objective:       " Ship it ",
		Status:          CodexGoalStatusPaused,
		TokenBudget:     &budget,
		TokensUsed:      10,
		TimeUsedSeconds: 11,
		ThreadID:        "codex-owned",
		Source:          CodexGoalSourceCodex,
	}

	req := NewSessionRequest("/repo", WithSessionGoal(goal))
	codexMeta := req.Meta[codexMetaKey].(map[string]any)
	goalMap := codexMeta[codexGoalMetaKey].(map[string]any)
	if goalMap[goalFieldObjective] != "Ship it" || goalMap[goalFieldTokenBudget] != budget {
		t.Fatalf("goal request meta = %#v", goalMap)
	}
	if _, ok := goalMap[goalFieldThreadID]; ok {
		t.Fatalf("client goal map included runtime fields: %#v", goalMap)
	}
	clearReq := LoadSessionRequest("session", "/repo", WithSessionGoalClear())
	if clearReq.Meta[codexMetaKey].(map[string]any)[codexGoalMetaKey] != nil {
		t.Fatalf("clear goal meta = %#v", clearReq.Meta)
	}
	if SetGoalRequest("session", goal)[codexGoalMetaKey].(map[string]any)[goalFieldStatus] != CodexGoalStatusPaused {
		t.Fatal("SetGoalRequest did not serialize status")
	}
	if ClearGoalRequest("session")[codexGoalMetaKey] != nil {
		t.Fatal("ClearGoalRequest did not serialize null")
	}

	options := applyOptions([]Option{WithCodexGoals(true)})
	if !options.EnableGoals {
		t.Fatal("WithCodexGoals did not set option")
	}
	agent := NewAgent(WithCodexGoals(true))
	init, err := agent.Initialize(context.Background(), acp.InitializeRequest{})
	if err != nil {
		t.Fatalf("Initialize returned error: %v", err)
	}
	goals := init.AgentCapabilities.Meta[codexMetaKey].(map[string]any)[codexGoalsCapabilityKey].(map[string]any)
	if goals["setMethod"] != codexSessionSetGoalMethod || goals["state"] != "session_info_update._meta.codex.goal" {
		t.Fatalf("goals capability = %#v", goals)
	}
	if _, ok := NewAgent().codexConfig()["features.goals"]; ok {
		t.Fatal("disabled goals produced config")
	}
	if NewAgent(WithCodexGoals(true)).codexConfig()["features.goals"] != true {
		t.Fatal("enabled goals config missing")
	}
}

func TestSetCodexGoalExtensionSetClearDedupAndSidecar(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	client := newSpyCodexClient()
	agent := NewAgent(WithCodexGoals(true), WithSessionStore(store), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return client, nil
	}))
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)

	session := newSession(agent, "session-1", "/tmp/project", nil, codex.Thread{ID: "thread-1"}, client, sessionMeta{})
	if err := agent.storeStartedSession(session); err != nil {
		t.Fatalf("store session: %v", err)
	}
	budget := int64(9000)
	resp, err := agent.HandleExtensionMethod(ctx, codexSessionSetGoalMethod, mustJSONRaw(SetGoalRequest(session.id, CodexGoal{
		Objective:   "Ship goals",
		Status:      CodexGoalStatusActive,
		TokenBudget: &budget,
	})))
	if err != nil {
		t.Fatalf("set goal extension returned error: %v", err)
	}
	goal := resp.(map[string]any)[codexGoalMetaKey].(map[string]any)
	if goal[goalFieldObjective] != "Ship goals" || goal[goalFieldSource] != CodexGoalSourceClient {
		t.Fatalf("set response goal = %#v", goal)
	}
	if len(conn.updates) != 1 || conn.updates[0].Update.SessionInfoUpdate == nil {
		t.Fatalf("updates = %#v", conn.updates)
	}
	if client.goalSet.Objective != "Ship goals" || client.goalSet.TokenBudget == nil || *client.goalSet.TokenBudget != budget {
		t.Fatalf("native goal set = %#v", client.goalSet)
	}

	projectKey, err := projectKeyForDirectory("/tmp/project")
	if err != nil {
		t.Fatalf("projectKeyForDirectory: %v", err)
	}
	sidecar, err := store.Load(ctx, SessionKey{ProjectKey: projectKey, SessionID: "session-1", Subpath: sessionStoreGoalSubpath})
	if err != nil || len(sidecar) != 1 || !strings.Contains(string(sidecar[0]), "Ship goals") {
		t.Fatalf("sidecar entries=%q err=%v", sidecar, err)
	}

	agent.updateGoalForClient(ctx, client, codex.Event{
		Kind:     codex.EventGoalUpdated,
		ThreadID: "thread-1",
		Goal:     &codex.Goal{ThreadID: "thread-1", Objective: "Ship goals", Status: CodexGoalStatusActive, TokenBudget: &budget},
	})
	if len(conn.updates) != 1 {
		t.Fatalf("duplicate native notification emitted update: %#v", conn.updates)
	}

	resp, err = agent.HandleExtensionMethod(ctx, codexSessionSetGoalMethod, mustJSONRaw(ClearGoalRequest(session.id)))
	if err != nil {
		t.Fatalf("clear goal extension returned error: %v", err)
	}
	if resp.(map[string]any)[codexGoalMetaKey] != nil {
		t.Fatalf("clear response = %#v", resp)
	}
	if client.goalClear != "thread-1" || len(conn.updates) != 2 {
		t.Fatalf("clear state goalClear=%q updates=%#v", client.goalClear, conn.updates)
	}
}

func TestSetCodexGoalExtensionValidationAndFailures(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	client := newSpyCodexClient()
	session := newSession(agent, "session-1", "/tmp/project", nil, codex.Thread{ID: "thread-1"}, client, sessionMeta{})
	agent.sessions[session.id] = session

	badRequests := []json.RawMessage{
		json.RawMessage(`{`),
		mustJSONRaw(map[string]any{codexGoalMetaKey: nil}),
		mustJSONRaw(map[string]any{jsonFieldSessionID: session.id}),
		json.RawMessage(`{"sessionId":"session-1","goal":`),
		mustJSONRaw(map[string]any{jsonFieldSessionID: session.id, codexGoalMetaKey: "bad"}),
		mustJSONRaw(map[string]any{jsonFieldSessionID: "missing", codexGoalMetaKey: nil}),
	}
	for _, raw := range badRequests {
		if _, err := agent.HandleExtensionMethod(ctx, codexSessionSetGoalMethod, raw); err == nil {
			t.Fatalf("HandleExtensionMethod accepted %s", raw)
		}
	}

	disabledAgent := NewAgent()
	disabledSession := newSession(disabledAgent, "session-disabled", "/tmp/project", nil, codex.Thread{ID: "thread-disabled"}, newSpyCodexClient(), sessionMeta{})
	disabledAgent.sessions[disabledSession.id] = disabledSession
	if _, err := disabledAgent.HandleExtensionMethod(ctx, codexSessionSetGoalMethod, mustJSONRaw(SetGoalRequest(disabledSession.id, CodexGoal{Objective: "Ship"}))); err == nil || !strings.Contains(err.Error(), "Codex goal support is not available") {
		t.Fatalf("disabled goal error = %v", err)
	}

	errConnAgent := NewAgent(WithCodexGoals(true))
	errConnAgent.setAgentClient(&errorAgentClient{recordingAgentClient: newRecordingAgentClient(), updateErr: errors.New("update failed")})
	errConnAgent.sessions[session.id] = session
	session.agent = errConnAgent
	if _, err := errConnAgent.HandleExtensionMethod(ctx, codexSessionSetGoalMethod, mustJSONRaw(SetGoalRequest(session.id, CodexGoal{Objective: "Ship"}))); err == nil {
		t.Fatal("set goal with update error succeeded")
	}

	unavailableClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("goals feature is disabled")}
	unavailableAgent := NewAgent(WithCodexGoals(true))
	unavailableSession := newSession(unavailableAgent, "session-2", "/tmp/project", nil, codex.Thread{ID: "thread-2"}, unavailableClient, sessionMeta{})
	unavailableAgent.sessions[unavailableSession.id] = unavailableSession
	if _, err := unavailableAgent.HandleExtensionMethod(ctx, codexSessionSetGoalMethod, mustJSONRaw(SetGoalRequest(unavailableSession.id, CodexGoal{Objective: "Ship"}))); err == nil || !strings.Contains(err.Error(), "Codex goal support is not available") {
		t.Fatalf("unavailable goal error = %v", err)
	}

	nativeErrorClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("native failed")}
	nativeErrorAgent := NewAgent(WithCodexGoals(true))
	nativeErrorSession := newSession(nativeErrorAgent, "session-3", "/tmp/project", nil, codex.Thread{ID: "thread-3"}, nativeErrorClient, sessionMeta{})
	nativeErrorAgent.sessions[nativeErrorSession.id] = nativeErrorSession
	if _, err := nativeErrorAgent.HandleExtensionMethod(ctx, codexSessionSetGoalMethod, mustJSONRaw(SetGoalRequest(nativeErrorSession.id, CodexGoal{Objective: "Ship"}))); err == nil || !strings.Contains(err.Error(), "native failed") {
		t.Fatalf("native goal error = %v", err)
	}
}

func TestGoalLifecycleAndLoadRestore(t *testing.T) {
	ctx := context.Background()
	client := newSpyCodexClient()
	var gotOptions codex.Options
	agent := NewAgent(WithCodexGoals(true), withClientFactory(func(_ context.Context, options codex.Options) (codex.Client, error) {
		gotOptions = options
		return client, nil
	}))
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)

	newResp, err := agent.NewSession(ctx, NewSessionRequest("/tmp/project", WithSessionGoal(CodexGoal{Objective: "Initial goal"})))
	if err != nil {
		t.Fatalf("NewSession returned error: %v", err)
	}
	if gotOptions.Config["features.goals"] != true || client.goalSet.Objective != "Initial goal" {
		t.Fatalf("goals config=%#v set=%#v", gotOptions.Config, client.goalSet)
	}
	if goal := newResp.Meta[codexMetaKey].(map[string]any)[codexGoalMetaKey].(map[string]any); goal[goalFieldObjective] != "Initial goal" {
		t.Fatalf("new session goal meta = %#v", goal)
	}
	list, err := agent.ListSessions(ctx, ListSessionsRequest())
	if err != nil || len(list.Sessions) != 1 {
		t.Fatalf("ListSessions = %#v err=%v", list, err)
	}
	if goal := list.Sessions[0].Meta[codexMetaKey].(map[string]any)[codexGoalMetaKey].(map[string]any); goal[goalFieldObjective] != "Initial goal" {
		t.Fatalf("list summary goal = %#v", goal)
	}

	if _, err := agent.LoadSession(ctx, LoadSessionRequest(newResp.SessionId, "/tmp/project", WithSessionGoalClear())); err != nil {
		t.Fatalf("LoadSession existing clear returned error: %v", err)
	}
	if client.goalClear != "thread-1" {
		t.Fatalf("existing load did not clear native goal: %q", client.goalClear)
	}

	store := NewInMemorySessionStore()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	if err != nil {
		t.Fatalf("projectKeyForDirectory: %v", err)
	}
	mainKey := SessionKey{ProjectKey: projectKey, SessionID: "stored-session"}
	if err := store.Append(ctx, mainKey, []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"agent_message","message":"hello"}}`)}); err != nil {
		t.Fatalf("append main: %v", err)
	}
	sidecar, err := goalSidecarEntry(canonicalGoalMeta(CodexGoal{Objective: "Stored goal", Status: CodexGoalStatusPaused, ThreadID: "stored-session", Source: CodexGoalSourceCodex}))
	if err != nil {
		t.Fatalf("goalSidecarEntry: %v", err)
	}
	if err := store.Replace(ctx, SessionKey{ProjectKey: projectKey, SessionID: "stored-session", Subpath: sessionStoreGoalSubpath}, []SessionStoreEntry{sidecar}); err != nil {
		t.Fatalf("replace sidecar: %v", err)
	}
	loadClient := newSpyCodexClient()
	loadAgent := NewAgent(WithCodexGoals(true), WithSessionStore(store), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return loadClient, nil
	}))
	loadConn := newRecordingAgentClient()
	loadAgent.setAgentClient(loadConn)
	loadResp, err := loadAgent.LoadSession(ctx, LoadSessionRequest("stored-session", cwd))
	if err != nil {
		t.Fatalf("LoadSession stored returned error: %v", err)
	}
	if loadClient.goalSet.Objective != "Stored goal" {
		t.Fatalf("stored load did not set native goal: %#v", loadClient.goalSet)
	}
	if goal := loadResp.Meta[codexMetaKey].(map[string]any)[codexGoalMetaKey].(map[string]any); goal[goalFieldObjective] != "Stored goal" {
		t.Fatalf("load response goal = %#v", goal)
	}
	if len(loadConn.updates) == 0 {
		t.Fatal("load did not emit replay/session updates")
	}

	resumeClient := newSpyCodexClient()
	resumeClient.goal = &codex.Goal{ThreadID: "resume-thread", Objective: "Native resume goal", Status: CodexGoalStatusComplete}
	resumeAgent := NewAgent(WithCodexGoals(true), withClientFactory(func(context.Context, codex.Options) (codex.Client, error) {
		return resumeClient, nil
	}))
	resumeResp, err := resumeAgent.ResumeSession(ctx, ResumeSessionRequest("resume-thread", "/tmp/project"))
	if err != nil {
		t.Fatalf("ResumeSession native goal returned error: %v", err)
	}
	if goal := resumeResp.Meta[codexMetaKey].(map[string]any)[codexGoalMetaKey].(map[string]any); goal[goalFieldObjective] != "Native resume goal" {
		t.Fatalf("resume native goal = %#v", goal)
	}
}

func TestGoalSidecarRolloutAndHelpers(t *testing.T) {
	sidecar, err := goalSidecarEntry(nil)
	if err != nil {
		t.Fatalf("goalSidecarEntry nil returned error: %v", err)
	}
	input, err := parseGoalSidecarEntry(sidecar)
	if err != nil || !input.present || !input.clear {
		t.Fatalf("parse clear sidecar input=%#v err=%v", input, err)
	}
	if _, err := parseGoalSidecarEntry(SessionStoreEntry(`bad`)); err == nil {
		t.Fatal("bad sidecar parsed")
	}
	if _, err := parseGoalSidecarEntry(SessionStoreEntry(`{"goal":{"status":"bad"}}`)); err == nil {
		t.Fatal("invalid sidecar goal parsed")
	}
	if _, err := parseGoalSidecarEntry(SessionStoreEntry(`{"goal":"bad"}`)); err == nil {
		t.Fatal("non-object sidecar goal parsed")
	}
	if _, err := parseGoalSidecarEntry(SessionStoreEntry(`{"goal":{"objective":"x","status":"bad"}}`)); err == nil {
		t.Fatal("invalid sidecar status parsed")
	}
	if input, err := parseGoalSidecarEntry(SessionStoreEntry(`{"other":true}`)); err != nil || input.present {
		t.Fatalf("sidecar without goal input=%#v err=%v", input, err)
	}
	if input, err := parseGoalSidecarEntry(SessionStoreEntry(`{"goal":{"objective":"defaults"}}`)); err != nil || !input.present || input.goal.Status != CodexGoalStatusActive {
		t.Fatalf("default sidecar goal input=%#v err=%v", input, err)
	}

	entries := make([]SessionStoreEntry, 0, 3)
	entries = append(entries,
		SessionStoreEntry(`not-json`),
		SessionStoreEntry(`{"type":"event_msg","payload":{"type":"thread_goal_updated","goal":{"objective":"Rollout goal","status":"blocked","tokenBudget":7,"tokensUsed":3,"timeUsedSeconds":4,"createdAt":5,"updatedAt":6,"threadId":"thread","source":"codex"}}}`),
	)
	input = goalFromRolloutEntries(entries)
	if !input.present || input.goal.Objective != "Rollout goal" || input.goal.Status != "blocked" || input.goal.TokensUsed != 3 {
		t.Fatalf("rollout goal input = %#v", input)
	}
	input = goalFromRolloutEntries(append(entries, SessionStoreEntry(`{"type":"event_msg","payload":{"type":"thread_goal_cleared"}}`)))
	if !input.present || !input.clear {
		t.Fatalf("rollout clear input = %#v", input)
	}
	if _, ok := rolloutGoalPayload(map[string]any{"goal": "bad"}); ok {
		t.Fatal("bad rollout goal parsed")
	}
	if _, err := parseStoredGoalObject(map[string]any{goalFieldObjective: "x", goalFieldStatus: true}); err == nil {
		t.Fatal("stored goal accepted non-string status")
	}
	if _, err := parseStoredGoalObject(map[string]any{goalFieldObjective: "x", goalFieldStatus: "weird"}); err == nil {
		t.Fatal("stored goal accepted unsupported status")
	}
	if _, err := parseStoredGoalObject(map[string]any{goalFieldObjective: "x", goalFieldTokenBudget: "bad"}); err == nil {
		t.Fatal("stored goal accepted bad token budget")
	}

	long := CodexGoal{Objective: strings.Repeat("x", maxGoalSummaryRunes+10), Status: CodexGoalStatusActive}
	if utf8Len := len([]rune(goalSummaryMeta(long)[goalFieldObjective].(string))); utf8Len > maxGoalSummaryRunes {
		t.Fatalf("summary length = %d", utf8Len)
	}
	summaryAgent := NewAgent()
	summarySession := newSession(summaryAgent, "summary", "/tmp/project", nil, codex.Thread{}, nil, sessionMeta{})
	if summarySession.goalSummaryMetaValue() != nil {
		t.Fatal("nil goal summary produced metadata")
	}
	summarySession.setGoalSnapshot(&long, false)
	if summarySession.goalSummaryMetaValue().(map[string]any)[goalFieldStatus] != CodexGoalStatusActive {
		t.Fatal("goal summary metadata missing status")
	}
	if cloneCodexGoalPtr(nil) != nil || nullableInt64Ptr(nil) != nil || cloneInt64Ptr(nil) != nil {
		t.Fatal("nil clone/nullable helpers failed")
	}
	if equalGoalSnapshot(nil, nil) != true || equalGoalSnapshot(&CodexGoal{Objective: "a"}, nil) {
		t.Fatal("equalGoalSnapshot nil branches failed")
	}
	a := int64(1)
	b := int64(2)
	if !equalInt64Ptr(&a, &a) || equalInt64Ptr(&a, &b) || equalInt64Ptr(&a, nil) {
		t.Fatal("equalInt64Ptr branches failed")
	}
	if !codexGoalUnavailable(errors.New("no such table: thread_goals")) || codexGoalUnavailable(errors.New("other")) {
		t.Fatal("codexGoalUnavailable failed")
	}
	if goalACPError(nil) != nil {
		t.Fatal("goalACPError nil failed")
	}
	threadBudget := int64(55)
	native := nativeGoalFromCodexGoal(CodexGoal{
		Objective:       "native",
		TokenBudget:     &threadBudget,
		TokensUsed:      1,
		TimeUsedSeconds: 2,
		CreatedAt:       3,
		UpdatedAt:       4,
	}, "thread-fallback")
	if native.ThreadID != "thread-fallback" || native.TokenBudget == nil || *native.TokenBudget != threadBudget {
		t.Fatalf("native goal = %#v", native)
	}
	if converted := codexGoalFromNative(codex.Goal{ThreadID: "thread", Objective: " native "}, CodexGoalSourceCodex); converted.Status != CodexGoalStatusActive || converted.Objective != "native" {
		t.Fatalf("converted goal = %#v", converted)
	}
	if _, err := goalSidecarEntry(map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("goalSidecarEntry accepted unmarshalable value")
	}
}

func TestSessionGoalStateBranches(t *testing.T) {
	ctx := context.Background()

	agent := NewAgent()
	session := newSession(agent, "local", "/tmp/project", nil, codex.Thread{}, nil, sessionMeta{})
	if err := session.applyClientGoalInput(ctx, goalMetaInput{}, true); err != nil {
		t.Fatalf("absent goal input returned error: %v", err)
	}
	if err := session.setGoal(ctx, CodexGoal{Objective: "local", Status: CodexGoalStatusActive}, false); err != nil {
		t.Fatalf("local set returned error: %v", err)
	}
	key, err := session.goalStoreKey()
	if err != nil {
		t.Fatalf("goalStoreKey returned error: %v", err)
	}
	if key.SessionID != "local" || key.Subpath != sessionStoreGoalSubpath {
		t.Fatalf("local goal store key = %#v", key)
	}
	if err := session.clearGoal(ctx, false); err != nil {
		t.Fatalf("local clear returned error: %v", err)
	}
	if err := session.clearGoal(ctx, true); err != nil {
		t.Fatalf("unchanged clear returned error: %v", err)
	}
	emitAgent := NewAgent()
	emitConn := newRecordingAgentClient()
	emitAgent.setAgentClient(emitConn)
	emitSession := newSession(emitAgent, "emit", "/tmp/project", nil, codex.Thread{}, nil, sessionMeta{})
	if err := emitSession.setGoal(ctx, CodexGoal{Objective: "emit", Status: CodexGoalStatusActive}, true); err != nil {
		t.Fatalf("local set with emit returned error: %v", err)
	}
	if len(emitConn.updates) != 1 {
		t.Fatalf("local set with emit updates = %#v", emitConn.updates)
	}

	storeAgent := NewAgent(WithSessionStore(NewInMemorySessionStore()))
	badCwd := newSession(storeAgent, "bad", "relative", nil, codex.Thread{}, nil, sessionMeta{})
	if err := badCwd.setGoal(ctx, CodexGoal{Objective: "bad", Status: CodexGoalStatusActive}, true); err == nil {
		t.Fatal("set goal with bad cwd succeeded")
	}
	badClear := newSession(storeAgent, "bad-clear", "relative", nil, codex.Thread{}, nil, sessionMeta{})
	badClear.setGoalSnapshot(&CodexGoal{Objective: "bad", Status: CodexGoalStatusActive}, false)
	if err := badClear.clearGoal(ctx, true); err == nil {
		t.Fatal("clear goal with bad cwd succeeded")
	}

	errClient := &errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("native failed")}
	native := newSession(NewAgent(), "native", "/tmp/project", nil, codex.Thread{ID: "thread-1"}, errClient, sessionMeta{})
	if err := native.setGoal(ctx, CodexGoal{Objective: "native", Status: CodexGoalStatusActive}, false); err == nil {
		t.Fatal("native set error was ignored")
	}
	if err := native.clearGoal(ctx, false); err == nil {
		t.Fatal("native clear error was ignored")
	}
	unchangedClient := newSpyCodexClient()
	unchangedSession := newSession(NewAgent(WithSessionStore(NewInMemorySessionStore())), "unchanged", "/tmp/project", nil, codex.Thread{ID: "thread-1"}, unchangedClient, sessionMeta{})
	unchangedSession.setGoalSnapshot(&CodexGoal{Objective: "same", Status: CodexGoalStatusActive, ThreadID: "thread-1"}, false)
	if err := unchangedSession.setGoal(ctx, CodexGoal{Objective: "same", Status: CodexGoalStatusActive}, true); err != nil {
		t.Fatalf("unchanged native set returned error: %v", err)
	}
	nativeBadCwdClient := newSpyCodexClient()
	nativeBadCwd := newSession(storeAgent, "native-bad-cwd", "relative", nil, codex.Thread{ID: "thread-native-bad"}, nativeBadCwdClient, sessionMeta{})
	if err := nativeBadCwd.setGoal(ctx, CodexGoal{Objective: "bad", Status: CodexGoalStatusActive}, true); err == nil {
		t.Fatal("native set with bad cwd succeeded")
	}

	updateAgent := NewAgent(WithSessionStore(NewInMemorySessionStore()))
	updateConn := newRecordingAgentClient()
	updateAgent.setAgentClient(updateConn)
	updateSession := newSession(updateAgent, "update", "/tmp/project", nil, codex.Thread{ID: "thread-update"}, newSpyCodexClient(), sessionMeta{})
	if err := updateSession.applyCodexGoalEvent(ctx, codex.Event{Kind: codex.EventRaw}, true); err != nil {
		t.Fatalf("raw goal event returned error: %v", err)
	}
	if err := updateSession.applyCodexGoalEvent(ctx, codex.Event{Kind: codex.EventGoalUpdated}, true); err != nil {
		t.Fatalf("nil goal update returned error: %v", err)
	}
	if err := updateSession.applyCodexGoalEvent(ctx, codex.Event{Kind: codex.EventGoalUpdated, Goal: &codex.Goal{Objective: "   "}}, true); err != nil {
		t.Fatalf("blank goal update returned error: %v", err)
	}
	if err := updateSession.applyCodexGoalEvent(ctx, codex.Event{Kind: codex.EventGoalCleared}, true); err != nil {
		t.Fatalf("unchanged goal clear returned error: %v", err)
	}
	if err := updateSession.applyCodexGoalEvent(ctx, codex.Event{Kind: codex.EventGoalUpdated, ThreadID: "thread-update", Goal: &codex.Goal{ThreadID: "thread-update", Objective: "from codex"}}, false); err != nil {
		t.Fatalf("goal update without emit returned error: %v", err)
	}
	if len(updateConn.updates) != 0 {
		t.Fatalf("emit=false produced updates: %#v", updateConn.updates)
	}
	if err := updateSession.applyCodexGoalEvent(ctx, codex.Event{Kind: codex.EventGoalUpdated, ThreadID: "thread-update", Goal: &codex.Goal{ThreadID: "thread-update", Objective: "new from codex"}}, true); err != nil {
		t.Fatalf("goal update with emit returned error: %v", err)
	}
	if len(updateConn.updates) != 1 {
		t.Fatalf("goal update with emit updates = %#v", updateConn.updates)
	}

	badEventSession := newSession(storeAgent, "bad-event", "relative", nil, codex.Thread{ID: "thread-bad"}, newSpyCodexClient(), sessionMeta{})
	if err := badEventSession.applyCodexGoalEvent(ctx, codex.Event{Kind: codex.EventGoalUpdated, Goal: &codex.Goal{Objective: "bad event"}}, true); err == nil {
		t.Fatal("goal event with bad cwd succeeded")
	}

	noStoreInput, err := (&Session{agent: &Agent{}, cwd: "/tmp/project"}).loadStoredGoalSnapshot(ctx)
	if err != nil || noStoreInput.present {
		t.Fatalf("nil store snapshot input=%#v err=%v", noStoreInput, err)
	}
	badSnapshot := newSession(NewAgent(), "bad-snapshot", "relative", nil, codex.Thread{}, nil, sessionMeta{})
	if _, err := badSnapshot.loadStoredGoalSnapshot(ctx); err == nil {
		t.Fatal("load stored goal with bad cwd succeeded")
	}
}

func TestNativeGoalRefreshBranches(t *testing.T) {
	ctx := context.Background()

	store := NewInMemorySessionStore()
	agent := NewAgent(WithCodexGoals(true), WithSessionStore(store))
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	client := newSpyCodexClient()
	client.goal = &codex.Goal{ThreadID: "thread-refresh", Objective: "Native refresh", Status: CodexGoalStatusComplete}
	session := newSession(agent, "refresh", "/tmp/project", nil, codex.Thread{ID: "thread-refresh"}, client, sessionMeta{})
	if err := session.refreshNativeGoal(ctx, true); err != nil {
		t.Fatalf("refresh native goal returned error: %v", err)
	}
	if goal := session.goalMetaValue().(map[string]any); goal[goalFieldObjective] != "Native refresh" {
		t.Fatalf("refreshed goal = %#v", goal)
	}
	if len(conn.updates) != 1 {
		t.Fatalf("refresh updates = %#v", conn.updates)
	}

	unchangedClient := newSpyCodexClient()
	unchangedClient.goal = &codex.Goal{ThreadID: "thread-same", Objective: "Same", Status: CodexGoalStatusActive}
	unchangedSession := newSession(agent, "same", "/tmp/project", nil, codex.Thread{ID: "thread-same"}, unchangedClient, sessionMeta{})
	unchangedSession.setGoalSnapshot(&CodexGoal{ThreadID: "thread-same", Objective: "Same", Status: CodexGoalStatusActive}, false)
	if err := unchangedSession.refreshNativeGoal(ctx, true); err != nil {
		t.Fatalf("unchanged refresh returned error: %v", err)
	}

	blankClient := newSpyCodexClient()
	blankClient.goal = &codex.Goal{ThreadID: "thread-blank", Objective: "  "}
	blankSession := newSession(agent, "blank", "/tmp/project", nil, codex.Thread{ID: "thread-blank"}, blankClient, sessionMeta{})
	blankSession.setGoalSnapshot(&CodexGoal{Objective: "old", Status: CodexGoalStatusActive}, false)
	if err := blankSession.refreshNativeGoal(ctx, false); err != nil {
		t.Fatalf("blank refresh returned error: %v", err)
	}
	if blankSession.goalMetaValue() != nil {
		t.Fatalf("blank native goal did not clear snapshot: %#v", blankSession.goalMetaValue())
	}

	if err := newSession(agent, "empty-thread", "/tmp/project", nil, codex.Thread{}, client, sessionMeta{}).refreshNativeGoal(ctx, true); err != nil {
		t.Fatalf("empty thread refresh returned error: %v", err)
	}
	if err := newSession(NewAgent(), "disabled", "/tmp/project", nil, codex.Thread{ID: "thread-disabled"}, client, sessionMeta{}).refreshNativeGoal(ctx, true); err != nil {
		t.Fatalf("disabled refresh returned error: %v", err)
	}

	badCwdClient := newSpyCodexClient()
	badCwdClient.goal = &codex.Goal{ThreadID: "thread-bad-cwd", Objective: "Bad cwd", Status: CodexGoalStatusActive}
	badCwdSession := newSession(agent, "bad-cwd", "relative", nil, codex.Thread{ID: "thread-bad-cwd"}, badCwdClient, sessionMeta{})
	if err := badCwdSession.refreshNativeGoal(ctx, false); err == nil {
		t.Fatal("bad cwd refresh succeeded")
	}

	unavailable := newSession(
		NewAgent(WithCodexGoals(true)),
		"unavailable",
		"/tmp/project",
		nil,
		codex.Thread{ID: "thread-unavailable"},
		&errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("no such table: thread_goals")},
		sessionMeta{},
	)
	if err := unavailable.refreshNativeGoal(ctx, true); err != nil {
		t.Fatalf("unavailable refresh returned error: %v", err)
	}

	nativeErr := newSession(
		NewAgent(WithCodexGoals(true)),
		"native-error",
		"/tmp/project",
		nil,
		codex.Thread{ID: "thread-native-error"},
		&errorCodexClient{spyCodexClient: newSpyCodexClient(), goalErr: errors.New("native failed")},
		sessionMeta{},
	)
	if err := nativeErr.refreshNativeGoal(ctx, true); err != nil {
		t.Fatalf("native error refresh returned error: %v", err)
	}
}

func TestRestoreGoalForLoadBranches(t *testing.T) {
	ctx := context.Background()

	noGoalAgent := NewAgent()
	noGoalConn := newRecordingAgentClient()
	noGoalAgent.setAgentClient(noGoalConn)
	noGoalSession := newSession(noGoalAgent, "none", "/tmp/project", nil, codex.Thread{ID: "thread-none"}, newSpyCodexClient(), sessionMeta{})
	if err := noGoalSession.restoreGoalForLoad(ctx, nil, goalMetaInput{}); err != nil {
		t.Fatalf("restore without goal returned error: %v", err)
	}
	if len(noGoalConn.updates) != 0 {
		t.Fatalf("restore without goal emitted updates: %#v", noGoalConn.updates)
	}

	loadErrSession := newSession(NewAgent(WithSessionStore(loadErrorStore{})), "load-error", "/tmp/project", nil, codex.Thread{ID: "thread-load-error"}, newSpyCodexClient(), sessionMeta{})
	if err := loadErrSession.restoreGoalForLoad(ctx, nil, goalMetaInput{}); err == nil {
		t.Fatal("restore with sidecar load error succeeded")
	}

	rolloutClient := newSpyCodexClient()
	rolloutAgent := NewAgent(WithCodexGoals(true))
	rolloutAgent.setAgentClient(newRecordingAgentClient())
	rolloutSession := newSession(rolloutAgent, "rollout", "/tmp/project", nil, codex.Thread{ID: "thread-rollout"}, rolloutClient, sessionMeta{})
	rolloutRows := []SessionStoreEntry{SessionStoreEntry(`{"type":"event_msg","payload":{"type":"thread_goal_updated","goal":{"objective":"Rollout restore","status":"active"}}}`)}
	if err := rolloutSession.restoreGoalForLoad(ctx, rolloutRows, goalMetaInput{}); err != nil {
		t.Fatalf("rollout restore returned error: %v", err)
	}
	if rolloutClient.goalSet.Objective != "Rollout restore" {
		t.Fatalf("rollout restore did not push native goal: %#v", rolloutClient.goalSet)
	}

	store := NewInMemorySessionStore()
	cwd := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	if err != nil {
		t.Fatalf("project key: %v", err)
	}
	sidecar, err := goalSidecarEntry(canonicalGoalMeta(CodexGoal{Objective: "Stored", Status: CodexGoalStatusActive}))
	if err != nil {
		t.Fatalf("goalSidecarEntry returned error: %v", err)
	}
	if err := store.Replace(ctx, SessionKey{ProjectKey: projectKey, SessionID: "clear", Subpath: sessionStoreGoalSubpath}, []SessionStoreEntry{sidecar}); err != nil {
		t.Fatalf("replace sidecar returned error: %v", err)
	}
	clearClient := newSpyCodexClient()
	clearClient.goal = &codex.Goal{ThreadID: "thread-clear", Objective: "Stored", Status: CodexGoalStatusActive}
	clearAgent := NewAgent(WithCodexGoals(true), WithSessionStore(store))
	clearAgent.setAgentClient(newRecordingAgentClient())
	clearSession := newSession(clearAgent, "clear", cwd, nil, codex.Thread{ID: "thread-clear"}, clearClient, sessionMeta{})
	if err := clearSession.restoreGoalForLoad(ctx, nil, goalMetaInput{present: true, clear: true}); err != nil {
		t.Fatalf("lifecycle clear restore returned error: %v", err)
	}
	if clearClient.goalClear != "thread-clear" {
		t.Fatalf("lifecycle clear did not clear native goal: %q", clearClient.goalClear)
	}
}
