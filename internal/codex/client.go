package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// App-server method names the adapter calls.
const (
	methodInitialize        = "initialize"
	methodInitialized       = "initialized"
	methodThreadStart       = "thread/start"
	methodThreadResume      = "thread/resume"
	methodThreadUnsubscribe = "thread/unsubscribe"
	methodTurnStart         = "turn/start"
	methodTurnInterrupt     = "turn/interrupt"
	methodModelList         = "model/list"

	clientName = "acp-go-codex"
)

// Field names shared by requests and events.
const (
	fieldID       = "id"
	fieldName     = "name"
	fieldType     = "type"
	fieldPath     = "path"
	fieldStatus   = "status"
	fieldThreadID = "threadId"
	fieldTurnID   = "turnId"
	fieldItemID   = "itemId"
	fieldItem     = "item"
	fieldMessage  = "message"
	fieldResult   = "result"
	fieldCwd      = "cwd"
	fieldModel    = "model"
	fieldConfig   = "config"
)

// Thread is the native thread identity an app-server call returned.
type Thread struct {
	ID   string
	Path string
	// Model is the model the thread reports, when the response names one.
	Model string
}

// ThreadStartRequest is one thread/start call.
type ThreadStartRequest struct {
	Cwd                   string
	AdditionalDirectories []string
	Model                 string
	ServiceTier           string
	Personality           string
	ApprovalPolicy        any
	SandboxMode           any
	// Environment and ExtraPathDirs are applied to this thread alone through
	// its shell environment policy.
	Environment   map[string]string
	ExtraPathDirs []string
}

// ThreadResumeRequest is one thread/resume call.
type ThreadResumeRequest struct {
	ThreadID      string
	Cwd           string
	Environment   map[string]string
	ExtraPathDirs []string
}

// TurnStartRequest is one turn/start call.
type TurnStartRequest struct {
	ThreadID          string
	Input             []UserInput
	Model             string
	ServiceTier       string
	ReasoningEffort   string
	Personality       string
	ApprovalPolicy    any
	SandboxPolicy     any
	OutputSchema      any
	CollaborationMode any
}

// Model is one model/list entry.
type Model struct {
	ID                     string
	Name                   string
	Description            string
	ContextWindow          int64
	DefaultReasoningEffort string
	ReasoningEfforts       []string
	// InputModalities is the app-server's own list of input modalities; nil
	// means the field was absent.
	InputModalities []string
}

