package codex

import (
	"context"
	"encoding/json"
)

// Client is the provider boundary used by the ACP layer.
type Client interface {
	StartThread(context.Context, ThreadStartRequest) (Thread, error)
	ResumeThread(context.Context, ThreadResumeRequest) (Thread, error)
	ForkThread(context.Context, ThreadForkRequest) (Thread, error)
	ListThreads(context.Context, ThreadListRequest) ([]Thread, error)
	ReadThread(context.Context, ThreadReadRequest) (ThreadHistory, error)
	ListTurns(context.Context, ThreadTurnsListRequest) (ThreadTurnsListResponse, error)
	RunTurn(context.Context, TurnStartRequest) (<-chan Event, error)
	SteerTurn(context.Context, TurnSteerRequest) error
	CancelTurn(context.Context, string, string) error
	CompactThread(context.Context, ThreadCompactRequest) (map[string]any, error)
	StartReview(context.Context, ReviewStartRequest) (map[string]any, error)
	CollaborationModeList(context.Context) (CollaborationModeListResponse, error)
	MCPServerStatusList(context.Context, string) (MCPServerStatusListResponse, error)
	DeleteThread(context.Context, ThreadDeleteRequest) error
	UnsubscribeThread(context.Context, string) error
	ModelList(context.Context) ([]Model, error)
	AccountRead(context.Context) (Account, error)
	RateLimitsSupported() bool
	ReadRateLimits(context.Context) (RateLimitSnapshot, error)
	LoginWithChatGPTTokens(context.Context, ChatGPTAuthTokens) error
	Logout(context.Context) error
	Close(context.Context) error
}

// BackgroundTerminalClient is the optional experimental provider surface used
// to contain native command processes without retiring the shared app-server
// generation. Callers must scope every operation to the owning Codex thread.
type BackgroundTerminalClient interface {
	ListBackgroundTerminals(context.Context, BackgroundTerminalListRequest) (BackgroundTerminalListResponse, error)
	TerminateBackgroundTerminal(context.Context, BackgroundTerminalTerminateRequest) (bool, error)
}

type RequestHandler func(context.Context, ServerRequest) (any, error)

type ServerRequest struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

type ThreadStartRequest struct {
	Cwd                   string
	AdditionalDirectories []string
	Model                 string
	ModelProvider         string
	ApprovalPolicy        any
	Sandbox               any
	ServiceTier           string
	DeveloperInstructions string
	Personality           any
	Ephemeral             *bool
	Config                map[string]any
}

type ThreadResumeRequest struct {
	ThreadID string
	Path     string
	Cwd      string
	Config   map[string]any
}

type ThreadForkRequest struct {
	ThreadID string
	Cwd      string
}

type ThreadListRequest struct {
	Cwd string
}

type ThreadReadRequest struct {
	ThreadID string
}

type ThreadTurnsListRequest struct {
	ThreadID      string
	Cursor        string
	Limit         int
	SortDirection string
}

type ThreadTurnsListResponse struct {
	Turns      []map[string]any
	NextCursor string
	Raw        map[string]any
}

type BackgroundTerminalListRequest struct {
	ThreadID string
	Cursor   string
	Limit    int
}

type BackgroundTerminalListResponse struct {
	Terminals  []BackgroundTerminal
	NextCursor string
}

type BackgroundTerminalTerminateRequest struct {
	ThreadID  string
	ProcessID string
}

type BackgroundTerminal struct {
	ItemID    string
	ProcessID string
	OSPID     *int64
	Raw       map[string]any
}

type Thread struct {
	ID              string
	SessionID       string
	Path            string
	Cwd             string
	Model           string
	Provider        string
	ReasoningEffort string
	Title           string
	UpdatedAt       string
	Raw             map[string]any
}

type ThreadHistory struct {
	Thread Thread
	Items  []map[string]any
	Events []Event
	Raw    map[string]any
}

type TurnStartRequest struct {
	ThreadID          string
	Prompt            []UserInput
	Model             string
	ServiceTier       string
	ReasoningEffort   string
	Personality       any
	ApprovalPolicy    any
	SandboxPolicy     any
	OutputSchema      any
	CollaborationMode any
}

