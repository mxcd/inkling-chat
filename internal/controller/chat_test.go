package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	wideevent "github.com/govengers/wide-event-logger"
	wideeventginadapter "github.com/govengers/wide-event-logger/ginadapter"
	"github.com/stretchr/testify/require"

	"github.com/mxcd/inkling-chat/internal/model"
	"github.com/mxcd/inkling-chat/internal/service"
)

// scriptedRunner answers every turn with its events, or fails to start one; asked keeps the
// turns when set.
type scriptedRunner struct {
	events []model.ChatEvent
	fails  error
	asked  *askedTurns
}

// askedTurns are the turns a runner was asked for, written by the server's goroutine.
type askedTurns struct {
	mutex sync.Mutex
	turns []model.ChatTurn
}

func (a *askedTurns) all() []model.ChatTurn {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return append([]model.ChatTurn(nil), a.turns...)
}

func (r scriptedRunner) Warm(context.Context, model.ChatTurn) error { return r.fails }

func (r scriptedRunner) Converse(_ context.Context, turn model.ChatTurn) (<-chan model.ChatEvent, error) {
	if r.asked != nil {
		r.asked.mutex.Lock()
		r.asked.turns = append(r.asked.turns, turn)
		r.asked.mutex.Unlock()
	}
	if r.fails != nil {
		return nil, r.fails
	}
	stream := make(chan model.ChatEvent, len(r.events))
	for _, event := range r.events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

// recorder keeps the wide events of the requests.
type recorder struct {
	mutex  sync.Mutex
	events []*wideevent.Event
}

func (r *recorder) Emit(we *wideevent.Event) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.events = append(r.events, we)
}

// last is the newest event's value under key.
func (r *recorder) last(key string) any {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if len(r.events) == 0 {
		return nil
	}
	for _, recorded := range r.events[len(r.events)-1].Fields() {
		if recorded.Key == key {
			return recorded.Value
		}
	}
	return nil
}

// newChatServer serves a ChatController on runner behind the wide-event middleware.
func newChatServer(t *testing.T, runner service.TurnRunner) (*httptest.Server, *recorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	chatService, err := service.NewChatService(&service.ChatServiceOptions{TurnRunner: runner, Model: "test-model"})
	require.NoError(t, err)
	chatController, err := NewChatController(&ChatControllerOptions{ChatService: chatService})
	require.NoError(t, err)
	emitted := &recorder{}
	wideEvents, err := wideeventginadapter.NewMiddleware(&wideeventginadapter.Options{Emitter: emitted})
	require.NoError(t, err)
	router := gin.New()
	router.Use(wideEvents)
	chatController.RegisterRoutes(router.Group("/api/v1"))
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, emitted
}

// postAcceptingJSON sends body as a message of conversation asking for the whole turn as JSON
// and returns the status and the answer.
func postAcceptingJSON(t *testing.T, server *httptest.Server, conversation, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/conversations/"+conversation+"/messages", strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(answer)
}

// post sends body as a message of conversation and returns the status, the content type and the
// whole answer.
func post(t *testing.T, server *httptest.Server, conversation, body string) (int, string, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/conversations/"+conversation+"/messages", strings.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, response.Header.Get("Content-Type"), string(answer)
}

