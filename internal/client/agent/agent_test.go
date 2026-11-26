package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	aikidoagent "github.com/govengers/aikido/agent"
	historymemory "github.com/govengers/aikido/agent/history/memory"
	"github.com/govengers/aikido/llm"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/mxcd/inkling-chat/internal/model"
	"github.com/mxcd/inkling-chat/internal/modeltest"
)

// servedModel is the one model the scripted server lists.
const servedModel = "test-model"

// newRuntime is a Runtime on the scripted model server with the host commands and a directory
// of its own, its symbolic links resolved as the service resolves them.
func newRuntime(t *testing.T, server *modeltest.Server, commands ...HostCommand) (*Runtime, string) {
	t.Helper()
	client, err := NewClient(&ClientOptions{
		BaseURL: server.BaseURL(), APIKey: "local", Namespace: "test", Registerer: prometheus.NewRegistry(), Traces: noop.NewTracerProvider(), MaximumAttempts: 1,
	})
	require.NoError(t, err)
	runtime, err := New(&Options{Client: client, History: historymemory.New(), Locker: aikidoagent.NewLocalLocker(), Commands: commands})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close()) })
	return runtime, temporaryDirectory(t)
}

// temporaryDirectory is a new directory with its symbolic links resolved (macOS's /var is one).
func temporaryDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return directory
}

// converse runs one turn to its end and returns its events.
func converse(t *testing.T, runtime *Runtime, turn model.ChatTurn) []model.ChatEvent {
	t.Helper()
	stream, err := runtime.Converse(t.Context(), turn)
	require.NoError(t, err)
	var events []model.ChatEvent
	for event := range stream {
		events = append(events, event)
	}
	return events
}

func kinds(events []model.ChatEvent) []model.ChatEventKind {
	found := make([]model.ChatEventKind, 0, len(events))
	for _, event := range events {
		found = append(found, event.Kind)
	}
	return found
}

func TestATurnWithTheWorkspaceWritesTheFileTheModelAsksFor(t *testing.T) {
	t.Parallel()
	server := modeltest.NewServer(t, []string{servedModel},
		modeltest.ToolCall("call-1", "write_file", `{"path":"notes.md","content":"hello"}`),
		modeltest.Text("Done", "."),
	)
	runtime, directory := newRuntime(t, server)

	events := converse(t, runtime, model.ChatTurn{ConversationID: uuid.New(), Text: "Write the notes.", Model: servedModel, System: "You are terse.", Workspace: true, Directory: directory})

	require.Equal(t, []model.ChatEventKind{
		model.ChatEventUsage, model.ChatEventToolCall, model.ChatEventToolResult, model.ChatEventText, model.ChatEventText, model.ChatEventUsage, model.ChatEventEnd,
	}, kinds(events))
	require.Equal(t, model.ChatEvent{Kind: model.ChatEventToolCall, CallID: "call-1", Tool: "write_file", Target: "notes.md"}, events[1])
	require.Equal(t, model.ChatEvent{Kind: model.ChatEventToolResult, CallID: "call-1", Tool: "write_file", OK: true}, events[2])
	require.Equal(t, model.ChatEvent{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonStop}, events[6])
	content, err := fs.ReadFile(os.DirFS(directory), "notes.md")
	require.NoError(t, err)
	require.Equal(t, "hello", string(content))
	require.Contains(t, server.Asked()[0], `"name":"write_file"`, "the workspace's tools are offered")
}

func TestATurnWithoutTheWorkspaceOffersNoTool(t *testing.T) {
	t.Parallel()
	server := modeltest.NewServer(t, []string{servedModel}, modeltest.Text("Hello"))
	runtime, _ := newRuntime(t, server)

	events := converse(t, runtime, model.ChatTurn{ConversationID: uuid.New(), Text: "Say hello.", Model: servedModel, System: "You are terse."})

	require.Equal(t, []model.ChatEventKind{model.ChatEventText, model.ChatEventUsage, model.ChatEventEnd}, kinds(events))
	require.Equal(t, "Hello", events[0].Text)
	asked := server.Asked()
	require.Len(t, asked, 1)
	require.NotContains(t, asked[0], `"tools"`)
	require.NotContains(t, asked[0], "cache_control", "an OpenAI-compatible server gets no cache breakpoints")
	require.Contains(t, asked[0], `"model":"test-model"`)
}

