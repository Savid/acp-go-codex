package codexacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

const (
	CodexGoalStatusActive        = "active"
	CodexGoalStatusPaused        = "paused"
	CodexGoalStatusBudgetLimited = "budgetLimited"
	CodexGoalStatusComplete      = "complete"

	CodexGoalSourceClient = "client"
	CodexGoalSourceCodex  = "codex"

	codexGoalsCapabilityKey = "goals"
	codexGoalMetaKey        = "goal"

	codexSessionSetGoalMethod = "_codex/session/setGoal"

	goalFieldCreatedAt       = "createdAt"
	goalFieldObjective       = "objective"
	goalFieldSource          = "source"
	goalFieldStatus          = "status"
	goalFieldThreadID        = "threadId"
	goalFieldTimeUsedSeconds = "timeUsedSeconds"
	goalFieldTokenBudget     = "tokenBudget"
	goalFieldTokensUsed      = "tokensUsed"
	goalFieldUpdatedAt       = "updatedAt"

	maxGoalObjectiveBytes = 4096
	maxGoalSummaryRunes   = 256

	goalCapabilityStateKey = "state"

	sessionStoreGoalSubpath = "goal.json"
)

var errCodexGoalsDisabled = errors.New("goals feature is disabled")

// CodexGoal is the Codex-specific session goal metadata shape used in
// _meta.codex.goal and _codex/session/setGoal requests.
type CodexGoal struct {
	Objective       string `json:"objective"`
	Status          string `json:"status,omitempty"`
	TokenBudget     *int64 `json:"tokenBudget,omitempty"`
	TokensUsed      int64  `json:"tokensUsed,omitempty"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds,omitempty"`
	CreatedAt       int64  `json:"createdAt,omitempty"`
	UpdatedAt       int64  `json:"updatedAt,omitempty"`
	ThreadID        string `json:"threadId,omitempty"`
	Source          string `json:"source,omitempty"`
}

type goalMetaInput struct {
	present bool
	clear   bool
	goal    CodexGoal
}

func parseGoalFromMeta(meta map[string]any) (goalMetaInput, error) {
	codexMeta, _ := meta[codexMetaKey].(map[string]any)
	if codexMeta == nil {
		return goalMetaInput{}, nil
	}

	raw, ok := codexMeta[codexGoalMetaKey]
	if !ok {
		return goalMetaInput{}, nil
	}

	return parseGoalValue(raw)
}

func parseGoalValue(raw any) (goalMetaInput, error) {
	if raw == nil {
		return goalMetaInput{present: true, clear: true}, nil
	}

	rawMap, _ := raw.(map[string]any)
	if rawMap == nil {
		return goalMetaInput{}, fmt.Errorf("_meta.%s.%s must be null or an object", codexMetaKey, codexGoalMetaKey)
	}

	goal, err := parseClientGoalObject(rawMap)
	if err != nil {
		return goalMetaInput{}, err
	}

	return goalMetaInput{present: true, goal: goal}, nil
}

func parseClientGoalObject(raw map[string]any) (CodexGoal, error) {
	for key := range raw {
		switch key {
		case goalFieldObjective,
			goalFieldStatus,
			goalFieldTokenBudget,
			goalFieldTokensUsed,
			goalFieldTimeUsedSeconds,
			goalFieldCreatedAt,
			goalFieldUpdatedAt,
			goalFieldThreadID,
			goalFieldSource:
		default:
			return CodexGoal{}, fmt.Errorf("_meta.%s.%s.%s is not supported", codexMetaKey, codexGoalMetaKey, key)
		}
	}

	objective, err := requiredGoalString(raw, goalFieldObjective, maxGoalObjectiveBytes)
	if err != nil {
		return CodexGoal{}, err
	}
	status, err := optionalGoalStatus(raw)
	if err != nil {
		return CodexGoal{}, err
	}
	if status == "" {
		status = CodexGoalStatusActive
	}
	if !clientSettableGoalStatus(status) {
		return CodexGoal{}, fmt.Errorf("_meta.%s.%s.%s is unsupported: %s", codexMetaKey, codexGoalMetaKey, goalFieldStatus, status)
	}
	tokenBudget, err := optionalGoalInt64(raw, goalFieldTokenBudget)
	if err != nil {
		return CodexGoal{}, err
	}
	if tokenBudget != nil && *tokenBudget < 0 {
		return CodexGoal{}, fmt.Errorf("_meta.%s.%s.%s must be non-negative", codexMetaKey, codexGoalMetaKey, goalFieldTokenBudget)
	}

	return CodexGoal{
		Objective:   objective,
		Status:      status,
		TokenBudget: tokenBudget,
	}, nil
}

