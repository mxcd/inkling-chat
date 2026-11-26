package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	wideevent "github.com/govengers/wide-event-logger"
	wideeventginadapter "github.com/govengers/wide-event-logger/ginadapter"

	"github.com/mxcd/inkling-chat/internal/model"
	"github.com/mxcd/inkling-chat/internal/service"
)

// defaultStreamTimeout is how long one answer may stream: above the agent's two hours for a
// turn, so the turn's own end arrives before the connection's.
const defaultStreamTimeout = 2*time.Hour + time.Minute

// streamErrorEvent is the stream's event for a turn that failed after the stream began; its
// data is an ErrorResponse.
const streamErrorEvent = "error"

// eventStreamType is the media type of the answer as server-sent events, the default.
const eventStreamType = "text/event-stream"

// ChatControllerOptions configure a ChatController. ChatService is required; StreamTimeout
// defaults to two hours and a minute.
type ChatControllerOptions struct {
	ChatService   *service.ChatService
	StreamTimeout time.Duration
}

// ChatController serves the conversations' messages.
type ChatController struct {
	options ChatControllerOptions
}

// NewChatController fails without the ChatService or with a negative timeout.
func NewChatController(options *ChatControllerOptions) (*ChatController, error) {
	if options == nil || options.ChatService == nil {
		return nil, errors.New("chat controller: ChatService is required")
	}
	resolved := *options
	if resolved.StreamTimeout == 0 {
		resolved.StreamTimeout = defaultStreamTimeout
	}
	if resolved.StreamTimeout < 0 {
		return nil, fmt.Errorf("chat controller: StreamTimeout %s is negative", resolved.StreamTimeout)
	}
	return &ChatController{options: resolved}, nil
}

// RegisterRoutes mounts the controller's routes below the API's base path.
func (c *ChatController) RegisterRoutes(router gin.IRouter) {
	router.POST("/conversations/:conversationID/messages", c.sendMessageHandler())
	router.GET("/personas", c.listPersonasHandler())
	router.GET("/directories", c.listDirectoriesHandler())
}

// directoriesResponse is a folder a user may choose for the tools: its path, its parent (empty at
// a root), the roots and its subfolders' names.
type directoriesResponse struct {
	Path        string   `json:"path"`
	Parent      string   `json:"parent"`
	Roots       []string `json:"roots"`
	Directories []string `json:"directories"`
}

// listDirectoriesHandler answers the folder ?path= names, the first root without one, for the
// frontend's folder picker: a browser does not tell a page the absolute path of a folder.
func (c *ChatController) listDirectoriesHandler() gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		listing, err := c.options.ChatService.ListDirectories(ginContext.Query("path"))
		if err != nil {
			RespondError(ginContext, err)
			return
		}
		wideeventginadapter.FromContext(ginContext).Success()
		ginContext.JSON(http.StatusOK, directoriesResponse{Path: listing.Path, Parent: listing.Parent, Roots: listing.Roots, Directories: listing.Directories})
	}
}

// SendMessagePayload is the body of a new message. Workspace lets the model use the tools of
// Directory for it, an absolute host path (the server's workspace when left out); Thinking is how
// long the model reasons first: off, low, medium or high, off when left out; Persona is the ID of
// the system prompt, the default one when left out.
type SendMessagePayload struct {
	Text      string `json:"text" binding:"required"`
	Workspace bool   `json:"workspace"`
	Directory string `json:"directory"`
	Thinking  string `json:"thinking"`
	Persona   string `json:"persona"`
}

// personaPayload is a persona as a client chooses it: never its prompt.
type personaPayload struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// personasResponse is the list of the personas, the default first. The list is fixed and short,
// so it has no pages.
type personasResponse struct {
	Items []personaPayload `json:"items"`
}

// listPersonasHandler answers the personas a message may choose.
func (c *ChatController) listPersonasHandler() gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		listed := c.options.ChatService.Personas()
		response := personasResponse{Items: make([]personaPayload, 0, len(listed))}
		for _, persona := range listed {
			response.Items = append(response.Items, personaPayload{ID: persona.ID, Label: persona.Label, Description: persona.Description})
		}
		wideeventginadapter.FromContext(ginContext).Success()
		ginContext.JSON(http.StatusOK, response)
	}
}