func TestATurnStreamsTheReasoningAndTheUsageOfEveryModelCall(t *testing.T) {
	t.Parallel()
	server := modeltest.NewServer(t, []string{servedModel}, modeltest.Reasoning("Hi.", "The user ", "greets."))
	runtime, _ := newRuntime(t, server)

	events := converse(t, runtime, model.ChatTurn{ConversationID: uuid.New(), Text: "Hello.", Model: servedModel})

	require.Equal(t, []model.ChatEventKind{model.ChatEventThinking, model.ChatEventThinking, model.ChatEventText, model.ChatEventUsage, model.ChatEventEnd}, kinds(events))
	require.Equal(t, "The user ", events[0].Text)
	used := events[3].Usage
	require.NotNil(t, used)
	require.Equal(t, 30, used.InputTokens)
	require.Equal(t, 8, used.OutputTokens)
	require.Positive(t, used.Duration)
	require.LessOrEqual(t, used.Latency, used.Duration)
}

func TestTheUsageNamesTheCachedInputTokens(t *testing.T) {
	t.Parallel()
	server := modeltest.NewServer(t, []string{servedModel}, modeltest.Text("Hello"))
	runtime, _ := newRuntime(t, server)

	events := converse(t, runtime, model.ChatTurn{ConversationID: uuid.New(), Text: "Say hello.", Model: servedModel})

	require.Equal(t, &model.ChatUsage{InputTokens: 20, CachedInputTokens: 12, OutputTokens: 5, Server: &model.ServerTimings{
		PromptProcessing: 4500 * time.Microsecond, PromptTokens: 8, PromptTokensPerSecond: 1777.8, GenerationTokensPerSecond: 100, DraftTokens: 4, DraftAccepted: 3,
	}}, withoutTimes(events[1].Usage))
}

func TestEveryCallsUsageCarriesWhatTheServerMeasuredOfThatCall(t *testing.T) {
	t.Parallel()
	server := modeltest.NewServer(t, []string{servedModel},
		modeltest.ToolCall("call-1", "list_files", `{}`),
		modeltest.Reasoning("Nothing there.", "Empty."),
		modeltest.Text("Done."),
	)
	runtime, directory := newRuntime(t, server)
	conversation := uuid.New()

	first := converse(t, runtime, model.ChatTurn{ConversationID: conversation, Text: "What is there?", Model: servedModel, Workspace: true, Directory: directory})
	second := converse(t, runtime, model.ChatTurn{ConversationID: conversation, Text: "Thanks.", Model: servedModel})

	var measured []*model.ServerTimings
	for _, event := range append(first, second...) {
		if event.Kind == model.ChatEventUsage {
			measured = append(measured, event.Usage.Server)
		}
	}
	require.Len(t, measured, 3)
	require.Equal(t, 20, measured[0].PromptTokens, "the tool call's timings")
	require.Equal(t, 10*time.Millisecond, measured[0].PromptProcessing)
	require.Nil(t, measured[1], "a call whose server sent no timings has none, and takes no other call's")
	require.Equal(t, 8, measured[2].PromptTokens, "a new turn counts its own calls")
}

func TestATurnAsksTheModelServerForItsThinking(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		thinking model.Thinking
		want     string
	}{
		{model.ThinkingOff, `"reasoning":{"effort":"none"}`},
		{model.ThinkingLow, `"reasoning":{"effort":"low"}`},
		{model.ThinkingHigh, `"reasoning":{"effort":"high"}`},
	} {
		t.Run(string(testCase.thinking), func(t *testing.T) {
			t.Parallel()
			server := modeltest.NewServer(t, []string{servedModel}, modeltest.Text("Hi"), modeltest.Text("Hi"))
			runtime, _ := newRuntime(t, server)
			turn := model.ChatTurn{ConversationID: uuid.New(), Text: "Hello.", Model: servedModel, Thinking: testCase.thinking}

			require.NoError(t, runtime.Warm(t.Context(), turn))
			converse(t, runtime, turn)

			for _, asked := range server.Asked() {
				require.Contains(t, asked, testCase.want, "the warm-up and the turn ask alike")
			}
		})
	}
	server := modeltest.NewServer(t, []string{servedModel}, modeltest.Text("Hi"))
	runtime, _ := newRuntime(t, server)
	converse(t, runtime, model.ChatTurn{ConversationID: uuid.New(), Text: "Hello.", Model: servedModel})
	require.NotContains(t, server.Asked()[0], `"reasoning"`, "a turn without a level leaves the server's default")
}

// withoutTimes is a usage without its measured times, which differ from run to run.
func withoutTimes(usage *model.ChatUsage) *model.ChatUsage {
	if usage == nil {
		return nil
	}
	copied := *usage
	copied.Latency, copied.Duration, copied.TokensPerSecond = 0, 0, 0
	return &copied
}

