// Package model holds the chat's domain types and errors. It imports no other internal package
// (ARC-002).
package model

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// The conditions a caller branches on (ERR-005); the controller maps them to status codes.
var (
	ErrInvalid     = errors.New("invalid input")
	ErrTooLarge    = errors.New("input too large")
	ErrNotFound    = errors.New("not found")
	ErrUnavailable = errors.New("the model server did not answer")
	ErrRejected    = errors.New("the model server rejected the request")
	// ErrForbidden is a request the server refuses to serve whatever its body: one from another
	// site's page or for another host name.
	ErrForbidden = errors.New("forbidden")
	// ErrUnsupportedMediaType is a body that is not JSON.
	ErrUnsupportedMediaType = errors.New("unsupported media type")
)

// InvalidInputError is an ErrInvalid whose Message says the user what to change; the controller
// shows it as it is.
type InvalidInputError struct {
	Message string
}

func (e *InvalidInputError) Error() string { return "invalid input: " + e.Message }

// Unwrap makes the error match ErrInvalid.
func (e *InvalidInputError) Unwrap() error { return ErrInvalid }

// Persona is a system prompt a message may choose by its ID: Label and Description are what a
// user picks it by, Prompt is what the model is told.
type Persona struct {
	ID          string
	Label       string
	Description string
	Prompt      string
}

// ChatEventKind says what a ChatEvent carries.
type ChatEventKind string

// The kinds: a piece of the answer's text, a piece of the model's reasoning, a tool call, its
// result, the usage of one model call and the end of the turn.
const (
	ChatEventText       ChatEventKind = "text"
	ChatEventThinking   ChatEventKind = "thinking"
	ChatEventToolCall   ChatEventKind = "tool_call"
	ChatEventToolResult ChatEventKind = "tool_result"
	ChatEventUsage      ChatEventKind = "usage"
	ChatEventEnd        ChatEventKind = "end"
)

// ChatEndReason says why a turn ended.
type ChatEndReason string

// The reasons: the model answered, it used up its model calls, the turn failed, ran out of time
// or was cancelled by the caller.
const (
	ChatEndReasonStop         ChatEndReason = "stop"
	ChatEndReasonMaximumTurns ChatEndReason = "max_turns"
	ChatEndReasonError        ChatEndReason = "error"
	ChatEndReasonTimeout      ChatEndReason = "timeout"
	ChatEndReasonCancelled    ChatEndReason = "cancelled"
)

// ChatUsage is what one model call of a turn cost and how fast it ran. InputTokens include the
// CachedInputTokens the server read from its prompt cache; OutputTokens include the reasoning.
// Latency is the time to the first output token as the client saw it; Duration runs to the end
// of the generation; TokensPerSecond is OutputTokens over Duration minus Latency, 0 when too
// short to measure. Server is what the model server measured itself, nil when it sends nothing.
type ChatUsage struct {
	// ContextLength is the model's context window in tokens, 0 when the model server does not say.
	ContextLength     int
	InputTokens       int
	CachedInputTokens int
	OutputTokens      int
	Latency           time.Duration
	Duration          time.Duration
	TokensPerSecond   float64
	Server            *ServerTimings
}

// ServerTimings is a model call as the model server measured it (llama.cpp's timings): the
// prompt processing of the PromptTokens it read, the cached ones not counted, and the
// generation's rate. DraftTokens were proposed by speculative decoding, DraftAccepted of them
// kept.
type ServerTimings struct {
	PromptProcessing          time.Duration
	PromptTokens              int
	PromptTokensPerSecond     float64
	GenerationTokensPerSecond float64
	DraftTokens               int
	DraftAccepted             int
}

// ChatEvent is one event of a turn. Text carries a piece of the answer or of the reasoning. A
// tool call and its result share CallID; Target is the file or pattern a call names, empty when
// it names none. A result is OK or names its Error. A usage event carries Usage. The end names
// its Reason and, for ChatEndReasonError and ChatEndReasonTimeout, Err.
type ChatEvent struct {
	Kind   ChatEventKind
	Text   string
	CallID string
	Tool   string
	Target string
	OK     bool
	Error  string
	Usage  *ChatUsage
	Reason ChatEndReason
	Err    error
}

// ServedModel is a model of the model server: its name and its context window in tokens, 0 when
// the server does not name it.
type ServedModel struct {
	Name          string
	ContextLength int
}

// Thinking is how long the model reasons before it answers: off, or a level of effort.
type Thinking string

// The levels a message may ask for. Off answers at once.
const (
	ThinkingOff    Thinking = "off"
	ThinkingLow    Thinking = "low"
	ThinkingMedium Thinking = "medium"
	ThinkingHigh   Thinking = "high"
)

// Valid says whether t is one of the levels.
func (t Thinking) Valid() bool {
	switch t {
	case ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh:
		return true
	default:
		return false
	}
}

// ChatTurn is one message of a conversation with what the model answers it with. Workspace
// offers the model the tools of Directory for this turn (the file tools, the shell and the host's
// go command), Directory being an absolute host path with its symbolic links resolved; Thinking
// is how long the model reasons first.
type ChatTurn struct {
	ConversationID uuid.UUID
	Text           string
	Model          string
	System         string
	Workspace      bool
	Directory      string
	Thinking       Thinking
}

// DirectoryListing is a directory a user may choose for the tools: its absolute Path, its Parent
// ("" at a root, where a user cannot go up), the Roots and the names of its visible
// subdirectories, sorted.
type DirectoryListing struct {
	Path        string
	Parent      string
	Roots       []string
	Directories []string
}