func TestAMessageIsAnsweredWithTheTurnAsAnEventStream(t *testing.T) {
	t.Parallel()
	server, emitted := newChatServer(t, scriptedRunner{events: []model.ChatEvent{
		{Kind: model.ChatEventToolCall, CallID: "call-1", Tool: "write_file", Target: "notes.md"},
		{Kind: model.ChatEventToolResult, CallID: "call-1", Tool: "write_file", OK: true},
		{Kind: model.ChatEventThinking, Text: "Short."},
		{Kind: model.ChatEventText, Text: "Done."},
		{Kind: model.ChatEventUsage, Usage: &model.ChatUsage{
			InputTokens: 40, CachedInputTokens: 32, OutputTokens: 6, Latency: 1500 * time.Microsecond, Duration: 2 * time.Second, TokensPerSecond: 3,
		}},
		{Kind: model.ChatEventUsage, Usage: &model.ChatUsage{
			InputTokens: 40, OutputTokens: 6, Server: &model.ServerTimings{
				PromptProcessing: 1040 * time.Millisecond, PromptTokens: 2108, PromptTokensPerSecond: 2030, GenerationTokensPerSecond: 95.5, DraftTokens: 10, DraftAccepted: 4,
			},
		}},
		{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonStop},
	}})

	status, contentType, answer := post(t, server, uuid.NewString(), `{"text":"Write the notes.","workspace":true}`)

	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "text/event-stream", contentType)
	require.Equal(t, strings.Join([]string{
		`event: tool_call`, `data: {"callId":"call-1","tool":"write_file","target":"notes.md"}`, ``,
		`event: tool_result`, `data: {"callId":"call-1","tool":"write_file","ok":true}`, ``,
		`event: thinking`, `data: {"text":"Short."}`, ``,
		`event: text`, `data: {"text":"Done."}`, ``,
		`event: usage`, `data: {"usage":{"contextLength":0,"inputTokens":40,"cachedInputTokens":32,"outputTokens":6,"latencyMs":1.5,"durationMs":2000,"tokensPerSecond":3,` +
			`"promptMs":0,"promptTokens":0,"promptPerSecond":0,"predictedPerSecond":0,"draftTokens":0,"draftAccepted":0}}`, ``,
		`event: usage`, `data: {"usage":{"contextLength":0,"inputTokens":40,"cachedInputTokens":0,"outputTokens":6,"latencyMs":0,"durationMs":0,"tokensPerSecond":0,` +
			`"promptMs":1040,"promptTokens":2108,"promptPerSecond":2030,"predictedPerSecond":95.5,"draftTokens":10,"draftAccepted":4}}`, ``,
		`event: end`, `data: {"reason":"stop"}`, ``, ``,
	}, "\n"), answer)
	require.Equal(t, "success", emitted.last("outcome"))
	require.Equal(t, true, emitted.last("chat.workspace"))
}

func TestAMessageAcceptingJSONIsAnsweredWithTheWholeTurn(t *testing.T) {
	t.Parallel()
	server, emitted := newChatServer(t, scriptedRunner{events: []model.ChatEvent{
		{Kind: model.ChatEventThinking, Text: "Read "},
		{Kind: model.ChatEventThinking, Text: "it."},
		{Kind: model.ChatEventToolCall, CallID: "call-1", Tool: "read_file", Target: "go.mod"},
		{Kind: model.ChatEventToolCall, CallID: "call-2", Tool: "grep", Target: "TODO"},
		{Kind: model.ChatEventToolResult, CallID: "call-2", Tool: "grep", Error: "no match"},
		{Kind: model.ChatEventToolResult, CallID: "call-1", Tool: "read_file", OK: true},
		{Kind: model.ChatEventText, Text: "Go "},
		{Kind: model.ChatEventText, Text: "1.26."},
		{Kind: model.ChatEventUsage, Usage: &model.ChatUsage{InputTokens: 40, CachedInputTokens: 32, OutputTokens: 6}},
		{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonStop},
	}})

	status, answer := postAcceptingJSON(t, server, uuid.NewString(), `{"text":"Which Go?","workspace":true}`)

	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{
		"text": "Go 1.26.", "thinking": "Read it.", "reason": "stop",
		"toolCalls": [
			{"callId": "call-1", "tool": "read_file", "target": "go.mod", "ok": true},
			{"callId": "call-2", "tool": "grep", "target": "TODO", "ok": false, "error": "no match"}
		],
		"usage": [{"contextLength": 0, "inputTokens": 40, "cachedInputTokens": 32, "outputTokens": 6, "latencyMs": 0, "durationMs": 0, "tokensPerSecond": 0,
			"promptMs": 0, "promptTokens": 0, "promptPerSecond": 0, "predictedPerSecond": 0, "draftTokens": 0, "draftAccepted": 0}]
	}`, answer)
	require.Equal(t, "success", emitted.last("outcome"))
}

func TestATurnAcceptingJSONThatFailsIsAnErrorResponse(t *testing.T) {
	t.Parallel()
	server, emitted := newChatServer(t, scriptedRunner{events: []model.ChatEvent{
		{Kind: model.ChatEventText, Text: "Half an"},
		{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonError, Err: fmt.Errorf("%w: status 400: context length", model.ErrRejected)},
	}})

	status, answer := postAcceptingJSON(t, server, uuid.NewString(), `{"text":"Hello"}`)

	require.Equal(t, http.StatusUnprocessableEntity, status)
	var response ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(answer), &response))
	require.Equal(t, "rejected", response.Code)
	require.NotContains(t, answer, "context length")
	require.Equal(t, "failure", emitted.last("outcome"))
}

func TestAMessageNamesTheThinkingOfItsTurn(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, body string
		want       model.Thinking
	}{
		{"a level", `{"text":"Hello","thinking":"high"}`, model.ThinkingHigh},
		{"off", `{"text":"Hello","thinking":"off"}`, model.ThinkingOff},
		{"none named", `{"text":"Hello"}`, model.ThinkingOff},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			asked := &askedTurns{}
			server, _ := newChatServer(t, scriptedRunner{events: []model.ChatEvent{{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonStop}}, asked: asked})

			status, _, _ := post(t, server, uuid.NewString(), testCase.body)

			require.Equal(t, http.StatusOK, status)
			require.Len(t, asked.all(), 1)
			require.Equal(t, testCase.want, asked.all()[0].Thinking)
		})
	}
}

func TestARequestTheControllerCannotReadIsA400InTheErrorShape(t *testing.T) {
	t.Parallel()
	server, emitted := newChatServer(t, scriptedRunner{})
	for _, testCase := range []struct {
		name, conversation, body string
	}{
		{"a conversation id that is no UUID", "not-a-uuid", `{"text":"Hello"}`},
		{"a body that is no JSON", uuid.NewString(), `{"text":`},
		{"a body without the text", uuid.NewString(), `{"workspace":true}`},
		{"a message of spaces", uuid.NewString(), `{"text":"   "}`},
		{"a thinking level the model does not know", uuid.NewString(), `{"text":"Hello","thinking":"lots"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, contentType, answer := post(t, server, testCase.conversation, testCase.body)

			require.Equal(t, http.StatusBadRequest, status)
			require.Contains(t, contentType, "application/json")
			require.JSONEq(t, `{"code":"invalid_argument","message":"The request is not valid."}`, answer)
			require.Equal(t, "failure", emitted.last("outcome"))
		})
	}
}

