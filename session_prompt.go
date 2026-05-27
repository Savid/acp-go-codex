package codexacp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/observer"
)

func (s *Session) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	releaseTurn, err := s.acquireTurn(ctx)
	if err != nil {
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil
	}
	defer releaseTurn()

	input, err := promptToCodex(params.Prompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	turnCtx := s.beginTurn(ctx)
	defer s.finishTurn()

	snapshot := s.snapshot()
	model, effort, tier, personality, collaborationMode := s.turnSettings()
	events, err := s.client.RunTurn(turnCtx, codex.TurnStartRequest{
		ThreadID:          snapshot.codexThreadID,
		Prompt:            input,
		Model:             model,
		ServiceTier:       tier,
		ReasoningEffort:   effort,
		Personality:       personality,
		CollaborationMode: collaborationMode,
		SandboxPolicy:     sandboxPolicyForAdditionalDirectories(snapshot.additionalDirectories),
		OutputSchema:      snapshot.outputSchema,
	})
	if err != nil {
		return acp.PromptResponse{}, codexAuthRequiredError(err, s.accountMetaSnapshot())
	}

	stopTail, tailDone := s.startRolloutTail(turnCtx)
	tailStopped := false
	defer func() {
		if !tailStopped {
			stopTail()
			<-tailDone
		}
	}()
	stopReason := acp.StopReasonEndTurn
	var usage *acp.Usage
	var agentText strings.Builder
	agentDeltaItems := map[string]struct{}{}
	reasoningDeltaItems := map[string]struct{}{}

	for event := range events {
		if event.TurnID != "" {
			s.setTurnID(event.TurnID)
		}
		if event.Kind == codex.EventAccountUpdated {
			s.setAccount(redactedAccountMeta(event.Account))
		}
		if err := s.emitRawCodexEvent(turnCtx, event); err != nil {
			return acp.PromptResponse{}, err
		}
		visibleEvent := dedupeCompletedTextEvent(event, agentDeltaItems, reasoningDeltaItems)
		if visibleEvent.Kind == codex.EventAgentMessageDelta {
			visibleEvent = dedupeCompletedAggregateTextEvent(visibleEvent, agentText.String())
		}
		for _, update := range eventUpdates(visibleEvent) {
			if err := s.emitUpdates(turnCtx, update); err != nil {
				return acp.PromptResponse{}, err
			}
		}
		if event.Kind == codex.EventCompleted {
			stopReason = stopReasonFromCodex(event.StopReason)
			usage = usageFromCodex(event.Usage)
		}
		if visibleEvent.Kind == codex.EventAgentMessageDelta && visibleEvent.Text != "" {
			agentText.WriteString(visibleEvent.Text)
		}
		if event.Kind == codex.EventError && event.Err != nil && !s.wasTurnCancelled() {
			return acp.PromptResponse{}, codexAuthRequiredError(event.Err, s.accountMetaSnapshot())
		}
	}
	stopTail()
	<-tailDone
	tailStopped = true

	if turnCtx.Err() != nil || s.wasTurnCancelled() {
		stopReason = acp.StopReasonCancelled
	}
	if err := s.mirrorAndEmitRollout(context.WithoutCancel(ctx)); err != nil {
		return acp.PromptResponse{}, err
	}

	return acp.PromptResponse{
		StopReason:    stopReason,
		Usage:         usage,
		UserMessageId: params.MessageId,
		Meta:          structuredOutputMeta(agentText.String(), snapshot.outputSchema),
	}, nil
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

func (s *Session) turnSettings() (model string, effort string, serviceTier string, personality any, collaborationMode any) {
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
			"mode": string(modePlan),
			"settings": map[string]any{
				"model":                  firstNonEmpty(model, "default"),
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

func (s *Session) emitUpdates(ctx context.Context, updates ...acp.SessionUpdate) error {
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

func (s *Session) emitRawCodexEvent(ctx context.Context, event codex.Event) error {
	if s.rolloutPath != "" {
		return nil
	}
	if !s.rawMessages.Enabled() || len(event.RawParams) == 0 {
		return nil
	}
	raw := decodedRawEvent(event.RawParams)
	raw["method"] = event.RawMethod
	if !s.rawMessages.ShouldEmit(raw) {
		return nil
	}

	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	payload := map[string]any{
		"sessionId": s.id,
		"message":   raw,
	}
	if event.RawJSON != "" {
		payload["rawJSON"] = event.RawJSON
	}

	return conn.NotifyExtension(ctx, rawCodexSDKMessageMethod, payload)
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
	case "commandExecution", "command", "exec", "shell":
		return acp.ToolKindExecute
	case "fileChange", "edit", "patch":
		return acp.ToolKindEdit
	case "mcpToolCall", "dynamicToolCall":
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

	return map[string]any{
		"type":          "workspaceWrite",
		"writableRoots": append([]string(nil), dirs...),
	}
}

func usageFromCodex(usage codex.Usage) *acp.Usage {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.TotalTokens == 0 {
		return nil
	}

	return &acp.Usage{
		InputTokens:  int(usage.InputTokens),
		OutputTokens: int(usage.OutputTokens),
		TotalTokens:  int(usage.TotalTokens),
	}
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
