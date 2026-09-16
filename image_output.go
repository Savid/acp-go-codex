package codexacp

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-codex/internal/codex"
	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-core/wire"
)

// readOutputImage reads a harness-returned file through the core output gate
// within the allowed roots.
func (s *session) readOutputImage(path string, limit int64) (image.Output, *image.OutputError) {
	data, mime, failure := image.ReadFile(path, s.outputRoots(), limit)
	if failure != nil {
		return image.Output{}, failure
	}

	return image.Output{Data: base64.StdEncoding.EncodeToString(data), MIME: mime, SizeBytes: int64(len(data))}, nil
}

// outputRoots are the directories a native image path may be read from: the
// workspace, the adapter's scratch parent, the system temp directory, and
// Codex's own generated-images directory.
func (s *session) outputRoots() []string {
	roots := []string{s.cwd}

	if scratch := s.agent.options.ScratchDir; scratch != "" {
		roots = append(roots, scratch)
	}

	if temp := os.TempDir(); temp != "" {
		roots = append(roots, temp)
	}

	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()

	if rt != nil && rt.home != "" {
		roots = append(roots, filepath.Join(rt.home, "generated_images"))
	}

	return roots
}

// toolState is the exact-id lifecycle published for one native tool call.
type toolState struct {
	published bool
	terminal  bool
	// content is the last emitted complete content array; each later
	// content-bearing update extends it so no delivered item disappears
	// under ACP's whole-array replacement.
	content []acp.ToolCallContent
}

func (state *cycleState) tool(id string) *toolState {
	if state.tools == nil {
		state.tools = make(map[string]*toolState)
	}

	tool := state.tools[id]
	if tool == nil {
		tool = &toolState{}
		state.tools[id] = tool
	}

	return tool
}

// publishPendingTool announces a tool call that is awaiting approval before
// the app-server reports the item started.
func (s *session) publishPendingTool(ctx context.Context, state *cycleState, request permissionRequest) error {
	state.toolsMu.Lock()
	defer state.toolsMu.Unlock()

	tool := state.tool(request.toolCallID)
	if tool.published {
		return nil
	}

	opts := []acp.ToolCallStartOpt{
		acp.WithStartKind(request.kind),
		acp.WithStartStatus(acp.ToolCallStatusPending),
		acp.WithStartRawInput(request.rawInput),
	}
	if len(request.content) > 0 {
		opts = append(opts, acp.WithStartContent(request.content))
	}

	if err := s.emit(ctx, acp.StartToolCall(acp.ToolCallId(request.toolCallID), request.title, opts...)); err != nil {
		return err
	}

	tool.published = true

	return nil
}

func (s *session) publishToolStart(ctx context.Context, state *cycleState, event codex.ToolEvent) error {
	state.toolsMu.Lock()
	defer state.toolsMu.Unlock()

	id := firstNonEmpty(event.ID, "codex-tool")

	tool := state.tool(id)
	if tool.terminal {
		return nil
	}

	kind := toolKind(event.Kind)

	if tool.published {
		return s.emit(ctx, acp.UpdateToolCall(acp.ToolCallId(id),
			acp.WithUpdateTitle(event.Title), acp.WithUpdateKind(kind),
			acp.WithUpdateStatus(acp.ToolCallStatusInProgress), acp.WithUpdateRawInput(event.Raw)))
	}

	if err := s.emit(ctx, acp.StartToolCall(acp.ToolCallId(id), event.Title,
		acp.WithStartKind(kind), acp.WithStartStatus(acp.ToolCallStatusInProgress), acp.WithStartRawInput(event.Raw))); err != nil {
		return err
	}

	tool.published = true

	return nil
}

// publishToolDelta appends one output delta and emits the complete content
// array.
func (s *session) publishToolDelta(ctx context.Context, state *cycleState, id string, text string) error {
	state.toolsMu.Lock()
	defer state.toolsMu.Unlock()

	if text == "" {
		return nil
	}

	tool := state.tool(firstNonEmpty(id, "codex-tool"))
	if tool.terminal {
		return nil
	}

	tool.content = append(tool.content, acp.ToolContent(acp.TextBlock(text)))

	return s.emit(ctx, acp.UpdateToolCall(acp.ToolCallId(firstNonEmpty(id, "codex-tool")), acp.WithUpdateContent(append([]acp.ToolCallContent(nil), tool.content...))))
}

// publishToolTerminal emits the terminal status and the complete final
// content array: the aggregated output, and for file changes their diffs.
func (s *session) publishToolTerminal(ctx context.Context, state *cycleState, event codex.ToolEvent) error {
	state.toolsMu.Lock()
	defer state.toolsMu.Unlock()

	id := firstNonEmpty(event.ID, "codex-tool")

	tool := state.tool(id)
	if tool.terminal {
		return nil
	}

	status := acp.ToolCallStatusCompleted
	if event.Status == itemStatusFailed || event.Status == itemStatusDeclined {
		status = acp.ToolCallStatusFailed
	}

	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(status), acp.WithUpdateKind(toolKind(event.Kind)), acp.WithUpdateRawOutput(event.Raw)}
	if event.Title != "" {
		opts = append(opts, acp.WithUpdateTitle(event.Title))
	}

	if len(tool.content) == 0 && event.Content != "" {
		tool.content = append(tool.content, acp.ToolContent(acp.TextBlock(event.Content)))
	}

	tool.content = append(tool.content, diffContent(event.Raw)...)

	if len(tool.content) > 0 {
		opts = append(opts, acp.WithUpdateContent(append([]acp.ToolCallContent(nil), tool.content...)))
	}

	if !tool.published {
		if err := s.emit(ctx, acp.StartToolCall(acp.ToolCallId(id), event.Title, acp.WithStartKind(toolKind(event.Kind)), acp.WithStartRawInput(event.Raw))); err != nil {
			return err
		}

		tool.published = true
	}

	tool.terminal = true

	return s.emit(ctx, acp.UpdateToolCall(acp.ToolCallId(id), opts...))
}

