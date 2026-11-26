//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/mxcd/inkling-chat/internal/modeltest"
)

func messagesPath(conversation string) string {
	return "/api/v1/conversations/" + conversation + "/messages"
}

// splitUsage takes the usage events out of a stream, whose measured times differ from run to
// run, and returns the rest and the data of the usage events.
func splitUsage(stream string) (string, []string) {
	var rest strings.Builder
	var usages []string
	for frame := range strings.SplitSeq(stream, "\n\n") {
		if data, found := strings.CutPrefix(frame, "event: usage\ndata: "); found {
			usages = append(usages, data)
		} else if frame != "" {
			rest.WriteString(frame + "\n\n")
		}
	}
	return rest.String(), usages
}

func TestAConversationUsesTheWorkspaceAndRemembersItsTurns(t *testing.T) {
	t.Parallel()
	model := modeltest.NewServer(t, []string{"served-model"},
		modeltest.ToolCall("call-1", "write_file", `{"path":"notes/ideas.md","content":"1. sleep"}`),
		modeltest.Text("I wrote ", "the file."),
		modeltest.Text("One idea: sleep."),
	)
	server := start(t, model.BaseURL())
	conversation := uuid.NewString()

	status, answer := server.post(t, messagesPath(conversation), `{"text":"Write my ideas to notes/ideas.md.","workspace":true}`)
	require.Equal(t, http.StatusOK, status)
	first, usages := splitUsage(answer)
	require.Len(t, usages, 2, "one usage per model call")
	require.Contains(t, usages[1], `{"usage":{"contextLength":32768,"inputTokens":20,"cachedInputTokens":12,"outputTokens":5,"latencyMs":`)
	require.Contains(t, usages[1], `"promptMs":4.5,"promptTokens":8,"promptPerSecond":1777.8,"predictedPerSecond":100,"draftTokens":4,"draftAccepted":3}}`,
		"the model server's own timings")
	require.Equal(t, strings.Join([]string{
		`event: tool_call`, `data: {"callId":"call-1","tool":"write_file","target":"notes/ideas.md"}`, ``,
		`event: tool_result`, `data: {"callId":"call-1","tool":"write_file","ok":true}`, ``,
		`event: text`, `data: {"text":"I wrote "}`, ``,
		`event: text`, `data: {"text":"the file."}`, ``,
		`event: end`, `data: {"reason":"stop"}`, ``, ``,
	}, "\n"), first)
	written, err := os.ReadFile(filepath.Join(server.workspace, "notes", "ideas.md"))
	require.NoError(t, err)
	require.Equal(t, "1. sleep", string(written))

	status, second := server.post(t, messagesPath(conversation), `{"text":"What was the idea?","thinking":"high"}`)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, second, `data: {"text":"One idea: sleep."}`)

	asked := model.Asked()
	require.Len(t, asked, 3)
	require.Contains(t, asked[0], `"model":"served-model"`, "the server's own model is asked for")
	require.Contains(t, asked[0], `"name":"write_file"`)
	require.Contains(t, asked[0], `"reasoning":{"effort":"none"}`, "thinking off is asked for explicitly")
	require.Contains(t, asked[2], `"reasoning":{"effort":"high"}`)
	require.NotContains(t, asked[2], `"tools"`, "a message without the workspace offers no tool")
	require.Contains(t, asked[2], "Write my ideas to notes/ideas.md.", "the conversation is remembered")
	require.Contains(t, asked[2], "I wrote the file.")
}