type TurnSteerRequest struct {
	ThreadID       string
	ExpectedTurnID string
	Input          []UserInput
}

type ThreadCompactRequest struct {
	ThreadID string
}

type ThreadDeleteRequest struct {
	ThreadID string
}

type ReviewStartRequest struct {
	ThreadID string
	Target   map[string]any
	Delivery string
}

type CollaborationModeListResponse struct {
	Modes []CollaborationMode
	Raw   map[string]any
}

type CollaborationMode struct {
	ID   string
	Name string
	Raw  map[string]any
}

type MCPServerStatusListResponse struct {
	Servers []MCPServerStatus
	Raw     map[string]any
}

type MCPServerStatus struct {
	Name      string
	Status    string
	Tools     []map[string]any
	Resources []map[string]any
	Templates []map[string]any
	Raw       map[string]any
}

type UserInput map[string]any

type ModelReasoningEffort struct {
	ID          string
	Description string
	Raw         map[string]any
}

type Model struct {
	ID                     string
	Name                   string
	Description            string
	Context                int64
	DefaultReasoningEffort string
	ReasoningEfforts       []ModelReasoningEffort
	// InputModalities is the app-server's authoritative list of input
	// modalities for the model. nil means the field was absent upstream.
	InputModalities []string
	Raw             map[string]any
}

type EventKind string

const (
	EventAgentMessageDelta EventKind = "agent_message_delta"
	EventReasoningDelta    EventKind = "reasoning_delta"
	EventPlanUpdated       EventKind = "plan_updated"
	EventToolStarted       EventKind = "tool_started"
	EventToolDelta         EventKind = "tool_delta"
	EventToolCompleted     EventKind = "tool_completed"
	EventImageStarted      EventKind = "image_started"
	EventImageCompleted    EventKind = "image_completed"
	EventDiffUpdated       EventKind = "diff_updated"
	EventUsageUpdated      EventKind = "usage_updated"
	EventAccountUpdated    EventKind = "account_updated"
	EventRateLimitsUpdated EventKind = "rate_limits_updated"
	EventRaw               EventKind = "raw"
	EventWarning           EventKind = "warning"
	EventError             EventKind = "error"
	EventCompleted         EventKind = "completed"
)

type PlanStepStatus string

const (
	PlanStepPending    PlanStepStatus = "pending"
	PlanStepInProgress PlanStepStatus = "in_progress"
	PlanStepCompleted  PlanStepStatus = "completed"
)

type PlanStep struct {
	Text   string
	Status PlanStepStatus
}

type StopReason string

const (
	StopReasonEndTurn   StopReason = "end_turn"
	StopReasonCancelled StopReason = "cancelled"
	StopReasonError     StopReason = "error"
)

type Event struct {
	Kind       EventKind
	ThreadID   string
	TurnID     string
	ItemID     string
	Text       string
	Diff       string
	Plan       []PlanStep
	Tool       ToolEvent
	Image      ImageEvent
	StopReason StopReason
	Usage      Usage
	TokenUsage TokenUsage
	Account    Account
	RateLimits *RateLimitSnapshot
	Completed  bool
	RawMethod  string
	RawParams  json.RawMessage
	RawJSON    string
	Err        error
}

type ImageEvent struct {
	ID            string
	Kind          string
	Status        string
	Result        string
	SavedPath     string
	RevisedPrompt string
	ArtifactRef   string
	Raw           map[string]any
}

type ToolEvent struct {
	ID        string
	Title     string
	Kind      string
	Status    string
	Locations []string
	Content   string
	Raw       map[string]any
}

type Usage struct {
	CachedReadTokens      int64
	CachedWriteTokens     int64
	InputTokens           int64
	OutputTokens          int64
	ReasoningOutputTokens int64
	TotalTokens           int64
}

type TokenUsage struct {
	Last               Usage
	Total              Usage
	ModelContextWindow int64
	Raw                map[string]any
}

type Account struct {
	ID       string
	Email    string
	PlanType string
	Raw      map[string]any
}

type ChatGPTAuthTokens struct {
	AccessToken      string
	RefreshToken     string
	AccountID        string
	PlanType         string
	ExpiresAtUnixSec int64
}
