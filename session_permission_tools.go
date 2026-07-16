package codexacp

import (
	"context"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
)

// permissionToolClass is the native operation family used when Codex sends an
// approval request with an identifier that differs from item/started. Classes
// deliberately do not cross-match: an ambiguous correlation is denied instead
// of attaching permission to the wrong tool.
type permissionToolClass string

const (
	permissionToolCommand     permissionToolClass = "command"
	permissionToolFileChange  permissionToolClass = "file_change"
	permissionToolPermissions permissionToolClass = "permissions"
	permissionToolMCP         permissionToolClass = "mcp"
)

type permissionToolRegistry struct {
	mu      sync.Mutex
	tools   map[acp.ToolCallId]*permissionToolRecord
	aliases map[string]acp.ToolCallId
}

type permissionToolRecord struct {
	id                 acp.ToolCallId
	class              permissionToolClass
	fingerprint        permissionToolFingerprint
	pendingNativeStart bool
	terminal           bool
}

type permissionToolFingerprint struct {
	title  string
	server string
	tool   string
}

func (r *permissionToolRegistry) reset() {
	r.mu.Lock()
	r.tools = nil
	r.aliases = nil
	r.mu.Unlock()
}

func (r *permissionToolRegistry) ensure() {
	if r.tools == nil {
		r.tools = make(map[acp.ToolCallId]*permissionToolRecord)
	}

	if r.aliases == nil {
		r.aliases = make(map[string]acp.ToolCallId)
	}
}

// requestPermissionForTool publishes a pending tool call before invoking the
// ACP permission callback when Codex has not published item/started yet. When
// item/started won the race, the callback reuses that exact nonterminal ACP ID.
// The registry lock spans both publication and callback so a completion cannot
// make the selected ID terminal between those two client calls.
func (s *session) requestPermissionForTool(
	ctx context.Context,
	conn agentClient,
	request acp.RequestPermissionRequest,
	class permissionToolClass,
) (acp.RequestPermissionResponse, bool, error) {
	registry := &s.permissionTools
	registry.mu.Lock()
	defer registry.mu.Unlock()

	if !s.hasActiveTurn() || ctx.Err() != nil {
		return acp.RequestPermissionResponse{}, false, nil
	}

	registry.ensure()

	fingerprint := permissionFingerprint(request.ToolCall.Title, request.ToolCall.RawInput, class)

	record, valid := registry.matchPermissionTool(request.ToolCall.ToolCallId, class, fingerprint)
	if !valid {
		return acp.RequestPermissionResponse{}, false, nil
	}

	if record == nil {
		id := request.ToolCall.ToolCallId
		if id == "" {
			return acp.RequestPermissionResponse{}, false, nil
		}

		title := string(id)
		if request.ToolCall.Title != nil && *request.ToolCall.Title != "" {
			title = *request.ToolCall.Title
		}

		kind := acp.ToolKindOther
		if request.ToolCall.Kind != nil {
			kind = *request.ToolCall.Kind
		}

		start := acp.StartToolCall(
			id,
			title,
			acp.WithStartKind(kind),
			acp.WithStartStatus(acp.ToolCallStatusPending),
			acp.WithStartContent(request.ToolCall.Content),
			acp.WithStartRawInput(request.ToolCall.RawInput),
		)
		if err := s.emitUpdates(ctx, start); err != nil {
			return acp.RequestPermissionResponse{}, false, err
		}

		record = &permissionToolRecord{
			id:                 id,
			class:              class,
			fingerprint:        fingerprint,
			pendingNativeStart: true,
		}
		registry.tools[id] = record
		registry.aliases[string(id)] = id
	}

	request.ToolCall.ToolCallId = record.id

	status := acp.ToolCallStatusInProgress
	if record.pendingNativeStart {
		status = acp.ToolCallStatusPending
	}

	request.ToolCall.Status = &status
	response, err := conn.RequestPermission(ctx, request)

	return response, true, err
}

