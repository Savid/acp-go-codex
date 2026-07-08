package codexacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/observer"
)

const (
	codexUsageMetaKey         = "usage"
	codexThreadUsageMetaKey   = "threadUsage"
	usageCachedReadTokensKey  = "cachedReadTokens"
	usageCachedWriteTokensKey = "cachedWriteTokens"
	usageInputTokensKey       = "inputTokens"
	usageOutputTokensKey      = "outputTokens"
	usageReasoningOutputKey   = "reasoningOutputTokens"
	usageTotalTokensKey       = "totalTokens"

	sandboxModeWorkspaceWrite   = "workspace-write"
	sandboxModeReadOnly         = "read-only"
	sandboxModeDangerFullAccess = "danger-full-access"
	toolKindCommandExecution    = "commandExecution"
	toolKindFileChange          = "fileChange"
	toolKindMcpToolCall         = "mcpToolCall"

	sandboxTypeDangerFullAccess = "dangerFullAccess"
	sandboxTypeReadOnly         = "readOnly"
	sandboxTypeWorkspaceWrite   = "workspaceWrite"
)

func (s *session) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	releaseTurn, err := s.acquireTurn(ctx)
	if err != nil {
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil
	}
	defer releaseTurn()

	input, err := promptToCodex(params.Prompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	if relaunchErr := s.ensureLiveClient(ctx); relaunchErr != nil {
		return acp.PromptResponse{}, s.mapTurnFailure(fmt.Errorf("%w: %w", codex.ErrConnectionClosed, relaunchErr))
	}

	turnCtx := s.beginTurn(ctx)
	defer s.finishTurn()

	snapshot := s.snapshot()
	model, effort, tier, personality, collaborationMode := s.turnSettings()
	s.prepareRolloutLiveCursors()

	events, err := s.client.RunTurn(turnCtx, codex.TurnStartRequest{
		ThreadID:          snapshot.codexThreadID,
		Prompt:            input,
		Model:             model,
		ServiceTier:       tier,
		ReasoningEffort:   effort,
		Personality:       personality,
		CollaborationMode: collaborationMode,
		ApprovalPolicy:    snapshot.approvalPolicy,
		SandboxPolicy: firstNonNil(
			sandboxPolicy(snapshot.sandboxPolicy),
			sandboxPolicyForAdditionalDirectories(snapshot.additionalDirectories),
		),
		OutputSchema: snapshot.outputSchema,
	})
	if err != nil {
		return acp.PromptResponse{}, s.mapTurnFailure(err)
	}

	rolloutCompleted := make(chan struct{}, 1)
	rolloutEvents := make(chan codex.Event, 128)
	stopTail, tailDone := s.startRolloutTail(turnCtx, rolloutCompleted, rolloutEvents)

	tailStopped := false
	defer func() {
		if !tailStopped {
			stopTail()
			<-tailDone
		}
	}()

	var agentText strings.Builder

	state := &promptEventState{
		snapshot:            snapshot,
		agentDeltaItems:     map[string]struct{}{},
		reasoningDeltaItems: map[string]struct{}{},
		agentText:           &agentText,
		stopReason:          acp.StopReasonEndTurn,
	}

	var (
		completionTimer    *time.Timer
		completionFallback <-chan time.Time
	)

	defer func() {
		if completionTimer != nil {
			completionTimer.Stop()
		}
	}()

	handleEvent := func(event codex.Event) error {
		return s.handlePromptEvent(turnCtx, event, state)
	}

	var turnDeadline <-chan time.Time

	if timeout := s.agent.options.TurnTimeout; timeout > 0 {
		deadlineTimer := time.NewTimer(timeout)
		defer deadlineTimer.Stop()

		turnDeadline = deadlineTimer.C
	}

	timedOut := false

	eventsOpen := true
	for eventsOpen {
		select {
		case event, ok := <-events:
			if !ok {
				eventsOpen = false

				continue
			}

			if err := handleEvent(event); err != nil {
				return acp.PromptResponse{}, err
			}
		case event := <-rolloutEvents:
			if err := handleEvent(event); err != nil {
				return acp.PromptResponse{}, err
			}
		case <-rolloutCompleted:
			state.completed = true

			if completionTimer == nil {
				if sessionRolloutCompletionFallback <= 0 {
					eventsOpen = false

					continue
				}

				completionTimer = time.NewTimer(sessionRolloutCompletionFallback)
				completionFallback = completionTimer.C
			}
		case <-completionFallback:
			eventsOpen = false
		case <-turnCtx.Done():
			eventsOpen = false
		case <-turnDeadline:
			timedOut = true
			eventsOpen = false
		}
	}

	stopTail()
	<-tailDone

	tailStopped = true

	if turnCtx.Err() != nil || s.wasTurnCancelled() {
		state.stopReason = acp.StopReasonCancelled
	}

	if timedOut && state.stopReason != acp.StopReasonCancelled {
		s.abortTurnAfterTimeout(ctx)

		return acp.PromptResponse{}, s.mapTurnFailure(&codex.TurnFailedError{
			Cause:   codex.CauseTimeout,
			Message: fmt.Sprintf("codex turn exceeded %s deadline", s.agent.options.TurnTimeout),
		})
	}

	if !state.completed && state.stopReason != acp.StopReasonCancelled {
		return acp.PromptResponse{}, s.mapTurnFailure(codex.ErrConnectionClosed)
	}

	if err := s.mirrorAndEmitRollout(context.WithoutCancel(ctx)); err != nil {
		return acp.PromptResponse{}, err
	}

	return acp.PromptResponse{
		StopReason:    state.stopReason,
		Usage:         state.usage,
		UserMessageId: params.MessageId,
		Meta:          structuredOutputMeta(agentText.String(), snapshot.outputSchema),
	}, nil
}

