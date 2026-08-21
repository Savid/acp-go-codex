package codexacp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-codex/internal/lifecycle"
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
	permissionToolLimit                           = 1024
	permissionAliasLimit                          = permissionToolLimit * 3
)

type permissionToolRegistry struct {
	mu      sync.Mutex
	tools   map[acp.ToolCallId]*permissionToolRecord
	aliases map[string]acp.ToolCallId
	failure error
}

type permissionToolRecord struct {
	id                 acp.ToolCallId
	class              permissionToolClass
	fingerprint        permissionToolFingerprint
	pendingNativeStart bool
	terminal           bool
	leases             int
	startDone          chan struct{}
	startSettled       bool
	startErr           error
	leaseDone          chan struct{}
}

type permissionToolFingerprint struct {
	title  string
	server string
	tool   string
}

func (r *permissionToolRegistry) reset() {
	r.mu.Lock()
	for _, record := range r.tools {
		if record == nil {
			continue
		}

		if !record.startSettled && record.startDone != nil {
			record.startSettled = true
			record.startErr = codex.ErrConnectionClosed
			close(record.startDone)
		}

		if record.leaseDone != nil {
			close(record.leaseDone)
			record.leaseDone = nil
		}
	}

	r.tools = nil
	r.aliases = nil
	r.failure = nil
	r.mu.Unlock()
}

func (r *permissionToolRegistry) ensure() error {
	if r.failure != nil {
		return r.failure
	}

	if r.tools == nil {
		r.tools = make(map[acp.ToolCallId]*permissionToolRecord)
	}

	if r.aliases == nil {
		r.aliases = make(map[string]acp.ToolCallId)
	}

	return nil
}

func (r *permissionToolRegistry) fail(err error) error {
	if err != nil && r.failure == nil {
		r.failure = err
	}

	return err
}

func (r *permissionToolRegistry) addTool(record *permissionToolRecord) error {
	if record == nil || record.id == "" {
		return r.fail(errors.New("codex permission tool omitted its ACP identity"))
	}

	if _, ok := r.tools[record.id]; !ok && len(r.tools) == permissionToolLimit {
		return r.fail(fmt.Errorf("%w: permission tool registry", codex.ErrTurnEventOverflow))
	}

	r.tools[record.id] = record

	return nil
}

func (r *permissionToolRegistry) addAlias(alias string, id acp.ToolCallId) error {
	if alias == "" {
		return nil
	}

	if _, ok := r.aliases[alias]; !ok && len(r.aliases) == permissionAliasLimit {
		return r.fail(fmt.Errorf("%w: permission tool alias registry", codex.ErrTurnEventOverflow))
	}

	r.aliases[alias] = id

	return nil
}

func (r *permissionToolRegistry) release(record *permissionToolRecord) {
	r.mu.Lock()
	if record != nil && record.leases > 0 {
		record.leases--
		if record.leases == 0 && record.leaseDone != nil {
			close(record.leaseDone)
			record.leaseDone = nil
		}
	}
	r.mu.Unlock()
}

func (r *permissionToolRegistry) acquire(record *permissionToolRecord) {
	if record.leases == 0 {
		record.leaseDone = make(chan struct{})
	}

	record.leases++
}