// createElicitationForMCPTool resolves a native MCP item/tool association to
// the exact published ACP tool ID. The registry stays locked until the client
// call returns so item/completed cannot make that ID terminal between
// correlation and request establishment.
func (s *session) createElicitationForMCPTool(
	ctx context.Context,
	conn agentClient,
	request acp.UnstableCreateElicitationRequest,
	nativeToolID string,
	params map[string]any,
) (acp.UnstableCreateElicitationResponse, bool, error) {
	registry := &s.permissionTools
	registry.mu.Lock()
	defer registry.mu.Unlock()

	s.mu.Lock()
	turnNonce := s.turnNonce
	active := s.turnDone != nil && turnNonce != ""
	s.mu.Unlock()

	if !active || ctx.Err() != nil {
		return acp.UnstableCreateElicitationResponse{}, false, nil
	}

	registry.ensure()

	fingerprint := permissionFingerprint(nil, params, permissionToolMCP)

	record, valid := registry.matchPermissionTool(acp.ToolCallId(nativeToolID), permissionToolMCP, fingerprint)
	if !valid || record == nil {
		return acp.UnstableCreateElicitationResponse{}, false, nil
	}

	response, err := conn.CreateElicitation(ctx, request, elicitationScope{
		SessionID:  s.id,
		TurnNonce:  turnNonce,
		ToolCallID: record.id,
	})

	return response, true, err
}

func (r *permissionToolRegistry) matchPermissionTool(
	id acp.ToolCallId,
	class permissionToolClass,
	fingerprint permissionToolFingerprint,
) (*permissionToolRecord, bool) {
	if id != "" {
		if canonical, ok := r.aliases[string(id)]; ok {
			record := r.tools[canonical]
			if record == nil || record.terminal || record.class != class {
				return nil, false
			}

			return record, true
		}
	}

	record, ambiguous := bestPermissionTool(r.tools, class, fingerprint, false)
	if ambiguous {
		return nil, false
	}

	return record, true
}

type permissionToolEventPublication struct {
	registry   *permissionToolRegistry
	event      codex.Event
	record     *permissionToolRecord
	originalID string
	transition bool
	locked     bool
}

func (s *session) preparePermissionToolEvent(event codex.Event) permissionToolEventPublication {
	publication := permissionToolEventPublication{event: event}
	if event.Kind != codex.EventToolStarted && event.Kind != codex.EventToolDelta && event.Kind != codex.EventToolCompleted {
		return publication
	}

	registry := &s.permissionTools
	registry.mu.Lock()
	registry.ensure()

	publication.registry = registry
	publication.locked = true
	publication.originalID = firstNonEmpty(event.Tool.ID, "codex-tool")

	if canonical, ok := registry.aliases[publication.originalID]; ok {
		publication.record = registry.tools[canonical]
	}

	class := permissionClassForToolEvent(event.Tool)

	fingerprint := permissionFingerprint(nil, event.Tool.Raw, class)
	if fingerprint.title == "" {
		fingerprint.title = normalizePermissionToolValue(event.Tool.Title)
	}

	if publication.record == nil && (event.Kind == codex.EventToolStarted || event.Kind == codex.EventToolCompleted) {
		publication.record, _ = bestPermissionTool(registry.tools, class, fingerprint, true)
	}

	if publication.record != nil {
		publication.event.Tool.ID = string(publication.record.id)
		publication.transition = event.Kind == codex.EventToolStarted && publication.record.pendingNativeStart
	}

	return publication
}

func (p *permissionToolEventPublication) finish(published bool) {
	if !p.locked {
		return
	}
	defer p.registry.mu.Unlock()

	if !published {
		return
	}

	class := permissionClassForToolEvent(p.event.Tool)

	fingerprint := permissionFingerprint(nil, p.event.Tool.Raw, class)
	if fingerprint.title == "" {
		fingerprint.title = normalizePermissionToolValue(p.event.Tool.Title)
	}

	record := p.record
	switch p.event.Kind {
	case codex.EventToolStarted:
		if record == nil {
			id := acp.ToolCallId(firstNonEmpty(p.event.Tool.ID, "codex-tool"))
			record = &permissionToolRecord{id: id, class: class, fingerprint: fingerprint}
			p.registry.tools[id] = record
		}

		record.pendingNativeStart = false
		record.fingerprint = mergePermissionFingerprint(record.fingerprint, fingerprint)
		p.registry.aliases[p.originalID] = record.id
		p.registry.aliases[string(record.id)] = record.id
	case codex.EventToolCompleted:
		if record == nil {
			id := acp.ToolCallId(firstNonEmpty(p.event.Tool.ID, "codex-tool"))
			record = &permissionToolRecord{id: id, class: class, fingerprint: fingerprint}
			p.registry.tools[id] = record
		}

		record.pendingNativeStart = false
		record.terminal = true
		record.fingerprint = mergePermissionFingerprint(record.fingerprint, fingerprint)
		p.registry.aliases[p.originalID] = record.id
		p.registry.aliases[string(record.id)] = record.id
	}
}