type promptEventState struct {
	snapshot            sessionSnapshot
	agentDeltaItems     map[string]struct{}
	reasoningDeltaItems map[string]struct{}
	agentText           *strings.Builder

	streamedUsage              codex.Usage
	streamedThreadUsage        codex.Usage
	streamedUsageContextWindow int64

	usage      *acp.Usage
	stopReason acp.StopReason
	completed  bool
}

func (s *session) handlePromptEvent(turnCtx context.Context, event codex.Event, state *promptEventState) error {
	if event.TurnID != "" {
		s.setTurnID(event.TurnID)
	}

	if event.Kind == codex.EventAccountUpdated {
		s.setAccount(redactedAccountMeta(event.Account))
	}

	if err := s.emitRawCodexEvent(turnCtx, event); err != nil {
		s.recordRawEmitFailure(turnCtx, err)
	}

	visibleEvent := dedupeCompletedTextEvent(event, state.agentDeltaItems, state.reasoningDeltaItems)
	if visibleEvent.Kind == codex.EventAgentMessageDelta {
		visibleEvent = dedupeCompletedAggregateTextEvent(visibleEvent, state.agentText.String())
	}

	if err := s.emitPromptUpdates(turnCtx, event, visibleEvent, state); err != nil {
		return err
	}

	s.applyPromptUsage(event, state)

	if visibleEvent.Kind == codex.EventAgentMessageDelta && visibleEvent.Text != "" {
		state.agentText.WriteString(visibleEvent.Text)
	}

	if failErr := turnFailureFromEvent(event); failErr != nil && !s.wasTurnCancelled() {
		return s.mapTurnFailure(failErr)
	}

	return nil
}

// turnFailureFromEvent extracts the native failure cause from a terminal event:
// a codex `error` event, or a `turn/completed` whose status is failed/errored.
func turnFailureFromEvent(event codex.Event) error {
	switch event.Kind {
	case codex.EventError, codex.EventCompleted:
		return event.Err
	default:
		return nil
	}
}