// Initialize performs the app-server handshake.
func (c *Client) Initialize(ctx context.Context) error {
	var resp map[string]any
	if err := c.Call(ctx, methodInitialize, map[string]any{
		"clientInfo":   map[string]any{fieldName: clientName, "title": clientName, "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &resp); err != nil {
		return err
	}

	return c.Notify(methodInitialized, map[string]any{})
}

// StartThread starts a new thread. nativePath is the PATH the app-server
// process itself runs with; the thread's PATH is composed ahead of it.
func (c *Client) StartThread(ctx context.Context, req ThreadStartRequest, nativePath string) (Thread, error) {
	params := map[string]any{}
	setNonEmpty(params, fieldCwd, req.Cwd)
	setNonEmpty(params, fieldModel, req.Model)
	setNonEmpty(params, "serviceTier", req.ServiceTier)
	setNonEmpty(params, "personality", req.Personality)
	setNonNil(params, "approvalPolicy", req.ApprovalPolicy)
	setNonNil(params, "sandbox", req.SandboxMode)

	if len(req.AdditionalDirectories) > 0 {
		params["permissions"] = permissionProfile(req.AdditionalDirectories)
	}

	config, err := threadSessionConfig(req.Environment, req.ExtraPathDirs, nativePath)
	if err != nil {
		return Thread{}, err
	}

	if len(config) > 0 {
		params[fieldConfig] = config
	}

	var resp map[string]any
	if err := c.Call(ctx, methodThreadStart, params, &resp); err != nil {
		return Thread{}, err
	}

	return threadFromResponse(resp)
}

// ResumeThread resumes a thread by id. The app-server resolves the id against
// the rollouts under its own home.
func (c *Client) ResumeThread(ctx context.Context, req ThreadResumeRequest, nativePath string) (Thread, error) {
	params := map[string]any{fieldThreadID: req.ThreadID}
	setNonEmpty(params, fieldCwd, req.Cwd)

	config, err := threadSessionConfig(req.Environment, req.ExtraPathDirs, nativePath)
	if err != nil {
		return Thread{}, err
	}

	if len(config) > 0 {
		params[fieldConfig] = config
	}

	var resp map[string]any
	if callErr := c.Call(ctx, methodThreadResume, params, &resp); callErr != nil {
		return Thread{}, callErr
	}

	thread, err := threadFromResponse(resp)
	if err != nil {
		return Thread{}, err
	}

	if thread.ID != req.ThreadID {
		return Thread{}, fmt.Errorf("codex thread/resume acknowledged thread %q instead of %q", thread.ID, req.ThreadID)
	}

	return thread, nil
}

// IsMissingThread recognizes the native refusal for a thread with no rollout.
func IsMissingThread(err error, threadID string) bool {
	var rpcErr *RPCError

	return errors.As(err, &rpcErr) && rpcErr.Code == -32600 &&
		rpcErr.Message == "no rollout found for thread id "+threadID
}

// UnsubscribeThread releases the app-server's thread subscription. An
// app-server without the method is not a failure.
func (c *Client) UnsubscribeThread(ctx context.Context, threadID string) error {
	err := c.Call(ctx, methodThreadUnsubscribe, map[string]any{fieldThreadID: threadID}, nil)
	if IsMethodNotFound(err) {
		return nil
	}

	return err
}

// StartTurn dispatches one turn and returns the native turn id the app-server
// named in its acknowledgement.
func (c *Client) StartTurn(ctx context.Context, req TurnStartRequest) (string, error) {
	params := map[string]any{fieldThreadID: req.ThreadID, "input": req.Input}
	setNonEmpty(params, fieldModel, req.Model)
	setNonEmpty(params, "serviceTier", req.ServiceTier)
	setNonEmpty(params, "effort", req.ReasoningEffort)
	setNonEmpty(params, "personality", req.Personality)
	setNonNil(params, "approvalPolicy", req.ApprovalPolicy)
	setNonNil(params, "sandboxPolicy", req.SandboxPolicy)
	setNonNil(params, "outputSchema", req.OutputSchema)
	setNonNil(params, "collaborationMode", req.CollaborationMode)

	var resp map[string]any
	if err := c.Call(ctx, methodTurnStart, params, &resp); err != nil {
		return "", err
	}

	turnID := stringValue(mapValue(resp, "turn"), fieldID)
	if turnID == "" {
		return "", errors.New("codex turn/start accepted a turn without naming it")
	}

	return turnID, nil
}

// InterruptTurn interrupts the thread's in-flight turn.
func (c *Client) InterruptTurn(ctx context.Context, threadID string, turnID string) error {
	params := map[string]any{fieldThreadID: threadID}
	setNonEmpty(params, fieldTurnID, turnID)

	return c.Call(ctx, methodTurnInterrupt, params, nil)
}

// ListModels reads the app-server's model catalog.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	var resp map[string]any
	if err := c.Call(ctx, methodModelList, map[string]any{}, &resp); err != nil {
		return nil, err
	}

	raw := mapSlice(resp, "models", "items", "data")

	models := make([]Model, 0, len(raw))

	for _, item := range raw {
		id := firstNonEmpty(stringValue(item, fieldID), stringValue(item, fieldModel), stringValue(item, fieldName))
		if id == "" {
			continue
		}

		efforts := make([]string, 0)

		for _, effort := range mapSlice(item, "supportedReasoningEfforts") {
			if value := stringValue(effort, "reasoningEffort"); value != "" {
				efforts = append(efforts, value)
			}
		}

		models = append(models, Model{
			ID:                     id,
			Name:                   firstNonEmpty(stringValue(item, "displayName"), stringValue(item, fieldName), id),
			Description:            stringValue(item, "description"),
			ContextWindow:          int64Value(item, "contextWindow"),
			DefaultReasoningEffort: stringValue(item, "defaultReasoningEffort"),
			ReasoningEfforts:       efforts,
			InputModalities:        stringSliceValue(item["inputModalities"]),
		})
	}

	return models, nil
}

func threadFromResponse(resp map[string]any) (Thread, error) {
	raw := mapValue(resp, "thread")

	thread := Thread{
		ID:    stringValue(raw, fieldID),
		Path:  stringValue(raw, fieldPath),
		Model: firstNonEmpty(stringValue(resp, fieldModel), stringValue(raw, fieldModel)),
	}
	if thread.ID == "" {
		return Thread{}, errors.New("codex thread response names no thread id")
	}

	// The rollout path is the adapter's only mirror source; a thread it cannot
	// read is never bound.
	if thread.Path == "" {
		return Thread{}, errors.New("codex thread response names no rollout path")
	}

	return thread, nil
}

func permissionProfile(additional []string) map[string]any {
	mods := make([]map[string]any, 0, len(additional))

	for _, path := range additional {
		if strings.TrimSpace(path) == "" {
			continue
		}

		mods = append(mods, map[string]any{fieldType: "additionalWritableRoot", fieldPath: path})
	}

	return map[string]any{fieldType: "profile", fieldID: ":workspace", "modifications": mods}
}

func setNonEmpty(values map[string]any, key string, value string) {
	if value != "" {
		values[key] = value
	}
}

func setNonNil(values map[string]any, key string, value any) {
	if text, ok := value.(string); ok && text == "" {
		return
	}

	if value != nil {
		values[key] = value
	}
}

func mapValue(values map[string]any, key string) map[string]any {
	if values == nil {
		return nil
	}

	child, _ := values[key].(map[string]any)

	return child
}

func mapSlice(values map[string]any, keys ...string) []map[string]any {
	for _, key := range keys {
		raw, ok := values[key].([]any)
		if !ok {
			continue
		}

		out := make([]map[string]any, 0, len(raw))

		for _, item := range raw {
			if obj, ok := item.(map[string]any); ok {
				out = append(out, obj)
			}
		}

		return out
	}

	return nil
}

func stringValue(values map[string]any, key string) string {
	if values == nil {
		return ""
	}

	value, _ := values[key].(string)

	return value
}

func int64Value(values map[string]any, key string) int64 {
	if values == nil {
		return 0
	}

	switch value := values[key].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return 0
	}
}

func stringSliceValue(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))

		for _, item := range typed {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}

		return out
	default:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