// chatEventPayload is one event of the answer's stream, named by the event's kind.
type chatEventPayload struct {
	Text   string        `json:"text,omitempty"`
	CallID string        `json:"callId,omitempty"`
	Tool   string        `json:"tool,omitempty"`
	Target string        `json:"target,omitempty"`
	OK     bool          `json:"ok,omitempty"`
	Error  string        `json:"error,omitempty"`
	Usage  *usagePayload `json:"usage,omitempty"`
	Reason string        `json:"reason,omitempty"`
}

// usagePayload is the usage of one model call, its times in milliseconds. The prompt and
// predicted fields and the draft counts are the model server's own measurement, 0 when it sends
// none: promptTokens is what it read, its cached tokens not counted.
type usagePayload struct {
	ContextLength      int     `json:"contextLength"`
	InputTokens        int     `json:"inputTokens"`
	CachedInputTokens  int     `json:"cachedInputTokens"`
	OutputTokens       int     `json:"outputTokens"`
	LatencyMs          float64 `json:"latencyMs"`
	DurationMs         float64 `json:"durationMs"`
	TokensPerSecond    float64 `json:"tokensPerSecond"`
	PromptMs           float64 `json:"promptMs"`
	PromptTokens       int     `json:"promptTokens"`
	PromptPerSecond    float64 `json:"promptPerSecond"`
	PredictedPerSecond float64 `json:"predictedPerSecond"`
	DraftTokens        int     `json:"draftTokens"`
	DraftAccepted      int     `json:"draftAccepted"`
}

// turnResponse is a whole turn in one JSON body, for scripts that steer the chat and check its
// answers: the answer, the reasoning, the tool calls with their results, the usage of every model
// call and why the turn ended.
type turnResponse struct {
	Text      string            `json:"text"`
	Thinking  string            `json:"thinking"`
	ToolCalls []toolCallPayload `json:"toolCalls"`
	Usage     []usagePayload    `json:"usage"`
	Reason    string            `json:"reason"`
}

