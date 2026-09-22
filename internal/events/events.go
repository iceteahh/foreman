// Package events decodes the NDJSON stream `claude -p --output-format stream-json`
// writes to stdout. Field names follow docs/cli-contract.md §3. Unknown fields and
// unknown event types are preserved as raw JSON, never dropped.
package events

import (
	"encoding/json"
	"strings"
)

// Event types seen on the stream.
const (
	TypeSystem      = "system"
	TypeAssistant   = "assistant"
	TypeUser        = "user"
	TypeStreamEvent = "stream_event"
	TypeRateLimit   = "rate_limit_event"
	TypeResult      = "result"
	// TypeRaw marks a stdout line that was not JSON (kept for the audit log).
	TypeRaw = "raw"
)

// System subtypes the runner reacts to.
const (
	SubtypeInit     = "init"
	SubtypeStatus   = "status"
	SubtypeAPIRetry = "api_retry" // {attempt, max_retries, retry_delay_ms, error_status, error}
)

// Terminal reasons seen on result events.
const (
	ReasonCompleted       = "completed"
	ReasonBudgetExhausted = "budget_exhausted"
	ReasonAPIError        = "api_error"
	ReasonMaxTurns        = "max_turns"
)

// Event is one decoded line. Exactly one of the typed pointers is set for known
// types; Raw always holds the original bytes.
type Event struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	UUID      string `json:"uuid,omitempty"`

	System    *SystemEvent    `json:"-"`
	Assistant *AssistantEvent `json:"-"`
	Result    *ResultEvent    `json:"-"`
	RateLimit *RateLimitEvent `json:"-"`

	// Raw is the exact line as received (without the trailing newline).
	Raw json.RawMessage `json:"-"`
	// Line is the 1-based stdout line number.
	Line int `json:"-"`
}

// SystemEvent covers subtypes init, thinking_tokens, and anything future.
type SystemEvent struct {
	Subtype           string   `json:"subtype"`
	CWD               string   `json:"cwd,omitempty"`
	Tools             []string `json:"tools,omitempty"`
	Model             string   `json:"model,omitempty"`
	PermissionMode    string   `json:"permissionMode,omitempty"`
	APIKeySource      string   `json:"apiKeySource,omitempty"`
	ClaudeCodeVersion string   `json:"claude_code_version,omitempty"`
	MCPServers        []any    `json:"mcp_servers,omitempty"`
	EstimatedTokens   int      `json:"estimated_tokens,omitempty"`
	// status subtype
	Status string `json:"status,omitempty"`
	// api_retry subtype (fixture stream_auth_failed_401.ndjson)
	Attempt      int    `json:"attempt,omitempty"`
	MaxRetries   int    `json:"max_retries,omitempty"`
	RetryDelayMS int    `json:"retry_delay_ms,omitempty"`
	ErrorStatus  int    `json:"error_status,omitempty"`
	Error        string `json:"error,omitempty"`
}

// IsAuthFailure reports an api_retry caused by a rejected credential. The CLI
// retries these ten times (~3 minutes) before giving up, so the runner aborts early.
func (s *SystemEvent) IsAuthFailure() bool {
	return s != nil && s.Subtype == SubtypeAPIRetry && (s.ErrorStatus == 401 || s.ErrorStatus == 403)
}

// IsRateLimited reports an api_retry caused by HTTP 429. The 429 shape is not
// yet captured from the CLI (cli-contract §4); the field names are those of
// the verified 401 fixture, which the CLI emits for every retried status.
func (s *SystemEvent) IsRateLimited() bool {
	return s != nil && s.Subtype == SubtypeAPIRetry && (s.ErrorStatus == 429 || s.ErrorStatus == 529)
}

// AssistantEvent wraps one API message (possibly a partial repeat of the same message id).
type AssistantEvent struct {
	Message         Message `json:"message"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	RequestID       string  `json:"request_id,omitempty"`
	Timestamp       string  `json:"timestamp,omitempty"`
}

// Message is the API message shape.
type Message struct {
	ID         string         `json:"id"`
	Model      string         `json:"model"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	StopReason *string        `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// ContentBlock is text, thinking, tool_use, or tool_result.
type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// Usage is the token accounting on assistant messages and the result.
// There is no cost field mid-stream (cli-contract #18).
type Usage struct {
	InputTokens              int           `json:"input_tokens"`
	OutputTokens             int           `json:"output_tokens"`
	CacheCreationInputTokens int           `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int           `json:"cache_read_input_tokens"`
	CacheCreation            CacheCreation `json:"cache_creation"`
}

// CacheCreation splits cache writes by TTL (different prices).
type CacheCreation struct {
	Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
}