// mapTurnFailure translates a native turn failure into the uniform ACP wire
// error. A dead app-server connection marks the session for lazy relaunch but
// leaves it addressable; a native thread-id drift is a wrapper-invariant break
// and surfaces as the unknown-session error.
func (s *session) mapTurnFailure(err error) error {
	if errors.Is(err, codex.ErrThreadNotFound) {
		return newUnknownSession()
	}

	if isCodexAuthError(err) {
		return codexAuthRequiredError(err, s.accountMetaSnapshot())
	}

	data := map[string]any{
		jsonFieldError:   valueTurnFailed,
		jsonFieldCause:   codex.CauseProvider,
		jsonFieldMessage: err.Error(),
	}

	var (
		tf       *codex.TurnFailedError
		procExit *codex.ProcessExitError
	)

	switch {
	case errors.As(err, &tf):
		data[jsonFieldCause] = tf.Cause
		data[jsonFieldMessage] = tf.Message

		if tf.StatusCode > 0 {
			data[jsonFieldStatusCode] = tf.StatusCode
		}

		if tf.ProviderCode != "" {
			data[jsonFieldProviderCode] = tf.ProviderCode
		}

		if tf.Cause == codex.CauseProcessExit || tf.Cause == codex.CauseTransport {
			s.markClientDead()
		}
	case errors.As(err, &procExit):
		// The app-server process died mid-turn: name the real exit status and
		// stderr tail instead of a bare transport EOF.
		data[jsonFieldCause] = codex.CauseProcessExit
		data[jsonFieldMessage] = procExit.Error()

		s.markClientDead()
	case errors.Is(err, codex.ErrConnectionClosed):
		data[jsonFieldCause] = codex.CauseTransport

		s.markClientDead()
	}

	return acp.NewInternalError(data)
}

// abortTurnAfterTimeout interrupts the in-flight native Codex turn after the
// turn deadline expires. The app-server connection stays alive, so the session
// remains retriable.
func (s *session) abortTurnAfterTimeout(ctx context.Context) {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()

	_ = s.client.CancelTurn(abortCtx, s.codexThreadID, s.activeTurnID())
}

func (s *session) recordRawEmitFailure(ctx context.Context, err error) {
	if err == nil {
		return
	}

	s.mu.Lock()
	s.rawEmitFailures++
	s.mu.Unlock()

	if logger := agentLogger(s.agent); logger != nil {
		logger.WarnContext(ctx, "codex raw event emit failed", slog.String(jsonFieldError, err.Error()))
	}
}

func (s *session) emitPromptUpdates(turnCtx context.Context, event codex.Event, visibleEvent codex.Event, state *promptEventState) error {
	for _, update := range eventUpdates(visibleEvent) {
		if err := s.emitUpdates(turnCtx, update); err != nil {
			return err
		}
	}

	for _, update := range usageUpdatesForEvent(event, &state.streamedUsage, &state.streamedThreadUsage, &state.streamedUsageContextWindow) {
		if err := s.emitUpdates(turnCtx, update); err != nil {
			return err
		}
	}

	return nil
}

func (s *session) applyPromptUsage(event codex.Event, state *promptEventState) {
	if event.Kind == codex.EventUsageUpdated {
		if tokenUsage := usageFromCodex(event.TokenUsage.Last); tokenUsage != nil {
			state.usage = tokenUsage
		}
	}

	if event.Kind == codex.EventCompleted {
		state.completed = true

		state.stopReason = stopReasonFromCodex(event.StopReason)
		if completedUsage := usageFromCodex(event.Usage); completedUsage != nil {
			state.usage = completedUsage
		}
	}
}

func dedupeCompletedTextEvent(event codex.Event, agentDeltaItems map[string]struct{}, reasoningDeltaItems map[string]struct{}) codex.Event {
	switch event.Kind {
	case codex.EventAgentMessageDelta:
		return dedupeCompletedTextKind(event, agentDeltaItems)
	case codex.EventReasoningDelta:
		return dedupeCompletedTextKind(event, reasoningDeltaItems)
	default:
		return event
	}
}