// diffContent renders a file-change item's changes as diff content.
func diffContent(item map[string]any) []acp.ToolCallContent {
	changes, _ := item["changes"].([]any)
	content := make([]acp.ToolCallContent, 0, len(changes))

	for _, raw := range changes {
		change, _ := raw.(map[string]any)

		path, _ := change["path"].(string)
		diff, _ := change["diff"].(string)

		if diff == "" {
			continue
		}

		content = append(content, acp.ToolCallContent{Diff: &acp.ToolCallContentDiff{Path: path, NewText: diff}})
	}

	return content
}

// publishImageStart announces an image generation item as a tool call.
func (s *session) publishImageStart(ctx context.Context, state *cycleState, event codex.ImageEvent) error {
	state.toolsMu.Lock()
	defer state.toolsMu.Unlock()

	id := firstNonEmpty(event.ID, "codex-image")

	tool := state.tool(id)
	if tool.published || tool.terminal {
		return nil
	}

	if err := s.emit(ctx, acp.StartToolCall(acp.ToolCallId(id), imageToolTitle(event.Kind),
		acp.WithStartKind(acp.ToolKindOther), acp.WithStartStatus(acp.ToolCallStatusInProgress), acp.WithStartRawInput(event.Raw))); err != nil {
		return err
	}

	tool.published = true

	return nil
}

// publishImageTerminal decodes an image item's bytes, inline or from an
// allowed path, and emits them as the tool call's content. A refusal is
// reported in place as the failed call's own content and the turn continues.
func (s *session) publishImageTerminal(ctx context.Context, state *cycleState, event codex.ImageEvent) error {
	state.toolsMu.Lock()
	defer state.toolsMu.Unlock()

	id := firstNonEmpty(event.ID, "codex-image")

	tool := state.tool(id)
	if tool.terminal {
		return nil
	}

	if !tool.published {
		if err := s.emit(ctx, acp.StartToolCall(acp.ToolCallId(id), imageToolTitle(event.Kind),
			acp.WithStartKind(acp.ToolKindOther), acp.WithStartStatus(acp.ToolCallStatusInProgress), acp.WithStartRawInput(event.Raw))); err != nil {
			return err
		}

		tool.published = true
	}

	tool.terminal = true

	if event.Status == itemStatusFailed {
		return s.emit(ctx, acp.UpdateToolCall(acp.ToolCallId(id), acp.WithUpdateStatus(acp.ToolCallStatusFailed), acp.WithUpdateRawOutput(event.Raw)))
	}

	limits := s.agent.options.ImageLimits.core()

	var (
		output  image.Output
		failure *image.OutputError
	)

	switch {
	case event.Result != "":
		output, failure = image.DecodeOutput(event.Result, "", limits.EffectiveOutputPerImage())
	case event.SavedPath != "":
		output, failure = s.readOutputImage(event.SavedPath, limits.EffectiveOutputPerImage())
	default:
		failure = &image.OutputError{Reason: image.ReasonMissingFile, Message: "image output carried neither bytes nor a path"}
	}

	if failure == nil && output.SizeBytes > limits.EffectiveOutputPerToolCall() {
		failure = &image.OutputError{
			Reason: image.ReasonTooLarge, Message: "tool call image content exceeds the per-tool-call limit",
			SizeBytes: output.SizeBytes, MaxBytes: limits.EffectiveOutputPerToolCall(),
		}
	}

	if failure != nil {
		guidance, recoverable := failure.Guidance()
		opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(acp.ToolCallStatusFailed)}

		if recoverable {
			opts = append(opts, acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(guidance))}))
		}

		err := s.emit(ctx, acp.UpdateToolCall(acp.ToolCallId(id), opts...))
		if recoverable {
			return err
		}

		return wire.TurnFailed(vendor, failure.TurnFailure())
	}

	s.mu.Lock()
	s.images = append(s.images, storedImage{ID: id, Kind: event.Kind, Data: output.Data, MIME: output.MIME})
	s.mu.Unlock()

	tool.content = []acp.ToolCallContent{acp.ToolContent(acp.ImageBlock(output.Data, output.MIME))}
	state.imagesEmitted = true

	return s.emit(ctx, acp.UpdateToolCall(acp.ToolCallId(id), acp.WithUpdateStatus(acp.ToolCallStatusCompleted), acp.WithUpdateContent(tool.content)))
}

func imageToolTitle(kind string) string {
	if kind == itemTypeImageView {
		return "View image"
	}

	return "Generate image"
}

func toolKind(itemType string) acp.ToolKind {
	switch itemType {
	case itemTypeCommandExecution:
		return acp.ToolKindExecute
	case itemTypeFileChange:
		return acp.ToolKindEdit
	case itemTypeWebSearch:
		return acp.ToolKindSearch
	default:
		return acp.ToolKindOther
	}
}