func TestATurnThatCannotStartIsAnErrorResponseNotAStream(t *testing.T) {
	t.Parallel()
	server, _ := newChatServer(t, scriptedRunner{fails: fmt.Errorf("%w: connection refused", model.ErrUnavailable)})

	status, contentType, answer := post(t, server, uuid.NewString(), `{"text":"Hello"}`)

	require.Equal(t, http.StatusBadGateway, status)
	require.Contains(t, contentType, "application/json")
	var response ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(answer), &response))
	require.Equal(t, "unavailable", response.Code)
	require.NotContains(t, answer, "connection refused", "no internal text reaches the client (API-002)")
}

func TestATurnThatFailsAfterItBeganEndsTheStreamWithAnErrorEvent(t *testing.T) {
	t.Parallel()
	server, emitted := newChatServer(t, scriptedRunner{events: []model.ChatEvent{
		{Kind: model.ChatEventText, Text: "Half an"},
		{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonError, Err: fmt.Errorf("%w: status 400: context length", model.ErrRejected)},
	}})

	status, _, answer := post(t, server, uuid.NewString(), `{"text":"Hello"}`)

	require.Equal(t, http.StatusOK, status, "the stream had begun")
	require.Contains(t, answer, "event: end\ndata: {\"reason\":\"error\"}\n\n")
	require.Contains(t, answer, "event: error\ndata: {\"code\":\"rejected\",")
	require.NotContains(t, answer, "context length")
	require.Equal(t, "failure", emitted.last("outcome"))
}

func TestErrorsMapToOneStatusAndCodeEach(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: the message is empty", model.ErrInvalid), http.StatusBadRequest, "invalid_argument"},
		{fmt.Errorf("guard: %w", model.ErrForbidden), http.StatusForbidden, "forbidden"},
		{fmt.Errorf("guard: %w", model.ErrUnsupportedMediaType), http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{fmt.Errorf("%w: the message has 9 bytes", model.ErrTooLarge), http.StatusRequestEntityTooLarge, "too_large"},
		{model.ErrNotFound, http.StatusNotFound, "not_found"},
		{fmt.Errorf("turn: %w", model.ErrRejected), http.StatusUnprocessableEntity, "rejected"},
		{fmt.Errorf("turn: %w", model.ErrUnavailable), http.StatusBadGateway, "unavailable"},
		{errors.New("pool exhausted at 10.0.0.7"), http.StatusInternalServerError, "internal"},
	} {
		t.Run(testCase.code, func(t *testing.T) {
			t.Parallel()
			status, response := errorResponse(testCase.err)
			require.Equal(t, testCase.status, status)
			require.Equal(t, testCase.code, response.Code)
			require.NotContains(t, response.Message, "10.0.0.7")
		})
	}
}