func requiredGoalString(raw map[string]any, key string, limit int) (string, error) {
	value, ok := raw[key]
	if !ok || value == nil {
		return "", fmt.Errorf("_meta.%s.%s.%s is required", codexMetaKey, codexGoalMetaKey, key)
	}

	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("_meta.%s.%s.%s must be a string", codexMetaKey, codexGoalMetaKey, key)
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("_meta.%s.%s.%s is required", codexMetaKey, codexGoalMetaKey, key)
	}
	if len(text) > limit {
		return "", fmt.Errorf("_meta.%s.%s.%s must be at most %d bytes", codexMetaKey, codexGoalMetaKey, key, limit)
	}

	return text, nil
}

func optionalGoalStatus(raw map[string]any) (string, error) {
	value, ok := raw[goalFieldStatus]
	if !ok || value == nil {
		return "", nil
	}

	status, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("_meta.%s.%s.%s must be a string or null", codexMetaKey, codexGoalMetaKey, goalFieldStatus)
	}

	return strings.TrimSpace(status), nil
}

func optionalGoalInt64(raw map[string]any, key string) (*int64, error) {
	value, ok := raw[key]
	if !ok || value == nil {
		//nolint:nilnil // nil pointer with nil error represents an absent optional integer.
		return nil, nil
	}
	parsed, err := goalInt64(value)
	if err != nil {
		return nil, fmt.Errorf("_meta.%s.%s.%s must be an integer or null", codexMetaKey, codexGoalMetaKey, key)
	}

	return &parsed, nil
}

func goalInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case int:
		return int64(typed), nil
	case int64:
		return typed, nil
	case float64:
		if typed != float64(int64(typed)) {
			return 0, fmt.Errorf("not an integer")
		}
		return int64(typed), nil
	case json.Number:
		return typed.Int64()
	default:
		return 0, fmt.Errorf("not an integer")
	}
}

func clientSettableGoalStatus(status string) bool {
	switch status {
	case CodexGoalStatusActive, CodexGoalStatusPaused, CodexGoalStatusBudgetLimited, CodexGoalStatusComplete:
		return true
	default:
		return false
	}
}

func readableGoalStatus(status string) bool {
	switch status {
	case CodexGoalStatusActive, CodexGoalStatusPaused, CodexGoalStatusBudgetLimited, CodexGoalStatusComplete,
		"blocked", "usage_limited", "budget_limited":
		return true
	default:
		return false
	}
}

func codexGoalsCapability() map[string]any {
	statuses := []string{CodexGoalStatusActive, CodexGoalStatusPaused, CodexGoalStatusBudgetLimited, CodexGoalStatusComplete}

	return map[string]any{
		capabilityScopeKey:     capabilityScopeSession,
		goalCapabilityStateKey: "session_info_update._meta.codex.goal",
		"initialState": map[string]any{
			"sessionResponses": []string{
				"session/new.result._meta.codex.goal",
				"session/load.result._meta.codex.goal",
				"session/resume.result._meta.codex.goal",
			},
			"listSummary": "session/list.result.sessions[]._meta.codex.goal",
		},
		"setMethod":              codexSessionSetGoalMethod,
		"semantics":              "full-snapshot",
		"maxObjectiveBytes":      maxGoalObjectiveBytes,
		"maxSummaryRunes":        maxGoalSummaryRunes,
		"statuses":               append([]string(nil), statuses...),
		"clientSettableStatuses": append([]string(nil), statuses...),
		"clearValue":             nil,
	}
}