func (p permissionToolEventPublication) updates() []acp.SessionUpdate {
	if !p.transition {
		return eventUpdates(p.event)
	}

	tool := p.event.Tool
	id := acp.ToolCallId(tool.ID)

	opts := []acp.ToolCallUpdateOpt{
		acp.WithUpdateStatus(acp.ToolCallStatusInProgress),
		acp.WithUpdateKind(toolKind(tool)),
		acp.WithUpdateRawInput(tool.Raw),
	}
	if tool.Title != "" {
		opts = append(opts, acp.WithUpdateTitle(tool.Title))
	}

	return []acp.SessionUpdate{acp.UpdateToolCall(id, opts...)}
}

func permissionClassForToolEvent(tool codex.ToolEvent) permissionToolClass {
	switch tool.Kind {
	case toolKindCommandExecution, valueCommand, "exec", toolKindShell:
		return permissionToolCommand
	case toolKindFileChange, "edit", toolKindPatch:
		return permissionToolFileChange
	case toolKindMcpToolCall, toolKindDynamicToolCall:
		return permissionToolMCP
	default:
		return ""
	}
}

func bestPermissionTool(
	tools map[acp.ToolCallId]*permissionToolRecord,
	class permissionToolClass,
	fingerprint permissionToolFingerprint,
	pendingOnly bool,
) (*permissionToolRecord, bool) {
	var best *permissionToolRecord

	bestScore := -1
	ambiguous := false

	for _, record := range tools {
		if record == nil || record.terminal || record.class != class || (pendingOnly && !record.pendingNativeStart) {
			continue
		}

		score, compatible := permissionFingerprintScore(record.fingerprint, fingerprint)
		if !compatible {
			continue
		}

		switch {
		case score > bestScore:
			best = record
			bestScore = score
			ambiguous = false
		case score == bestScore:
			ambiguous = true
		}
	}

	if ambiguous {
		return nil, true
	}

	return best, false
}

func permissionFingerprint(title *string, raw any, class permissionToolClass) permissionToolFingerprint {
	fingerprint := permissionToolFingerprint{
		server: normalizePermissionToolValue(deepPermissionString(raw, "serverName", "server")),
		tool:   normalizePermissionToolValue(deepPermissionString(raw, "toolName", "tool", "tool_title")),
	}
	if title != nil {
		fingerprint.title = normalizePermissionToolValue(*title)
	}

	if class == permissionToolMCP && fingerprint.tool == "" {
		fingerprint.tool = fingerprint.title
	}

	return fingerprint
}

func deepPermissionString(value any, keys ...string) string {
	values, ok := value.(map[string]any)
	if !ok {
		return ""
	}

	for _, key := range keys {
		if text, ok := values[key].(string); ok && text != "" {
			return text
		}
	}

	childKeys := make([]string, 0, len(values))
	for key := range values {
		childKeys = append(childKeys, key)
	}

	sort.Strings(childKeys)

	for _, key := range childKeys {
		if text := deepPermissionString(values[key], keys...); text != "" {
			return text
		}
	}

	return ""
}

func permissionFingerprintScore(left permissionToolFingerprint, right permissionToolFingerprint) (int, bool) {
	score := 0

	for _, pair := range [][2]string{{left.server, right.server}, {left.tool, right.tool}} {
		if pair[0] != "" && pair[1] != "" {
			if pair[0] != pair[1] {
				return 0, false
			}

			score += 4
		}
	}

	if left.title != "" && right.title != "" && left.title == right.title {
		score++
	}

	return score, true
}

func mergePermissionFingerprint(left permissionToolFingerprint, right permissionToolFingerprint) permissionToolFingerprint {
	if left.title == "" {
		left.title = right.title
	}

	if left.server == "" {
		left.server = right.server
	}

	if left.tool == "" {
		left.tool = right.tool
	}

	return left
}

func normalizePermissionToolValue(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}

		return -1
	}, value)
}