// RateLimitEvent reports the account's rate-limit windows. Every captured
// fixture shows status "allowed"; anything else is treated as a limit.
type RateLimitEvent struct {
	Info struct {
		Status        string `json:"status"`
		ResetsAt      int64  `json:"resetsAt"`
		RateLimitType string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
}

// Limited reports whether the account is currently rate limited.
func (r *RateLimitEvent) Limited() bool {
	return r != nil && r.Info.Status != "" && r.Info.Status != "allowed"
}

// PermissionDenial is one entry of result.permission_denials.
type PermissionDenial struct {
	Tool  string          `json:"tool_name"`
	ID    string          `json:"tool_use_id,omitempty"`
	Input json.RawMessage `json:"tool_input,omitempty"`
}

// ModelUsage is one entry of result.modelUsage.
type ModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

// ResultEvent is the final event. Classify on IsError + TerminalReason, never Subtype alone.
type ResultEvent struct {
	Subtype           string                `json:"subtype"`
	IsError           bool                  `json:"is_error"`
	TerminalReason    string                `json:"terminal_reason"`
	APIErrorStatus    *int                  `json:"api_error_status"`
	SessionID         string                `json:"session_id"`
	NumTurns          int                   `json:"num_turns"`
	StopReason        string                `json:"stop_reason"`
	TotalCostUSD      float64               `json:"total_cost_usd"`
	DurationMS        int64                 `json:"duration_ms"`
	DurationAPIMS     int64                 `json:"duration_api_ms"`
	PermissionDenials []PermissionDenial    `json:"permission_denials"`
	Result            *string               `json:"result"`
	StructuredOutput  json.RawMessage       `json:"structured_output,omitempty"`
	Errors            []string              `json:"errors,omitempty"`
	Usage             Usage                 `json:"usage"`
	ModelUsage        map[string]ModelUsage `json:"modelUsage"`
}

// Text returns result.result or "".
func (r *ResultEvent) Text() string {
	if r == nil || r.Result == nil {
		return ""
	}
	return *r.Result
}

// Outcome is the harness's classification of a run's end.
type Outcome string

const (
	OutcomeCompleted       Outcome = "completed"
	OutcomeBudgetExhausted Outcome = "budget_exhausted"
	OutcomeAPIError        Outcome = "api_error"
	OutcomeMaxTurns        Outcome = "max_turns"
	OutcomeError           Outcome = "error" // is_error with an unrecognised reason
	OutcomeCrash           Outcome = "crash" // process exited without a result event
)

// Classify maps a result event to an Outcome (cli-contract #12, #13).
func Classify(r *ResultEvent) Outcome {
	if r == nil {
		return OutcomeCrash
	}
	switch r.TerminalReason {
	case ReasonBudgetExhausted:
		return OutcomeBudgetExhausted
	case ReasonAPIError:
		return OutcomeAPIError
	case ReasonMaxTurns:
		return OutcomeMaxTurns
	}
	// Subtype is only consulted once terminal_reason has said nothing. The
	// pinned CLI always sets terminal_reason (result_max_turns.json carries
	// both), so these two lines are unreachable against it and exist for a
	// build that predates the field — a downgrade, or a fixture captured
	// before it existed. They are never the primary signal: classifying on
	// subtype alone is what conflated a budget stop with a crash.
	if strings.Contains(r.Subtype, "max_turns") {
		return OutcomeMaxTurns
	}
	if strings.Contains(r.Subtype, "max_budget") {
		return OutcomeBudgetExhausted
	}
	if r.IsError {
		return OutcomeError
	}
	if r.TerminalReason == ReasonCompleted || r.TerminalReason == "" {
		return OutcomeCompleted
	}
	return OutcomeError
}

// Parse decodes one stdout line. Non-JSON lines become TypeRaw events.
func Parse(line []byte, lineNo int) *Event {
	raw := make(json.RawMessage, len(line))
	copy(raw, line)
	ev := &Event{Raw: raw, Line: lineNo, Type: TypeRaw}
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" || trimmed[0] != '{' {
		return ev
	}
	var head struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		UUID      string `json:"uuid"`
	}
	if err := json.Unmarshal(line, &head); err != nil || head.Type == "" {
		return ev
	}
	ev.Type, ev.Subtype, ev.SessionID, ev.UUID = head.Type, head.Subtype, head.SessionID, head.UUID
	switch head.Type {
	case TypeSystem:
		ev.System = &SystemEvent{}
		_ = json.Unmarshal(line, ev.System)
	case TypeAssistant:
		ev.Assistant = &AssistantEvent{}
		_ = json.Unmarshal(line, ev.Assistant)
	case TypeResult:
		ev.Result = &ResultEvent{}
		_ = json.Unmarshal(line, ev.Result)
	case TypeRateLimit:
		ev.RateLimit = &RateLimitEvent{}
		_ = json.Unmarshal(line, ev.RateLimit)
	}
	return ev
}

// ToolUses returns the tool_use blocks in an assistant event.
func (a *AssistantEvent) ToolUses() []ContentBlock {
	if a == nil {
		return nil
	}
	var out []ContentBlock
	for _, c := range a.Message.Content {
		if c.Type == "tool_use" {
			out = append(out, c)
		}
	}
	return out
}