func (a *Agent) setCodexGoal(ctx context.Context, params json.RawMessage) (any, error) {
	var request struct {
		SessionID acp.SessionId   `json:"sessionId"`
		Goal      json.RawMessage `json:"goal"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}
	if request.SessionID == "" {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldSessionID: validationRequired})
	}
	if request.Goal == nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: "goal is required"})
	}

	input, err := parseGoalRaw(request.Goal)
	if err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	session, err := a.session(request.SessionID)
	if err != nil {
		return nil, err
	}
	if err := session.applyClientGoalInput(ctx, input, true); err != nil {
		return nil, goalACPError(err)
	}

	return map[string]any{codexGoalMetaKey: session.goalMetaValue()}, nil
}

func parseGoalRaw(rawGoal json.RawMessage) (goalMetaInput, error) {
	var raw any
	decoder := json.NewDecoder(bytes.NewReader(rawGoal))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return goalMetaInput{}, err
	}

	return parseGoalValue(raw)
}

func (s *Session) applyClientGoalInput(ctx context.Context, input goalMetaInput, emit bool) error {
	if !input.present {
		return nil
	}
	if !s.agent.options.EnableGoals {
		return errCodexGoalsDisabled
	}

	if input.clear {
		return s.clearGoal(ctx, emit)
	}

	return s.setGoal(ctx, input.goal, emit)
}

func (s *Session) applyLifecycleOrNativeGoal(ctx context.Context, input goalMetaInput, emit bool) error {
	if input.present {
		return s.applyClientGoalInput(ctx, input, emit)
	}

	return s.refreshNativeGoal(ctx, emit)
}

func (s *Session) setGoal(ctx context.Context, goal CodexGoal, emit bool) error {
	threadID := s.codexThreadIDSnapshot()
	if threadID == "" || s.client == nil {
		goal.ThreadID = threadID
		goal.Source = CodexGoalSourceClient
		changed := s.setGoalSnapshot(&goal, true)
		if changed && emit {
			if err := s.persistGoalSidecar(ctx); err != nil {
				return err
			}
			return s.emitGoalInfoUpdate(ctx)
		}
		return nil
	}

	native, err := s.client.SetGoal(ctx, codex.GoalSetRequest{
		ThreadID:    threadID,
		Objective:   goal.Objective,
		Status:      goal.Status,
		TokenBudget: cloneInt64Ptr(goal.TokenBudget),
	})
	if err != nil {
		return codexThreadACPError(err, s.accountMetaSnapshot(), codexThreadErrorData(s.id, threadID))
	}

	next := codexGoalFromNative(native, CodexGoalSourceClient)
	changed := s.setGoalSnapshot(&next, true)
	if !changed {
		return nil
	}
	if err := s.persistGoalSidecar(ctx); err != nil {
		return err
	}
	if emit {
		return s.emitGoalInfoUpdate(ctx)
	}

	return nil
}

func (s *Session) clearGoal(ctx context.Context, emit bool) error {
	threadID := s.codexThreadIDSnapshot()
	if threadID != "" && s.client != nil {
		if _, err := s.client.ClearGoal(ctx, threadID); err != nil {
			return codexThreadACPError(err, s.accountMetaSnapshot(), codexThreadErrorData(s.id, threadID))
		}
	}

	changed := s.setGoalSnapshot(nil, true)
	if !changed {
		return nil
	}
	if err := s.persistGoalSidecar(ctx); err != nil {
		return err
	}
	if emit {
		return s.emitGoalInfoUpdate(ctx)
	}

	return nil
}

func (s *Session) applyCodexGoalEvent(ctx context.Context, event codex.Event, emit bool) error {
	var changed bool
	switch event.Kind {
	case codex.EventGoalUpdated:
		if event.Goal == nil || strings.TrimSpace(event.Goal.Objective) == "" {
			return nil
		}
		goal := codexGoalFromNative(*event.Goal, CodexGoalSourceCodex)
		changed = s.setGoalSnapshot(&goal, true)
	case codex.EventGoalCleared:
		changed = s.setGoalSnapshot(nil, true)
	default:
		return nil
	}
	if !changed {
		return nil
	}
	if err := s.persistGoalSidecar(ctx); err != nil {
		return err
	}
	if emit {
		return s.emitGoalInfoUpdate(ctx)
	}

	return nil
}

func (s *Session) restoreGoalForLoad(ctx context.Context, rolloutEntries []SessionStoreEntry, lifecycle goalMetaInput) error {
	stored, err := s.loadStoredGoalSnapshot(ctx)
	if err != nil {
		return err
	}
	if !stored.present {
		stored = goalFromRolloutEntries(rolloutEntries)
	}
	input := stored
	if lifecycle.present {
		input = lifecycle
	}
	if !input.present {
		return s.refreshNativeGoal(ctx, true)
	}
	if err := s.applyClientGoalInput(ctx, input, false); err != nil {
		return err
	}

	return s.emitGoalInfoUpdate(ctx)
}

func (s *Session) refreshNativeGoal(ctx context.Context, emit bool) error {
	if !s.agent.options.EnableGoals || s.client == nil {
		return nil
	}
	threadID := s.codexThreadIDSnapshot()
	if threadID == "" {
		return nil
	}

	native, err := s.client.GetGoal(ctx, threadID)
	if err != nil {
		if codexGoalUnavailable(err) {
			return nil
		}
		s.agent.log.DebugContext(ctx, "refresh Codex goal failed", slog.String("error", err.Error()))

		return nil
	}

	var goal *CodexGoal
	if native != nil && strings.TrimSpace(native.Objective) != "" {
		converted := codexGoalFromNative(*native, CodexGoalSourceCodex)
		goal = &converted
	}
	changed := s.setGoalSnapshot(goal, true)
	if !changed {
		return nil
	}
	if err := s.persistGoalSidecar(ctx); err != nil {
		return err
	}
	if emit {
		return s.emitGoalInfoUpdate(ctx)
	}

	return nil
}

func (s *Session) loadStoredGoalSnapshot(ctx context.Context) (goalMetaInput, error) {
	store := s.agent.sessionStore()
	if store == nil {
		return goalMetaInput{}, nil
	}
	key, err := s.goalStoreKey()
	if err != nil {
		return goalMetaInput{}, err
	}
	entries, err := store.Load(ctx, key)
	if err != nil {
		return goalMetaInput{}, err
	}
	if len(entries) == 0 {
		return goalMetaInput{}, nil
	}

	return parseGoalSidecarEntry(entries[len(entries)-1])
}

func (s *Session) persistGoalSidecar(ctx context.Context) error {
	store := s.agent.options.SessionStore
	if store == nil {
		return nil
	}
	key, err := s.goalStoreKey()
	if err != nil {
		return err
	}
	entry, _ := goalSidecarEntry(s.goalMetaValue())

	return store.Replace(ctx, key, []SessionStoreEntry{entry})
}

func (s *Session) goalStoreKey() (SessionKey, error) {
	projectKey, err := projectKeyForDirectory(s.cwd)
	if err != nil {
		return SessionKey{}, err
	}

	return SessionKey{
		ProjectKey: projectKey,
		SessionID:  string(s.id),
		Subpath:    sessionStoreGoalSubpath,
	}, nil
}

func goalSidecarEntry(goal any) (SessionStoreEntry, error) {
	raw, err := json.Marshal(map[string]any{codexGoalMetaKey: goal})
	if err != nil {
		return nil, err
	}

	return SessionStoreEntry(raw), nil
}

func parseGoalSidecarEntry(entry SessionStoreEntry) (goalMetaInput, error) {
	var sidecar map[string]json.RawMessage
	if err := json.Unmarshal(entry, &sidecar); err != nil {
		return goalMetaInput{}, err
	}
	raw, ok := sidecar[codexGoalMetaKey]
	if !ok {
		return goalMetaInput{}, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return goalMetaInput{present: true, clear: true}, nil
	}
	var goal CodexGoal
	if err := json.Unmarshal(raw, &goal); err != nil {
		return goalMetaInput{}, err
	}
	goal.Objective = strings.TrimSpace(goal.Objective)
	if goal.Objective == "" {
		return goalMetaInput{}, fmt.Errorf("%s.%s is required", sessionStoreGoalSubpath, goalFieldObjective)
	}
	if goal.Status == "" {
		goal.Status = CodexGoalStatusActive
	}
	if !readableGoalStatus(goal.Status) {
		return goalMetaInput{}, fmt.Errorf("%s.%s is unsupported: %s", sessionStoreGoalSubpath, goalFieldStatus, goal.Status)
	}

	return goalMetaInput{present: true, goal: goal}, nil
}

func goalFromRolloutEntries(entries []SessionStoreEntry) goalMetaInput {
	accumulator := goalMetaInput{}
	for _, entry := range entries {
		row, err := decodeRolloutRow(entry)
		if err != nil || row.Type != "event_msg" {
			continue
		}
		switch stringFromAny(row.Payload["type"]) {
		case "thread_goal_updated":
			if goal, ok := rolloutGoalPayload(row.Payload); ok {
				accumulator = goalMetaInput{present: true, goal: goal}
			}
		case "thread_goal_cleared":
			accumulator = goalMetaInput{present: true, clear: true}
		}
	}

	return accumulator
}

func rolloutGoalPayload(payload map[string]any) (CodexGoal, bool) {
	rawGoal, _ := payload[codexGoalMetaKey].(map[string]any)
	if rawGoal == nil {
		rawGoal = payload
	}
	goal, err := parseStoredGoalObject(rawGoal)
	if err != nil {
		return CodexGoal{}, false
	}

	return goal, true
}

func parseStoredGoalObject(raw map[string]any) (CodexGoal, error) {
	objective, err := requiredGoalString(raw, goalFieldObjective, maxGoalObjectiveBytes)
	if err != nil {
		return CodexGoal{}, err
	}
	status, err := optionalGoalStatus(raw)
	if err != nil {
		return CodexGoal{}, err
	}
	if status == "" {
		status = CodexGoalStatusActive
	}
	if !readableGoalStatus(status) {
		return CodexGoal{}, fmt.Errorf("_meta.%s.%s.%s is unsupported: %s", codexMetaKey, codexGoalMetaKey, goalFieldStatus, status)
	}
	tokenBudget, err := optionalGoalInt64(raw, goalFieldTokenBudget)
	if err != nil {
		return CodexGoal{}, err
	}
	tokensUsed, _ := goalInt64WithDefault(raw[goalFieldTokensUsed])
	timeUsed, _ := goalInt64WithDefault(raw[goalFieldTimeUsedSeconds])
	createdAt, _ := goalInt64WithDefault(raw[goalFieldCreatedAt])
	updatedAt, _ := goalInt64WithDefault(raw[goalFieldUpdatedAt])

	return CodexGoal{
		Objective:       objective,
		Status:          status,
		TokenBudget:     tokenBudget,
		TokensUsed:      tokensUsed,
		TimeUsedSeconds: timeUsed,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
		ThreadID:        strings.TrimSpace(stringFromAny(raw[goalFieldThreadID])),
		Source:          strings.TrimSpace(stringFromAny(raw[goalFieldSource])),
	}, nil
}

func goalInt64WithDefault(value any) (int64, bool) {
	if value == nil {
		return 0, false
	}
	parsed, err := goalInt64(value)
	if err != nil {
		return 0, false
	}

	return parsed, true
}

func (a *Agent) updateGoalForClient(ctx context.Context, client codex.Client, event codex.Event) {
	a.mu.Lock()
	sessions := make([]*Session, 0, len(a.sessions))
	for _, session := range a.sessions {
		if session.client != client {
			continue
		}
		if event.ThreadID != "" && session.codexThreadID != event.ThreadID {
			continue
		}
		sessions = append(sessions, session)
	}
	a.mu.Unlock()

	for _, session := range sessions {
		if err := session.applyCodexGoalEvent(ctx, event, true); err != nil {
			a.log.DebugContext(ctx, "apply Codex goal event failed", slog.String("error", err.Error()))
		}
	}
}

func (s *Session) setGoalSnapshot(goal *CodexGoal, bumpRevision bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if equalGoalSnapshot(s.goal, goal) {
		return false
	}
	if goal == nil {
		s.goal = nil
	} else {
		cloned := cloneCodexGoal(*goal)
		s.goal = &cloned
	}
	if bumpRevision {
		s.goalRevision++
	}

	return true
}

func (s *Session) goalSnapshot() (*CodexGoal, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.goal == nil {
		return nil, s.goalRevision
	}
	goal := cloneCodexGoal(*s.goal)

	return &goal, s.goalRevision
}

func (s *Session) codexThreadIDSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.codexThreadID
}

func (s *Session) goalMetaValue() any {
	goal, _ := s.goalSnapshot()
	if goal == nil {
		return nil
	}

	return canonicalGoalMeta(*goal)
}

func (s *Session) goalSummaryMetaValue() any {
	goal, _ := s.goalSnapshot()
	if goal == nil {
		return nil
	}

	return goalSummaryMeta(*goal)
}

func canonicalGoalMeta(goal CodexGoal) map[string]any {
	return map[string]any{
		goalFieldObjective:       goal.Objective,
		goalFieldStatus:          goal.Status,
		goalFieldTokenBudget:     nullableInt64Ptr(goal.TokenBudget),
		goalFieldTokensUsed:      goal.TokensUsed,
		goalFieldTimeUsedSeconds: goal.TimeUsedSeconds,
		goalFieldCreatedAt:       goal.CreatedAt,
		goalFieldUpdatedAt:       goal.UpdatedAt,
		goalFieldThreadID:        nullableString(goal.ThreadID),
		goalFieldSource:          nullableString(goal.Source),
	}
}

func goalSummaryMeta(goal CodexGoal) map[string]any {
	objective := goal.Objective
	if utf8.RuneCountInString(objective) > maxGoalSummaryRunes {
		runes := []rune(objective)
		objective = strings.TrimSpace(string(runes[:maxGoalSummaryRunes-3])) + "..."
	}

	return map[string]any{
		goalFieldObjective: objective,
		goalFieldStatus:    goal.Status,
	}
}

func (s *Session) emitGoalInfoUpdate(ctx context.Context) error {
	goal := s.goalMetaValue()

	return s.emitUpdates(ctx, acp.SessionUpdate{
		SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
			Meta: goalInfoMeta(goal),
		},
	})
}

func goalInfoMeta(goal any) map[string]any {
	codexMeta := map[string]any{codexGoalMetaKey: goal}

	return map[string]any{
		codexMetaKey:   codexMeta,
		packageMetaKey: cloneAnyMap(codexMeta),
	}
}

func codexGoalFromNative(goal codex.Goal, source string) CodexGoal {
	status := goal.Status
	if status == "" {
		status = CodexGoalStatusActive
	}

	return CodexGoal{
		Objective:       strings.TrimSpace(goal.Objective),
		Status:          status,
		TokenBudget:     cloneInt64Ptr(goal.TokenBudget),
		TokensUsed:      goal.TokensUsed,
		TimeUsedSeconds: goal.TimeUsedSeconds,
		CreatedAt:       goal.CreatedAt,
		UpdatedAt:       goal.UpdatedAt,
		ThreadID:        goal.ThreadID,
		Source:          source,
	}
}

func nativeGoalFromCodexGoal(goal CodexGoal, threadID string) codex.Goal {
	return codex.Goal{
		ThreadID:        firstNonEmpty(goal.ThreadID, threadID),
		Objective:       goal.Objective,
		Status:          goal.Status,
		TokenBudget:     cloneInt64Ptr(goal.TokenBudget),
		TokensUsed:      goal.TokensUsed,
		TimeUsedSeconds: goal.TimeUsedSeconds,
		CreatedAt:       goal.CreatedAt,
		UpdatedAt:       goal.UpdatedAt,
	}
}

func cloneCodexGoal(goal CodexGoal) CodexGoal {
	goal.TokenBudget = cloneInt64Ptr(goal.TokenBudget)

	return goal
}

func cloneCodexGoalPtr(goal *CodexGoal) *CodexGoal {
	if goal == nil {
		return nil
	}
	cloned := cloneCodexGoal(*goal)

	return &cloned
}

func equalGoalSnapshot(left *CodexGoal, right *CodexGoal) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Objective == right.Objective &&
		left.Status == right.Status &&
		equalInt64Ptr(left.TokenBudget, right.TokenBudget) &&
		left.TokensUsed == right.TokensUsed &&
		left.TimeUsedSeconds == right.TimeUsedSeconds &&
		left.CreatedAt == right.CreatedAt &&
		left.UpdatedAt == right.UpdatedAt &&
		left.ThreadID == right.ThreadID
}

func equalInt64Ptr(left *int64, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func nullableInt64Ptr(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func cloneInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value

	return &cloned
}

func goalACPError(err error) error {
	if err == nil {
		return nil
	}
	if codexGoalUnavailable(err) {
		return acp.NewInvalidRequest(map[string]any{
			jsonFieldError: "Codex goal support is not available for the current Codex app-server or CODEX_HOME",
			"cause":        err.Error(),
		})
	}

	return err
}

func codexGoalUnavailable(err error) bool {
	text := strings.ToLower(err.Error())

	return strings.Contains(text, "goals feature is disabled") ||
		strings.Contains(text, "no such table: thread_goals")
}