func TestRefusedRequestsAnswerInTheErrorShape(t *testing.T) {
	t.Parallel()
	model := modeltest.NewServer(t, []string{"served-model"})
	server := start(t, model.BaseURL())
	for _, testCase := range []struct {
		name, path, body string
		status           int
		code             string
	}{
		{"a conversation id that is no UUID", messagesPath("not-a-uuid"), `{"text":"Hello"}`, http.StatusBadRequest, "invalid_argument"},
		{"a message without text", messagesPath(uuid.NewString()), `{}`, http.StatusBadRequest, "invalid_argument"},
		{"an unknown API route", "/api/v1/nothing", `{}`, http.StatusNotFound, "not_found"},
		{"a message over the limit", messagesPath(uuid.NewString()), `{"text":"` + strings.Repeat("a", 4<<20+1) + `"}`, http.StatusRequestEntityTooLarge, "too_large"},
		{"a body over the limit", messagesPath(uuid.NewString()), `{"text":"` + strings.Repeat("a", 16<<20+1) + `"}`, http.StatusRequestEntityTooLarge, "too_large"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, answer := server.post(t, testCase.path, testCase.body)
			require.Equal(t, testCase.status, status)
			require.Contains(t, answer, `"code":"`+testCase.code+`"`)
		})
	}
	require.Empty(t, model.Asked(), "a refused message never reaches the model")
}

func TestAFailedTurnEndsItsStreamWithAnErrorEvent(t *testing.T) {
	t.Parallel()
	model := modeltest.NewServer(t, []string{"served-model"}, modeltest.Status(http.StatusBadRequest, `{"error":{"message":"context length exceeded"}}`))
	server := start(t, model.BaseURL())

	status, answer := server.post(t, messagesPath(uuid.NewString()), `{"text":"Hello"}`)

	require.Equal(t, http.StatusOK, status)
	require.Contains(t, answer, "event: end\ndata: {\"reason\":\"error\"}\n\n")
	require.Contains(t, answer, "event: error\ndata: {\"code\":\"rejected\",")
	require.NotContains(t, answer, "context length exceeded", "the model server's text stays inside (API-002)")
}

func TestWithoutAModelServerTheServiceIsUnreadyAndAnswers502(t *testing.T) {
	t.Parallel()
	stopped := httptest.NewServer(http.NotFoundHandler())
	stopped.Close()
	server := start(t, stopped.URL+"/v1")

	live, _, err := server.get(t, "/api/health")
	require.NoError(t, err)
	ready, readiness, err := server.get(t, "/api/ready")
	require.NoError(t, err)
	status, answer := server.post(t, messagesPath(uuid.NewString()), `{"text":"Hello"}`)

	require.Equal(t, http.StatusOK, live, "liveness looks at no dependency")
	require.Equal(t, http.StatusServiceUnavailable, ready)
	require.Contains(t, readiness, `"model":"failed"`)
	require.Equal(t, http.StatusBadGateway, status)
	require.Contains(t, answer, `"code":"unavailable"`)
}

func TestTheFrontendAnswersDeepLinksAndTheMetricsStayInternal(t *testing.T) {
	t.Parallel()
	model := modeltest.NewServer(t, []string{"served-model"})
	server := start(t, model.BaseURL())

	root, page, err := server.get(t, "/")
	require.NoError(t, err)
	deepLink, linked, err := server.get(t, "/conversations/42")
	require.NoError(t, err)
	public, publicMetrics, err := server.get(t, "/metrics")
	require.NoError(t, err)
	internal, metrics, err := server.do(t, http.MethodGet, server.metricsURL+"/metrics", "")
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, root)
	require.Contains(t, page, "<!doctype html>")
	require.Equal(t, http.StatusOK, deepLink)
	require.Equal(t, page, linked)
	require.Equal(t, http.StatusOK, public)
	require.NotContains(t, publicMetrics, "inkling_chat_http_requests_total", "the public listener serves the frontend there, not the metrics (MET-001)")
	require.Equal(t, http.StatusOK, internal)
	require.Contains(t, metrics, "inkling_chat_http_requests_total")
}