func dedupeCompletedTextKind(event codex.Event, deltaItems map[string]struct{}) codex.Event {
	if event.ItemID == "" {
		return event
	}

	if event.Completed {
		if _, ok := deltaItems[event.ItemID]; ok {
			event.Text = ""
		}

		return event
	}

	if event.Text != "" {
		deltaItems[event.ItemID] = struct{}{}
	}

	return event
}

func dedupeCompletedAggregateTextEvent(event codex.Event, priorText string) codex.Event {
	if !event.Completed || event.ItemID != "" || event.Text == "" {
		return event
	}

	if event.Text == priorText {
		event.Text = ""
	}

	return event
}

func (s *session) turnSettings() (model string, effort string, serviceTier string, personality any, collaborationMode any) {
	s.mu.Lock()
	model = s.model
	effort = s.reasoningEffort
	serviceTier = s.serviceTier
	personalityValue := s.personality
	mode := s.mode
	s.mu.Unlock()

	if personalityValue != "" {
		personality = personalityValue
	}

	if mode == modePlan {
		collaborationMode = map[string]any{
			jsonFieldMode: string(modePlan),
			"settings": map[string]any{
				"model":                  firstNonEmpty(model, valueDefault),
				"developer_instructions": nil,
				"reasoning_effort":       nullableString(effort),
			},
		}
	}

	return model, effort, serviceTier, personality, collaborationMode
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}

	return value
}

func (s *session) emitUpdates(ctx context.Context, updates ...acp.SessionUpdate) error {
	if len(updates) > 0 {
		s.agent.observe.ObserveFirstPromptUpdate(ctx)
	}

	for _, update := range updates {
		if err := s.agent.emitUpdate(ctx, s.id, update); err != nil {
			return err
		}
	}

	return nil
}

func (s *session) emitRawCodexEvent(ctx context.Context, event codex.Event) error {
	if !s.rawMessages.Enabled() || len(event.RawParams) == 0 {
		return nil
	}

	raw := decodedRawEvent(event.RawParams)
	if !s.rawMessages.ShouldEmit(raw) {
		return nil
	}

	raw["method"] = event.RawMethod

	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	payload := map[string]any{
		jsonFieldSessionID: s.id,
		jsonFieldSequence:  s.nextRawEventSequence(),
		jsonFieldSource:    "codex-app-server",
		jsonFieldEvent:     raw,
	}
	if event.RawJSON != "" {
		payload["rawJSON"] = event.RawJSON
	}

	return conn.NotifyExtension(ctx, RawEventMethod, capRawEventPayload(payload))
}

func eventUpdates(event codex.Event) []acp.SessionUpdate {
	switch event.Kind {
	case codex.EventAgentMessageDelta:
		if event.Text == "" {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateAgentMessageText(event.Text)}
	case codex.EventReasoningDelta:
		if event.Text == "" {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateAgentThoughtText(event.Text)}
	case codex.EventPlanUpdated:
		return planUpdate(event.Plan)
	case codex.EventToolStarted:
		return []acp.SessionUpdate{startToolUpdate(event.Tool)}
	case codex.EventToolDelta:
		return toolDeltaUpdate(event.Tool, event.Text)
	case codex.EventToolCompleted:
		return []acp.SessionUpdate{completeToolUpdate(event.Tool)}
	case codex.EventDiffUpdated:
		if event.Diff == "" {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateToolCall(acp.ToolCallId("codex-diff"), acp.WithUpdateContent([]acp.ToolCallContent{diffContent("", event.Diff)}))}
	case codex.EventWarning:
		if event.Text == "" {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateAgentThoughtText(event.Text)}
	default:
		return nil
	}
}