func TestAConversationRemembersItsEarlierTurns(t *testing.T) {
	t.Parallel()
	server := modeltest.NewServer(t, []string{servedModel}, modeltest.Text("Blue."), modeltest.Text("You said blue."))
	runtime, _ := newRuntime(t, server)
	conversation := uuid.New()

	converse(t, runtime, model.ChatTurn{ConversationID: conversation, Text: "My colour is blue.", Model: servedModel})
	converse(t, runtime, model.ChatTurn{ConversationID: conversation, Text: "Which colour?", Model: servedModel})

	second := server.Asked()[1]
	require.Contains(t, second, "My colour is blue.")
	require.Contains(t, second, "Blue.")
	require.Contains(t, second, "Which colour?")
}

func TestAFailedTurnEndsOnceWithTheModelsError(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name   string
		status int
		want   error
	}{
		{"a request the server refuses is rejected", http.StatusBadRequest, model.ErrRejected},
		{"a server error is unavailable", http.StatusInternalServerError, model.ErrUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			server := modeltest.NewServer(t, []string{servedModel}, modeltest.Status(testCase.status, `{"error":{"message":"no"}}`))
			runtime, _ := newRuntime(t, server)

			events := converse(t, runtime, model.ChatTurn{ConversationID: uuid.New(), Text: "Hello?", Model: servedModel})

			require.Equal(t, []model.ChatEventKind{model.ChatEventEnd}, kinds(events))
			require.Equal(t, model.ChatEndReasonError, events[0].Reason)
			require.ErrorIs(t, events[0].Err, testCase.want)
		})
	}
}

func TestACancelledTurnEndsAsCancelledWithoutAnError(t *testing.T) {
	t.Parallel()
	server := modeltest.NewServer(t, []string{servedModel}, modeltest.Text("never read"))
	runtime, _ := newRuntime(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	stream, err := runtime.Converse(ctx, model.ChatTurn{ConversationID: uuid.New(), Text: "Hello?", Model: servedModel})
	require.NoError(t, err)
	var last model.ChatEvent
	for event := range stream {
		last = event
	}

	// the reader's context ended, so the end may not have been sent: a sent one names the cancel
	if last.Kind == model.ChatEventEnd {
		require.Equal(t, model.ChatEndReasonCancelled, last.Reason)
		require.NoError(t, last.Err)
	}
}

// sentRequest is the part of a chat completion request a prompt cache keys on, and its token limit.
type sentRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools     json.RawMessage `json:"tools"`
	MaxTokens int             `json:"max_tokens"`
}

func decodeRequest(t *testing.T, body string) sentRequest {
	t.Helper()
	var sent sentRequest
	require.NoError(t, json.Unmarshal([]byte(body), &sent))
	return sent
}

func TestTheWarmUpSendsTheStartOfATurnWithTheWorkspaceForOneToken(t *testing.T) {
	t.Parallel()
	server := modeltest.NewServer(t, []string{servedModel}, modeltest.Text("Hi"), modeltest.Text("Done."))
	runtime, directory := newRuntime(t, server)
	turn := model.ChatTurn{ConversationID: uuid.New(), Text: "Write the notes.", Model: servedModel, System: "You are terse.", Workspace: true, Directory: directory}

	require.NoError(t, runtime.Warm(t.Context(), turn))
	converse(t, runtime, turn)

	asked := server.Asked()
	require.Len(t, asked, 2)
	warm, turnRequest := decodeRequest(t, asked[0]), decodeRequest(t, asked[1])
	require.Equal(t, 1, warm.MaxTokens)
	require.Equal(t, servedModel, warm.Model)
	require.Equal(t, "system", warm.Messages[0].Role)
	require.Equal(t, "You are terse.", warm.Messages[0].Content)
	require.Contains(t, string(warm.Tools), `"name":"write_file"`)
	require.Contains(t, string(warm.Tools), `"name":"bash"`)
	// what a prompt cache keeps (model, system prompt, tools) is the same as in a turn's request
	require.Equal(t, turnRequest.Model, warm.Model)
	require.Equal(t, turnRequest.Messages[0], warm.Messages[0])
	require.JSONEq(t, string(turnRequest.Tools), string(warm.Tools))
}