func TestNewChatControllerValidatesItsOptions(t *testing.T) {
	t.Parallel()
	chatService, err := service.NewChatService(&service.ChatServiceOptions{TurnRunner: scriptedRunner{}, Model: "test-model"})
	require.NoError(t, err)
	_, err = NewChatController(nil)
	require.Error(t, err)
	_, err = NewChatController(&ChatControllerOptions{})
	require.Error(t, err)
	_, err = NewChatController(&ChatControllerOptions{ChatService: chatService, StreamTimeout: -time.Second})
	require.Error(t, err)
}

func TestARefusalForTheUserShowsItsOwnMessage(t *testing.T) {
	t.Parallel()
	status, response := errorResponse(fmt.Errorf("chat service: %w", &model.InvalidInputError{Message: "There is no persona \"x\"."}))

	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, ErrorResponse{Code: "invalid_argument", Message: "There is no persona \"x\"."}, response)
}

func TestAMessageNamesItsPersonaAndAnUnknownOneIsRefused(t *testing.T) {
	t.Parallel()
	asked := &askedTurns{}
	server, _ := newChatServer(t, scriptedRunner{events: []model.ChatEvent{{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonStop}}, asked: asked})

	status, _, _ := post(t, server, uuid.NewString(), `{"text":"Hello","persona":"golang"}`)
	refused, _, answer := post(t, server, uuid.NewString(), `{"text":"Hello","persona":"pirate"}`)

	require.Equal(t, http.StatusOK, status)
	require.Contains(t, asked.all()[0].System, "senior Go developer")
	require.Equal(t, http.StatusBadRequest, refused)
	require.JSONEq(t, `{"code":"invalid_argument","message":"There is no persona \"pirate\"."}`, answer)
	require.Len(t, asked.all(), 1)
}

func TestTheDirectoryOfAMessageReachesTheService(t *testing.T) {
	t.Parallel()
	server, _ := newChatServer(t, scriptedRunner{})

	status, _, answer := post(t, server, uuid.NewString(), `{"text":"Hello","workspace":true,"directory":"/does/not/exist"}`)

	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, answer, "Cannot use the directory: /does/not/exist does not exist.")
}

func TestThePersonasAreListedWithoutTheirPrompts(t *testing.T) {
	t.Parallel()
	server, emitted := newChatServer(t, scriptedRunner{})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/personas", nil)
	require.NoError(t, err)

	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	var listed struct {
		Items []map[string]string `json:"items"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&listed))

	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, map[string]string{"id": "assistant", "label": "Assistant", "description": "A helpful general assistant."}, listed.Items[0])
	ids := make([]string, 0, len(listed.Items))
	for _, item := range listed.Items {
		require.NotContains(t, item, "prompt")
		ids = append(ids, item["id"])
	}
	require.Contains(t, ids, "golang")
	require.Equal(t, "success", emitted.last("outcome"))
}

func TestTheFolderPickerListsAFolderOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "app"), 0o750))
	chatService, err := service.NewChatService(&service.ChatServiceOptions{TurnRunner: scriptedRunner{}, Model: "test-model", WorkspaceRoots: []string{root}})
	require.NoError(t, err)
	chatController, err := NewChatController(&ChatControllerOptions{ChatService: chatService})
	require.NoError(t, err)
	router := gin.New()
	chatController.RegisterRoutes(router.Group("/api/v1"))
	resolved, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)

	listed := httptest.NewRecorder()
	router.ServeHTTP(listed, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/directories", nil))
	refused := httptest.NewRecorder()
	router.ServeHTTP(refused, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/directories?path=relative", nil))

	require.Equal(t, http.StatusOK, listed.Code)
	require.JSONEq(t, `{"path":"`+resolved+`","parent":"","roots":["`+resolved+`"],"directories":["app"]}`, listed.Body.String())
	require.Equal(t, http.StatusBadRequest, refused.Code)
	require.Contains(t, refused.Body.String(), "is not an absolute path")
}