func usageUpdatesForEvent(
	event codex.Event,
	streamedUsage *codex.Usage,
	streamedThreadUsage *codex.Usage,
	streamedUsageContextWindow *int64,
) []acp.SessionUpdate {
	switch event.Kind {
	case codex.EventUsageUpdated:
		if event.TokenUsage.Last == *streamedUsage &&
			event.TokenUsage.Total == *streamedThreadUsage &&
			event.TokenUsage.ModelContextWindow == *streamedUsageContextWindow {
			return nil
		}

		updates := tokenUsageUpdateFromCodex(event.TokenUsage)
		if len(updates) > 0 {
			*streamedUsage = event.TokenUsage.Last
			*streamedThreadUsage = event.TokenUsage.Total
			*streamedUsageContextWindow = event.TokenUsage.ModelContextWindow
		}

		return updates
	case codex.EventCompleted:
		if event.Usage == *streamedUsage {
			return nil
		}

		updates := usageUpdateFromCodex(event.Usage)
		if len(updates) > 0 {
			*streamedUsage = event.Usage
			*streamedThreadUsage = codex.Usage{}
			*streamedUsageContextWindow = 0
		}

		return updates
	default:
		return nil
	}
}

func planUpdate(steps []codex.PlanStep) []acp.SessionUpdate {
	if len(steps) == 0 {
		return nil
	}

	entries := make([]acp.PlanEntry, 0, len(steps))
	for _, step := range steps {
		entries = append(entries, acp.PlanEntry{
			Content:  step.Text,
			Priority: acp.PlanEntryPriorityMedium,
			Status:   planStatus(step.Status),
		})
	}

	return []acp.SessionUpdate{acp.UpdatePlan(entries...)}
}

func startToolUpdate(tool codex.ToolEvent) acp.SessionUpdate {
	id := acp.ToolCallId(firstNonEmpty(tool.ID, "codex-tool"))
	title := firstNonEmpty(tool.Title, tool.Kind, string(id))

	return acp.StartToolCall(
		id,
		title,
		acp.WithStartKind(toolKind(tool)),
		acp.WithStartStatus(acp.ToolCallStatusInProgress),
		acp.WithStartRawInput(tool.Raw),
	)
}

func toolDeltaUpdate(tool codex.ToolEvent, text string) []acp.SessionUpdate {
	if text == "" {
		text = tool.Content
	}

	if text == "" {
		return nil
	}

	id := acp.ToolCallId(firstNonEmpty(tool.ID, "codex-tool"))

	return []acp.SessionUpdate{acp.UpdateToolCall(id, acp.WithUpdateContent([]acp.ToolCallContent{textToolContent(text)}))}
}

func completeToolUpdate(tool codex.ToolEvent) acp.SessionUpdate {
	id := acp.ToolCallId(firstNonEmpty(tool.ID, "codex-tool"))

	opts := []acp.ToolCallUpdateOpt{
		acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
		acp.WithUpdateKind(toolKind(tool)),
		acp.WithUpdateRawOutput(tool.Raw),
	}
	if tool.Title != "" {
		opts = append(opts, acp.WithUpdateTitle(tool.Title))
	}

	if tool.Content != "" {
		opts = append(opts, acp.WithUpdateContent([]acp.ToolCallContent{textToolContent(tool.Content)}))
	}

	return acp.UpdateToolCall(id, opts...)
}

func textToolContent(text string) acp.ToolCallContent {
	return acp.ToolCallContent{
		Content: &acp.ToolCallContentContent{
			Content: acp.TextBlock(text),
		},
	}
}

func diffContent(path string, diff string) acp.ToolCallContent {
	return acp.ToolCallContent{
		Diff: &acp.ToolCallContentDiff{
			Path:    path,
			NewText: diff,
		},
	}
}