func TestADirectoryOutsideTheRootsIsRefusedAndOneInsideIsUsed(t *testing.T) {
	t.Parallel()
	model := modeltest.NewServer(t, []string{"served-model"},
		modeltest.ToolCall("call-1", "bash", `{"command":"echo hi > made.txt && pwd"}`),
		modeltest.Text("Done."),
	)
	server := start(t, model.BaseURL())
	project := filepath.Join(server.root, "project")
	require.NoError(t, os.Mkdir(project, 0o750))

	outside, refusal := server.post(t, messagesPath(uuid.NewString()), `{"text":"Hello","workspace":true,"directory":"`+server.workspace+`"}`)
	status, answer := server.post(t, messagesPath(uuid.NewString()), `{"text":"Make a file.","workspace":true,"directory":"`+project+`"}`)

	require.Equal(t, http.StatusBadRequest, outside)
	require.Contains(t, refusal, `"code":"invalid_argument"`)
	require.Contains(t, refusal, "is outside the folders the chat may work in")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, answer, `data: {"callId":"call-1","tool":"bash","target":"echo hi \u003e made.txt \u0026\u0026 pwd"}`)
	made, err := os.ReadFile(filepath.Join(server.root, "project", "made.txt"))
	require.NoError(t, err)
	require.Equal(t, "hi\n", string(made))
	asked := model.Asked()
	require.Len(t, asked, 2, "the refused message never reached the model")
	require.Contains(t, asked[0], "Working directory: ")
	require.Contains(t, asked[0], `"name":"bash"`)
}

func TestTheGuardRefusesCrossSiteRequests(t *testing.T) {
	t.Parallel()
	model := modeltest.NewServer(t, []string{"served-model"})
	server := start(t, model.BaseURL())
	path := server.baseURL + messagesPath(uuid.NewString())
	for _, testCase := range []struct {
		name   string
		header http.Header
		status int
		code   string
	}{
		{"another site's page", http.Header{"Content-Type": {"application/json"}, "Origin": {"https://evil.example"}}, http.StatusForbidden, "forbidden"},
		{"a text/plain post", http.Header{"Content-Type": {"text/plain"}}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"a form post", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, answer, err := server.send(t, http.MethodPost, path, `{"text":"Hello"}`, testCase.header)
			require.NoError(t, err)
			require.Equal(t, testCase.status, status)
			require.Contains(t, answer, `"code":"`+testCase.code+`"`)
		})
	}
	require.Empty(t, model.Asked(), "a refused request never reaches the model")
}

func TestThePersonasAreListed(t *testing.T) {
	t.Parallel()
	model := modeltest.NewServer(t, []string{"served-model"})
	server := start(t, model.BaseURL())

	status, answer, err := server.get(t, "/api/v1/personas")

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, answer, `{"items":[{"id":"assistant","label":"Assistant","description":"A helpful general assistant."}`)
	require.Contains(t, answer, `"id":"golang"`)
}

func TestAScriptAcceptingJSONGetsTheWholeTurnAndCanFollowUp(t *testing.T) {
	t.Parallel()
	model := modeltest.NewServer(t, []string{"served-model"},
		modeltest.ToolCall("call-1", "write_file", `{"path":"answer.txt","content":"42"}`),
		modeltest.Text("Wrote ", "it."),
		modeltest.Text("It says 42."),
	)
	server := start(t, model.BaseURL())
	conversation := uuid.NewString()
	header := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}

	status, answer, err := server.send(t, http.MethodPost, server.baseURL+messagesPath(conversation), `{"text":"Write the answer.","workspace":true}`, header)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	var turn struct {
		Text      string `json:"text"`
		Reason    string `json:"reason"`
		ToolCalls []struct {
			Tool   string `json:"tool"`
			Target string `json:"target"`
			OK     bool   `json:"ok"`
		} `json:"toolCalls"`
		Usage []map[string]any `json:"usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(answer), &turn))
	require.Equal(t, "Wrote it.", turn.Text)
	require.Equal(t, "stop", turn.Reason)
	require.Len(t, turn.ToolCalls, 1)
	require.Equal(t, "write_file", turn.ToolCalls[0].Tool)
	require.Equal(t, "answer.txt", turn.ToolCalls[0].Target)
	require.True(t, turn.ToolCalls[0].OK)
	require.Len(t, turn.Usage, 2, "one usage per model call")

	status, second, err := server.send(t, http.MethodPost, server.baseURL+messagesPath(conversation), `{"text":"What does it say?"}`, header)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, second, `"text":"It says 42."`)
}