func TestAFailedWarmUpNamesTheModelsError(t *testing.T) {
	t.Parallel()
	server := modeltest.NewServer(t, []string{servedModel}, modeltest.Status(http.StatusBadRequest, `{"error":{"message":"no"}}`))
	runtime, directory := newRuntime(t, server)

	err := runtime.Warm(t.Context(), model.ChatTurn{Model: servedModel, System: "You are terse.", Workspace: true, Directory: directory})

	require.ErrorIs(t, err, model.ErrRejected)
}

func TestNewFailsWithoutItsDependencies(t *testing.T) {
	t.Parallel()
	_, err := New(nil)
	require.Error(t, err)
	_, err = New(&Options{History: historymemory.New(), Locker: aikidoagent.NewLocalLocker()})
	require.Error(t, err)
}

func TestTheTargetOfACallIsTheFileOrPatternItNames(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name, arguments, want string
	}{
		{"a path", `{"path":"notes/today.md","content":"x"}`, "notes/today.md"},
		{"a move's source", `{"source":"old.md","destination":"new.md"}`, "old.md"},
		{"a pattern", `{"pattern":"TODO"}`, "TODO"},
		{"a command", `{"command":"go test ./...","description":"run the tests"}`, "go test ./..."},
		{"a script of several lines", `{"command":"cd app\ngo build ./..."}`, "cd app …"},
		{"a long command", `{"command":"` + strings.Repeat("a", 100) + `"}`, strings.Repeat("a", 79) + "…"},
		{"a background process", `{"program":"go","args":["run","./cmd/server"]}`, "go run ./cmd/server"},
		{"a background process's id", `{"id":3}`, "#3"},
		{"a request's URL", `{"url":"http://127.0.0.1:20080/health"}`, "http://127.0.0.1:20080/health"},
		{"nothing named", `{}`, ""},
		{"invalid JSON", `{"path":`, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, testCase.want, target(testCase.arguments))
		})
	}
}

func TestAnEventWithoutItsBodyFailsTheTurnInsteadOfPanicking(t *testing.T) {
	t.Parallel()
	turn := translator{}

	_, relevant := turn.translate(aikidoagent.Event{Kind: aikidoagent.EventToolCall})
	require.False(t, relevant)
	ended, relevant := turn.translate(aikidoagent.Event{Kind: aikidoagent.EventEnd, EndReason: aikidoagent.EndReasonStop})

	require.True(t, relevant)
	require.Equal(t, model.ChatEndReasonError, ended.Reason)
	require.ErrorContains(t, ended.Err, "tool_call")
}

func TestTheEndOfARunCarriesItsReasonAndItsError(t *testing.T) {
	t.Parallel()
	failure := errors.New("connection refused")
	for _, testCase := range []struct {
		name   string
		events []aikidoagent.Event
		reason model.ChatEndReason
		want   error
	}{
		{"a stop", []aikidoagent.Event{{Kind: aikidoagent.EventEnd, EndReason: aikidoagent.EndReasonStop}}, model.ChatEndReasonStop, nil},
		{"the turn cap", []aikidoagent.Event{{Kind: aikidoagent.EventEnd, EndReason: aikidoagent.EndReasonMaximumTurns}}, model.ChatEndReasonMaximumTurns, nil},
		{"a timeout", []aikidoagent.Event{{Kind: aikidoagent.EventEnd, EndReason: aikidoagent.EndReasonTimeout}}, model.ChatEndReasonTimeout, model.ErrUnavailable},
		{"a failed run", []aikidoagent.Event{{Kind: aikidoagent.EventError, Err: failure}, {Kind: aikidoagent.EventEnd, EndReason: aikidoagent.EndReasonError}}, model.ChatEndReasonError, model.ErrUnavailable},
		{"a refused request", []aikidoagent.Event{{Kind: aikidoagent.EventError, Err: llm.ErrInvalidRequest}, {Kind: aikidoagent.EventEnd, EndReason: aikidoagent.EndReasonError}}, model.ChatEndReasonError, model.ErrRejected},
		{"an unknown reason", []aikidoagent.Event{{Kind: aikidoagent.EventEnd, EndReason: "later"}}, model.ChatEndReasonError, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			turn := translator{}
			var ended model.ChatEvent
			for _, event := range testCase.events {
				if converted, relevant := turn.translate(event); relevant {
					ended = converted
				}
			}
			require.Equal(t, model.ChatEventEnd, ended.Kind)
			require.Equal(t, testCase.reason, ended.Reason)
			if testCase.want == nil {
				require.NoError(t, ended.Err)
				return
			}
			require.ErrorIs(t, ended.Err, testCase.want)
		})
	}
}