func toolKind(tool codex.ToolEvent) acp.ToolKind {
	switch tool.Kind {
	case toolKindCommandExecution, valueCommand, "exec", "shell":
		return acp.ToolKindExecute
	case toolKindFileChange, "edit", "patch":
		return acp.ToolKindEdit
	case toolKindMcpToolCall, "dynamicToolCall":
		return acp.ToolKindOther
	default:
		return acp.ToolKindOther
	}
}

func planStatus(status codex.PlanStepStatus) acp.PlanEntryStatus {
	switch status {
	case codex.PlanStepInProgress:
		return acp.PlanEntryStatusInProgress
	case codex.PlanStepCompleted:
		return acp.PlanEntryStatusCompleted
	default:
		return acp.PlanEntryStatusPending
	}
}

func stopReasonFromCodex(reason codex.StopReason) acp.StopReason {
	switch reason {
	case codex.StopReasonCancelled:
		return acp.StopReasonCancelled
	default:
		return acp.StopReasonEndTurn
	}
}

func sandboxPolicyForAdditionalDirectories(dirs []string) any {
	if len(dirs) == 0 {
		return nil
	}

	return workspaceWriteSandboxPolicy(dirs)
}

func sandboxMode(policy any) any {
	switch p := policy.(type) {
	case string:
		return p
	case map[string]any:
		switch p[jsonFieldType] {
		case sandboxTypeDangerFullAccess, sandboxModeDangerFullAccess, "fullAccess":
			return sandboxModeDangerFullAccess
		case sandboxTypeReadOnly, sandboxModeReadOnly:
			return sandboxModeReadOnly
		case sandboxTypeWorkspaceWrite, sandboxModeWorkspaceWrite:
			return sandboxModeWorkspaceWrite
		}
	}

	return nil
}

func sandboxPolicy(policy any) any {
	switch p := policy.(type) {
	case string:
		switch p {
		case sandboxModeDangerFullAccess:
			return map[string]any{jsonFieldType: sandboxTypeDangerFullAccess}
		case sandboxModeReadOnly:
			return map[string]any{
				jsonFieldType:          sandboxTypeReadOnly,
				jsonFieldNetworkAccess: false,
			}
		case sandboxModeWorkspaceWrite:
			return workspaceWriteSandboxPolicy(nil)
		default:
			return p
		}
	case map[string]any:
		cloned, _ := cloneAny(p).(map[string]any)
		if cloned == nil {
			return p
		}

		if cloned[jsonFieldType] == sandboxTypeWorkspaceWrite || cloned[jsonFieldType] == sandboxModeWorkspaceWrite {
			cloned[jsonFieldType] = sandboxTypeWorkspaceWrite
			if _, ok := cloned["writableRoots"]; !ok {
				cloned["writableRoots"] = []string{}
			}

			if _, ok := cloned[jsonFieldNetworkAccess]; !ok {
				cloned[jsonFieldNetworkAccess] = false
			}

			if _, ok := cloned["excludeTmpdirEnvVar"]; !ok {
				cloned["excludeTmpdirEnvVar"] = false
			}

			if _, ok := cloned["excludeSlashTmp"]; !ok {
				cloned["excludeSlashTmp"] = false
			}
		}

		return cloned
	default:
		return p
	}
}

func workspaceWriteSandboxPolicy(dirs []string) map[string]any {
	return map[string]any{
		jsonFieldType:          sandboxTypeWorkspaceWrite,
		"writableRoots":        append([]string{}, dirs...),
		jsonFieldNetworkAccess: false,
		"excludeTmpdirEnvVar":  false,
		"excludeSlashTmp":      false,
	}
}