// toolCallPayload is one tool call and its result, joined by their call ID.
type toolCallPayload struct {
	CallID string `json:"callId"`
	Tool   string `json:"tool"`
	Target string `json:"target,omitempty"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// add folds one event of the turn into the response.
func (r *turnResponse) add(event model.ChatEvent) {
	switch event.Kind {
	case model.ChatEventText:
		r.Text += event.Text
	case model.ChatEventThinking:
		r.Thinking += event.Text
	case model.ChatEventToolCall:
		r.ToolCalls = append(r.ToolCalls, toolCallPayload{CallID: event.CallID, Tool: event.Tool, Target: event.Target})
	case model.ChatEventToolResult:
		for index := range r.ToolCalls {
			if r.ToolCalls[index].CallID == event.CallID {
				r.ToolCalls[index].OK, r.ToolCalls[index].Error = event.OK, event.Error
			}
		}
	case model.ChatEventUsage:
		if usage := toUsagePayload(event.Usage); usage != nil {
			r.Usage = append(r.Usage, *usage)
		}
	case model.ChatEventEnd:
		r.Reason = string(event.Reason)
	}
}

// sendMessageHandler answers a message with the agent's turn as a stream of server-sent events
// (text, thinking, tool_call, tool_result, usage, end; error for a turn that failed): 200 instead of
// API-009's 201, because the answer is the stream, not the stored message. A request whose Accept
// header prefers application/json gets the whole turn in one body once it ended instead.
func (c *ChatController) sendMessageHandler() gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		input, err := sendMessageInput(ginContext)
		if err != nil {
			RespondError(ginContext, err)
			return
		}
		we := wideeventginadapter.FromContext(ginContext)
		if ginContext.NegotiateFormat(eventStreamType, gin.MIMEJSON) == gin.MIMEJSON {
			c.answerWhole(ginContext, input, we)
			return
		}
		stream := newEventStream(ginContext, c.options.StreamTimeout, we)
		err = c.options.ChatService.SendMessage(ginContext.Request.Context(), input, stream.deliver)
		switch {
		case err == nil:
			we.Success()
		case !stream.started:
			RespondError(ginContext, err)
		default:
			we.Failure(err)
			_, response := errorResponse(err)
			stream.send(streamErrorEvent, response)
		}
	}
}

// answerWhole runs the turn to its end and answers it as a turnResponse; a turn that failed is
// an error response, also after it began, since nothing was sent yet.
func (c *ChatController) answerWhole(ginContext *gin.Context, input service.SendMessageInput, we *wideevent.Event) {
	extendWriteDeadline(ginContext, c.options.StreamTimeout, we)
	response := turnResponse{ToolCalls: []toolCallPayload{}, Usage: []usagePayload{}}
	if err := c.options.ChatService.SendMessage(ginContext.Request.Context(), input, response.add); err != nil {
		RespondError(ginContext, err)
		return
	}
	we.Success()
	ginContext.JSON(http.StatusOK, response)
}

func sendMessageInput(ginContext *gin.Context) (service.SendMessageInput, error) {
	conversationID, err := uuid.Parse(ginContext.Param("conversationID"))
	if err != nil {
		return service.SendMessageInput{}, fmt.Errorf("%w: conversation id: %w", model.ErrInvalid, err)
	}
	var payload SendMessagePayload
	if err := ginContext.ShouldBindJSON(&payload); err != nil {
		if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
			return service.SendMessageInput{}, fmt.Errorf("%w: message body: %w", model.ErrTooLarge, err)
		}
		return service.SendMessageInput{}, fmt.Errorf("%w: message body: %w", model.ErrInvalid, err)
	}
	return service.SendMessageInput{
		ConversationID: conversationID, Text: payload.Text, Workspace: payload.Workspace, Directory: payload.Directory, Thinking: model.Thinking(payload.Thinking), Persona: payload.Persona,
	}, nil
}

// eventStream writes a turn's events as server-sent events. It sends the headers with the first
// event, so a turn refused before it began can still answer with a status code.
type eventStream struct {
	ginContext *gin.Context
	timeout    time.Duration
	we         *wideevent.Event
	started    bool
	failed     bool
}

func newEventStream(ginContext *gin.Context, timeout time.Duration, we *wideevent.Event) *eventStream {
	return &eventStream{ginContext: ginContext, timeout: timeout, we: we}
}

func (s *eventStream) deliver(event model.ChatEvent) {
	s.send(string(event.Kind), toChatEventPayload(event))
}

// send writes one event and flushes it. After a failed write the reader is gone: the rest of
// the turn is read to its end without writing, and the failure is recorded once (LOG-002).
func (s *eventStream) send(name string, data any) {
	if s.failed {
		return
	}
	if !s.started {
		s.begin()
	}
	encoded, err := json.Marshal(data)
	if err == nil {
		_, err = fmt.Fprintf(s.ginContext.Writer, "event: %s\ndata: %s\n\n", name, encoded)
	}
	if err != nil {
		s.failed = true
		s.we.Err("stream.error", err).Bool("degraded", true)
		return
	}
	s.ginContext.Writer.Flush()
}

// begin sends the stream's headers and moves the connection's write deadline, which the
// server's write timeout would otherwise end a long answer with (LAY-006).
func (s *eventStream) begin() {
	s.started = true
	extendWriteDeadline(s.ginContext, s.timeout, s.we)
	header := s.ginContext.Writer.Header()
	header.Set("Content-Type", eventStreamType)
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	s.ginContext.Writer.WriteHeader(http.StatusOK)
}

// extendWriteDeadline moves the connection's write deadline timeout ahead, past the server's write
// timeout, for an answer that takes as long as a turn.
func extendWriteDeadline(ginContext *gin.Context, timeout time.Duration, we *wideevent.Event) {
	if err := http.NewResponseController(ginContext.Writer).SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		we.Err("stream.deadline_error", err).Bool("degraded", true) // the server's write timeout stays
	}
}

func toChatEventPayload(event model.ChatEvent) chatEventPayload {
	return chatEventPayload{
		Text: event.Text, CallID: event.CallID, Tool: event.Tool, Target: event.Target, OK: event.OK, Error: event.Error, Usage: toUsagePayload(event.Usage), Reason: string(event.Reason),
	}
}

func toUsagePayload(usage *model.ChatUsage) *usagePayload {
	if usage == nil {
		return nil
	}
	payload := &usagePayload{
		ContextLength: usage.ContextLength, InputTokens: usage.InputTokens, CachedInputTokens: usage.CachedInputTokens, OutputTokens: usage.OutputTokens,
		LatencyMs: milliseconds(usage.Latency), DurationMs: milliseconds(usage.Duration), TokensPerSecond: usage.TokensPerSecond,
	}
	if server := usage.Server; server != nil {
		payload.PromptMs, payload.PromptTokens, payload.PromptPerSecond = milliseconds(server.PromptProcessing), server.PromptTokens, server.PromptTokensPerSecond
		payload.PredictedPerSecond, payload.DraftTokens, payload.DraftAccepted = server.GenerationTokensPerSecond, server.DraftTokens, server.DraftAccepted
	}
	return payload
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}