func (r *permissionToolRegistry) waitForLeases(ctx context.Context, record *permissionToolRecord) error {
	r.mu.Lock()
	if record == nil || record.leases == 0 || record.leaseDone == nil {
		r.mu.Unlock()

		return nil
	}

	done := record.leaseDone
	r.mu.Unlock()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newPermissionToolRecord(id acp.ToolCallId, class permissionToolClass, fingerprint permissionToolFingerprint) *permissionToolRecord {
	return &permissionToolRecord{
		id: id, class: class, fingerprint: fingerprint, startDone: make(chan struct{}),
	}
}

func (r *permissionToolRegistry) completeStart(record *permissionToolRecord, err error) {
	if record == nil {
		return
	}

	r.mu.Lock()
	if !record.startSettled {
		record.startSettled = true

		record.startErr = err
		if err != nil {
			delete(r.tools, record.id)

			for alias, id := range r.aliases {
				if id == record.id {
					delete(r.aliases, alias)
				}
			}
		}

		if record.startDone != nil {
			close(record.startDone)
		}
	}
	r.mu.Unlock()
}

func (r *permissionToolRegistry) waitForStart(ctx context.Context, record *permissionToolRecord) error {
	r.mu.Lock()
	if record == nil || record.startDone == nil || record.startSettled {
		var err error
		if record != nil {
			err = record.startErr
		}
		r.mu.Unlock()

		return err
	}

	done := record.startDone
	r.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	r.mu.Lock()
	err := record.startErr
	r.mu.Unlock()

	return err
}

// requestPermissionForTool publishes a pending tool call before invoking the
// ACP permission callback when Codex has not published item/started yet. When
// item/started won the race, the callback reuses that exact nonterminal ACP ID.
// Selection takes a lease and releases the registry before lifecycle or host
// calls, so a native completion can advance without changing the selected ID.
func (s *session) requestPermissionForTool(
	ctx context.Context,
	conn agentClient,
	request acp.RequestPermissionRequest,
	class permissionToolClass,
) (acp.RequestPermissionResponse, bool, error) {
	params, _ := request.ToolCall.RawInput.(map[string]any)

	turnNonce, active := s.activeTurnNonceForNativeTurn(codex.RequestTurnID(params))
	if !active || ctx.Err() != nil {
		return acp.RequestPermissionResponse{}, false, nil
	}

	registry := &s.permissionTools
	registry.mu.Lock()
	if err := registry.ensure(); err != nil {
		registry.mu.Unlock()

		return acp.RequestPermissionResponse{}, true, err
	}

	fingerprint := permissionFingerprint(request.ToolCall.Title, request.ToolCall.RawInput, class)

	record, valid := registry.matchPermissionTool(request.ToolCall.ToolCallId, class, fingerprint)
	if !valid {
		registry.mu.Unlock()

		return acp.RequestPermissionResponse{}, false, nil
	}

	created := false

	if record == nil {
		id := request.ToolCall.ToolCallId
		if id == "" {
			registry.mu.Unlock()

			return acp.RequestPermissionResponse{}, false, nil
		}

		record = newPermissionToolRecord(id, class, fingerprint)

		record.pendingNativeStart = true
		if err := registry.addTool(record); err != nil {
			registry.mu.Unlock()

			return acp.RequestPermissionResponse{}, true, err
		}

		if err := registry.addAlias(string(id), id); err != nil {
			registry.mu.Unlock()

			return acp.RequestPermissionResponse{}, true, err
		}

		created = true
	}

	registry.acquire(record)
	pendingNativeStart := record.pendingNativeStart
	registry.mu.Unlock()

	releaseLease := sync.OnceFunc(func() { registry.release(record) })
	defer releaseLease()

	if created {
		title := string(record.id)
		if request.ToolCall.Title != nil && *request.ToolCall.Title != "" {
			title = *request.ToolCall.Title
		}

		kind := acp.ToolKindOther
		if request.ToolCall.Kind != nil {
			kind = *request.ToolCall.Kind
		}

		start := acp.StartToolCall(
			record.id, title,
			acp.WithStartKind(kind),
			acp.WithStartStatus(acp.ToolCallStatusPending),
			acp.WithStartContent(request.ToolCall.Content),
			acp.WithStartRawInput(request.ToolCall.RawInput),
		)
		err := s.emitUpdates(withTurnRoute(ctx, turnNonce), start)
		registry.completeStart(record, err)

		if err != nil {
			return acp.RequestPermissionResponse{}, false, err
		}
	} else if err := registry.waitForStart(ctx, record); err != nil {
		return acp.RequestPermissionResponse{}, true, err
	}

	request.ToolCall.ToolCallId = record.id

	status := acp.ToolCallStatusInProgress
	if pendingNativeStart {
		status = acp.ToolCallStatusPending
	}

	request.ToolCall.Status = &status

	// Mint correlation first; publication waits for the connection's exact
	// host-request registration barrier.
	action, correlation, err := s.beginAction(ctx, lifecycle.ActionPermission, true)
	if err != nil {
		return acp.RequestPermissionResponse{}, false, err
	}

	request.Meta = stampActionCorrelation(request.Meta, correlation)

	response, err := requestPermissionWithAction(ctx, conn, request, action, releaseLease)
	if err != nil && action != nil && !action.registered {
		return acp.RequestPermissionResponse{}, false, err
	}

	if resolveErr := action.resolve(ctx, permissionActionState(response, err)); resolveErr != nil {
		return acp.RequestPermissionResponse{}, true, resolveErr
	}

	return response, true, err
}

// createElicitationForMCPTool resolves a native MCP item/tool association to
// the exact published ACP tool ID. A lease keeps the selection stable while the
// registry is released before lifecycle and host calls.
func (s *session) createElicitationForMCPTool(
	ctx context.Context,
	conn agentClient,
	request acp.UnstableCreateElicitationRequest,
	nativeToolID string,
	params map[string]any,
) (acp.UnstableCreateElicitationResponse, bool, error) {
	turnNonce, active := s.activeTurnNonceForNativeTurn(codex.RequestTurnID(params))
	if !active || ctx.Err() != nil {
		return acp.UnstableCreateElicitationResponse{}, false, nil
	}

	registry := &s.permissionTools
	registry.mu.Lock()
	if err := registry.ensure(); err != nil {
		registry.mu.Unlock()

		return acp.UnstableCreateElicitationResponse{}, true, err
	}

	fingerprint := permissionFingerprint(nil, params, permissionToolMCP)

	record, valid := registry.matchPermissionTool(acp.ToolCallId(nativeToolID), permissionToolMCP, fingerprint)
	if !valid || record == nil {
		registry.mu.Unlock()

		return acp.UnstableCreateElicitationResponse{}, false, nil
	}

	registry.acquire(record)
	registry.mu.Unlock()

	releaseLease := sync.OnceFunc(func() { registry.release(record) })
	defer releaseLease()

	if err := registry.waitForStart(ctx, record); err != nil {
		return acp.UnstableCreateElicitationResponse{}, true, err
	}

	action, correlation, err := s.beginAction(ctx, lifecycle.ActionElicitation, true)
	if err != nil {
		return acp.UnstableCreateElicitationResponse{}, false, err
	}

	response, err := createElicitationWithAction(ctx, conn, request, elicitationScope{
		SessionID:         s.id,
		TurnNonce:         turnNonce,
		ToolCallID:        record.id,
		ActionCorrelation: correlation,
	}, action, releaseLease)
	if err != nil && action != nil && !action.registered {
		return acp.UnstableCreateElicitationResponse{}, false, err
	}

	if resolveErr := action.resolve(ctx, elicitationActionState(response, err)); resolveErr != nil {
		return acp.UnstableCreateElicitationResponse{}, true, resolveErr
	}

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
	event          codex.Event
	record         *permissionToolRecord
	transition     bool
	prependStart   bool
	publishesStart bool
}

func (r *permissionToolRegistry) recordForEvent(
	event codex.Event,
) (string, permissionToolClass, permissionToolFingerprint, *permissionToolRecord) {
	originalID := firstNonEmpty(event.Tool.ID, "codex-tool")

	var record *permissionToolRecord
	if canonical, ok := r.aliases[originalID]; ok {
		record = r.tools[canonical]
	}

	class := permissionClassForToolEvent(event.Tool)

	fingerprint := permissionFingerprint(nil, event.Tool.Raw, class)
	if fingerprint.title == "" {
		fingerprint.title = normalizePermissionToolValue(event.Tool.Title)
	}

	if record == nil && (event.Kind == codex.EventToolStarted || event.Kind == codex.EventToolCompleted) {
		record, _ = bestPermissionTool(r.tools, class, fingerprint, true)
	}

	return originalID, class, fingerprint, record
}

func (s *session) preparePermissionToolEvent(ctx context.Context, event codex.Event) (permissionToolEventPublication, error) {
	publication := permissionToolEventPublication{event: event}
	if event.Kind != codex.EventToolStarted && event.Kind != codex.EventToolDelta && event.Kind != codex.EventToolCompleted {
		return publication, nil
	}

	registry := &s.permissionTools
	registry.mu.Lock()

	if err := registry.ensure(); err != nil {
		registry.mu.Unlock()

		return publication, err
	}

	originalID, class, fingerprint, record := registry.recordForEvent(event)

	if record != nil {
		if event.Kind != codex.EventToolStarted && record.startDone != nil && !record.startSettled {
			registry.mu.Unlock()

			if err := registry.waitForStart(ctx, record); err != nil {
				return publication, err
			}

			return s.preparePermissionToolEvent(ctx, event)
		}

		if event.Kind == codex.EventToolCompleted && record.leases > 0 {
			registry.mu.Unlock()

			if err := registry.waitForLeases(ctx, record); err != nil {
				return publication, err
			}

			return s.preparePermissionToolEvent(ctx, event)
		}

		if record.startErr != nil {
			registry.mu.Unlock()

			return publication, record.startErr
		}

		publication.event.Tool.ID = string(record.id)
		publication.transition = event.Kind == codex.EventToolStarted && record.pendingNativeStart
	}

	switch publication.event.Kind {
	case codex.EventToolStarted:
		if record == nil {
			id := acp.ToolCallId(firstNonEmpty(publication.event.Tool.ID, "codex-tool"))

			record = newPermissionToolRecord(id, class, fingerprint)
			if err := registry.addTool(record); err != nil {
				registry.mu.Unlock()

				return publication, err
			}

			publication.publishesStart = true
		}

		record.pendingNativeStart = false

		record.fingerprint = mergePermissionFingerprint(record.fingerprint, fingerprint)
		if err := registry.addAlias(originalID, record.id); err != nil {
			registry.mu.Unlock()

			return publication, err
		}

		if err := registry.addAlias(string(record.id), record.id); err != nil {
			registry.mu.Unlock()

			return publication, err
		}
	case codex.EventToolDelta:
		if record == nil {
			id := acp.ToolCallId(firstNonEmpty(publication.event.Tool.ID, "codex-tool"))

			record = newPermissionToolRecord(id, class, fingerprint)
			if err := registry.addTool(record); err != nil {
				registry.mu.Unlock()

				return publication, err
			}

			publication.prependStart = true
			publication.publishesStart = true
		}

		if err := registry.addAlias(originalID, record.id); err != nil {
			registry.mu.Unlock()

			return publication, err
		}
	case codex.EventToolCompleted:
		if record == nil {
			id := acp.ToolCallId(firstNonEmpty(publication.event.Tool.ID, "codex-tool"))

			record = newPermissionToolRecord(id, class, fingerprint)
			if err := registry.addTool(record); err != nil {
				registry.mu.Unlock()

				return publication, err
			}

			publication.prependStart = true
			publication.publishesStart = true
		}

		record.pendingNativeStart = false
		record.terminal = true

		record.fingerprint = mergePermissionFingerprint(record.fingerprint, fingerprint)
		if err := registry.addAlias(originalID, record.id); err != nil {
			registry.mu.Unlock()

			return publication, err
		}

		if err := registry.addAlias(string(record.id), record.id); err != nil {
			registry.mu.Unlock()

			return publication, err
		}
	}

	publication.record = record
	registry.mu.Unlock()

	return publication, nil
}

func (p permissionToolEventPublication) updates(snapshots map[acp.ToolCallId][]acp.ToolCallContent) []acp.SessionUpdate {
	updates := make([]acp.SessionUpdate, 0, 2)

	if p.prependStart {
		tool := p.event.Tool
		updates = append(updates, acp.StartToolCall(
			acp.ToolCallId(tool.ID), firstNonEmpty(tool.Title, tool.ID),
			acp.WithStartKind(toolKind(tool)),
			acp.WithStartStatus(acp.ToolCallStatusInProgress),
			acp.WithStartRawInput(tool.Raw),
		))
	}

	if !p.transition {
		return append(updates, eventUpdatesWithToolSnapshots(p.event, snapshots)...)
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

	return append(updates, acp.UpdateToolCall(id, opts...))
}

func (p permissionToolEventPublication) finish(session *session, err error) {
	if p.publishesStart {
		session.permissionTools.completeStart(p.record, err)
	}
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
		tool:   normalizePermissionToolValue(deepPermissionString(raw, "toolName", "tool_name", "tool", "tool_title")),
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