func usageFromCodex(usage codex.Usage) *acp.Usage {
	if usage.CachedReadTokens == 0 &&
		usage.CachedWriteTokens == 0 &&
		usage.InputTokens == 0 &&
		usage.OutputTokens == 0 &&
		usage.ReasoningOutputTokens == 0 &&
		usage.TotalTokens == 0 {
		return nil
	}

	result := &acp.Usage{
		InputTokens:  int(usage.InputTokens),
		OutputTokens: int(usage.OutputTokens),
		TotalTokens:  int(usage.TotalTokens),
	}
	if result.TotalTokens == 0 {
		result.TotalTokens = result.InputTokens + result.OutputTokens
	}

	if usage.CachedReadTokens > 0 {
		result.CachedReadTokens = acp.Ptr(int(usage.CachedReadTokens))
	}

	if usage.CachedWriteTokens > 0 {
		result.CachedWriteTokens = acp.Ptr(int(usage.CachedWriteTokens))
	}

	if usage.ReasoningOutputTokens > 0 {
		result.ThoughtTokens = acp.Ptr(int(usage.ReasoningOutputTokens))
	}

	return result
}

func usageUpdateFromCodex(usage codex.Usage) []acp.SessionUpdate {
	return usageUpdateFromCodexContext(usage, codex.Usage{}, 0)
}

func tokenUsageUpdateFromCodex(usage codex.TokenUsage) []acp.SessionUpdate {
	return usageUpdateFromCodexContext(usage.Last, usage.Total, usage.ModelContextWindow)
}

func usageUpdateFromCodexContext(usage codex.Usage, threadUsage codex.Usage, contextWindow int64) []acp.SessionUpdate {
	acpUsage := usageFromCodex(usage)
	if acpUsage == nil {
		return nil
	}

	used := acpUsage.TotalTokens
	meta := map[string]any{
		codexUsageMetaKey: usageMetaFromCodex(acpUsage),
	}

	if threadACPUsage := usageFromCodex(threadUsage); threadACPUsage != nil {
		used = threadACPUsage.TotalTokens
		meta[codexThreadUsageMetaKey] = usageMetaFromCodex(threadACPUsage)
	}

	return []acp.SessionUpdate{{
		UsageUpdate: &acp.SessionUsageUpdate{
			Used: used,
			Size: int(contextWindow),
			Meta: map[string]any{
				codexMetaKey: meta,
			},
		},
	}}
}

func usageMetaFromCodex(usage *acp.Usage) map[string]any {
	meta := map[string]any{
		usageInputTokensKey:       usage.InputTokens,
		usageCachedReadTokensKey:  0,
		usageCachedWriteTokensKey: 0,
		usageOutputTokensKey:      usage.OutputTokens,
		usageReasoningOutputKey:   0,
		usageTotalTokensKey:       usage.TotalTokens,
	}
	if usage.CachedReadTokens != nil {
		meta[usageCachedReadTokensKey] = *usage.CachedReadTokens
	}

	if usage.CachedWriteTokens != nil {
		meta[usageCachedWriteTokensKey] = *usage.CachedWriteTokens
	}

	if usage.ThoughtTokens != nil {
		meta[usageReasoningOutputKey] = *usage.ThoughtTokens
	}

	return meta
}

func promptResultForObserver(resp acp.PromptResponse, err error, model string) observer.PromptResult {
	result := observer.PromptResult{
		Err:        err,
		Model:      model,
		StopReason: string(resp.StopReason),
	}
	if resp.Usage == nil {
		return result
	}

	result.InputTokens = resp.Usage.InputTokens
	result.OutputTokens = resp.Usage.OutputTokens

	result.TotalTokens = resp.Usage.TotalTokens
	if resp.Usage.CachedReadTokens != nil {
		result.CachedReadTokens = *resp.Usage.CachedReadTokens
	}

	if resp.Usage.CachedWriteTokens != nil {
		result.CachedWriteTokens = *resp.Usage.CachedWriteTokens
	}

	if resp.Usage.ThoughtTokens != nil {
		result.ThoughtTokens = *resp.Usage.ThoughtTokens
	}

	return result
}

func structuredOutputMeta(text string, schema any) map[string]any {
	if schema == nil || strings.TrimSpace(text) == "" {
		return nil
	}

	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return nil
	}

	return map[string]any{codexMetaKey: map[string]any{structuredOutputMetaKey: value}}
}
