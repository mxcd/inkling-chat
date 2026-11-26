// Package modeltest is a scripted model server for tests: it speaks the chat completions stream
// and the model list the chat's client reads, answers in the order of a script and keeps what
// it was asked. The unit tests of the client and the end-to-end suite share it.
package modeltest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Reply is what the server answers one chat completion with.
type Reply struct {
	status int
	body   string
}

// The timings the server's answers end with, as Strata and llama.cpp send them: TextTimings with
// Text, ToolCallTimings with ToolCall. Reasoning sends none, like a server that does not measure.
const (
	TextTimings     = `"timings":{"cache_n":12,"prompt_n":8,"prompt_ms":4.5,"prompt_per_token_ms":0.56,"prompt_per_second":1777.8,"predicted_n":5,"predicted_ms":50,"predicted_per_token_ms":10,"predicted_per_second":100,"draft_n":4,"draft_n_accepted":3}`
	ToolCallTimings = `"timings":{"cache_n":0,"prompt_n":20,"prompt_ms":10,"prompt_per_token_ms":0.5,"prompt_per_second":2000,"predicted_n":9,"predicted_ms":90,"predicted_per_token_ms":10,"predicted_per_second":100,"draft_n":0,"draft_n_accepted":0}`
)

// Text streams pieces as the answer's text.
func Text(pieces ...string) Reply {
	chunks := make([]string, 0, len(pieces)+1)
	for _, piece := range pieces {
		chunks = append(chunks, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, quoted(piece)))
	}
	return streamReply(append(chunks, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25,"prompt_tokens_details":{"cached_tokens":12}},`+TextTimings+`}`))
}

// Reasoning streams pieces as the model's reasoning, then answer as its text.
func Reasoning(answer string, pieces ...string) Reply {
	chunks := make([]string, 0, len(pieces)+2)
	for _, piece := range pieces {
		chunks = append(chunks, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"reasoning":%s},"finish_reason":null}]}`, quoted(piece)))
	}
	chunks = append(chunks, fmt.Sprintf(`{"choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, quoted(answer)),
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":8,"total_tokens":38}}`)
	return streamReply(chunks)
}

// ToolCall streams one call of a tool with its arguments as JSON text.
func ToolCall(callID, name, arguments string) Reply {
	call := fmt.Sprintf(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":%s,"type":"function","function":{"name":%s,"arguments":%s}}]},"finish_reason":null}]}`,
		quoted(callID), quoted(name), quoted(arguments))
	return streamReply([]string{call, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":9,"total_tokens":29},` + ToolCallTimings + `}`})
}

// Status answers with an HTTP status and body instead of a stream.
func Status(status int, body string) Reply {
	return Reply{status: status, body: body}
}

func streamReply(chunks []string) Reply {
	var body strings.Builder
	for _, chunk := range chunks {
		body.WriteString("data: " + chunk + "\n\n")
	}
	body.WriteString("data: [DONE]\n\n")
	return Reply{status: http.StatusOK, body: body.String()}
}

func quoted(text string) string {
	encoded, err := json.Marshal(text)
	if err != nil {
		panic(err) // a string always encodes
	}
	return string(encoded)
}

// ContextLength is the context window the server lists for every model, as vLLM names it.
const ContextLength = 32768

// Server answers the chat completions in the order of its script and keeps what it was asked;
// its model list names its models.
type Server struct {
	*httptest.Server
	mutex    sync.Mutex
	script   []Reply
	requests []string
}

// NewServer starts a server that lists models and answers with script; it closes with the test.
func NewServer(tb testing.TB, models []string, script ...Reply) *Server {
	tb.Helper()
	scripted := &Server{script: script}
	routes := http.NewServeMux()
	routes.HandleFunc("GET /v1/models", func(writer http.ResponseWriter, _ *http.Request) {
		entries := make([]map[string]any, 0, len(models))
		for _, name := range models {
			entries = append(entries, map[string]any{"id": name, "max_model_len": ContextLength})
		}
		writer.Header().Set("Connection", "close") // no idle connection outlives the test (goleak)
		require.NoError(tb, json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": entries}))
	})
	routes.HandleFunc("POST /v1/chat/completions", func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		require.NoError(tb, err)
		next := scripted.take(string(body))
		writer.Header().Set("Connection", "close")
		if next.status == http.StatusOK {
			writer.Header().Set("Content-Type", "text/event-stream")
		}
		writer.WriteHeader(next.status)
		_, err = io.WriteString(writer, next.body)
		require.NoError(tb, err)
	})
	scripted.Server = httptest.NewServer(routes)
	tb.Cleanup(scripted.Close)
	return scripted
}

// take records a request and returns the reply it gets: the script's next one, a 500 after its end.
func (m *Server) take(body string) Reply {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.requests = append(m.requests, body)
	if len(m.requests) > len(m.script) {
		return Reply{status: http.StatusInternalServerError, body: `{"error":{"message":"the script ended"}}`}
	}
	return m.script[len(m.requests)-1]
}

// Asked is the bodies of the chat completions so far.
func (m *Server) Asked() []string {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return append([]string(nil), m.requests...)
}

// BaseURL is the API root a client is pointed at.
func (m *Server) BaseURL() string { return m.URL + "/v1" }
