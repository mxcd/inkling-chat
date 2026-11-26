package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/govengers/telemetry/health"
	wideevent "github.com/govengers/wide-event-logger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
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

func (r *recorder) count() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return len(r.events)
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

// routes is a controller with one route that answers and one that panics.
type routes struct{}

func (routes) RegisterRoutes(router gin.IRouter) {
	router.POST("/echo", func(ginContext *gin.Context) {
		var body map[string]any
		if err := ginContext.ShouldBindJSON(&body); err != nil {
			ginContext.Status(http.StatusRequestEntityTooLarge)
			return
		}
		ginContext.Status(http.StatusNoContent)
	})
	router.GET("/panic", func(*gin.Context) { panic("the handler broke") })
}

type testRouter struct {
	engine   *gin.Engine
	emitted  *recorder
	registry *prometheus.Registry
}

// newTestRouter is the router with the routes controller, a frontend that answers "frontend"
// and one readiness check.
func newTestRouter(t *testing.T, check health.Check) testRouter {
	t.Helper()
	gin.SetMode(gin.TestMode)
	probes, err := health.New(&health.Options{Checks: map[string]health.Check{"model": check}})
	require.NoError(t, err)
	built := testRouter{emitted: &recorder{}, registry: prometheus.NewRegistry()}
	built.engine, err = NewRouter(&RouterOptions{
		Controllers: []Controller{routes{}}, Health: probes, Emitter: built.emitted, Metrics: built.registry, Traces: noop.NewTracerProvider(),
		UI: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, err := io.WriteString(writer, "frontend")
			require.NoError(t, err)
		}),
		Namespace: "test", Version: "1.2.3", ListenAddress: listenAddress,
	})
	require.NoError(t, err)
	return built
}

// listenAddress is the test router's loopback listen address.
const listenAddress = "127.0.0.1:8080"

// request sends what the frontend sends: to the listen address, a body as JSON.
func (r testRouter) request(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return r.send(t, sent{method: method, path: path, host: listenAddress, header: http.Header{"Content-Type": {"application/json"}}}, body)
}

// sent is a request with exactly its headers, to its host.
type sent struct {
	method, path, host string
	header             http.Header
}

func (r testRouter) send(t *testing.T, what sent, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), what.method, what.path, strings.NewReader(body))
	request.Host = what.host
	request.Header = what.header
	answer := httptest.NewRecorder()
	r.engine.ServeHTTP(answer, request)
	return answer
}

func ready(context.Context) error { return nil }

func TestAnUnknownAPIPathIsThe404ErrorBodyAndADeepLinkTheFrontend(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, ready)

	unknown := router.request(t, http.MethodGet, "/api/v1/nothing", "")
	bare := router.request(t, http.MethodGet, "/api", "")
	deepLink := router.request(t, http.MethodGet, "/conversations/42", "")
	lookalike := router.request(t, http.MethodGet, "/apiary", "")

	require.Equal(t, http.StatusNotFound, unknown.Code)
	require.JSONEq(t, `{"code":"not_found","message":"Not found."}`, unknown.Body.String())
	require.Equal(t, http.StatusNotFound, bare.Code)
	require.Equal(t, http.StatusOK, deepLink.Code)
	require.Equal(t, "frontend", deepLink.Body.String())
	require.Equal(t, "frontend", lookalike.Body.String(), "only /api and what is below it is the API")
}

func TestTheProbesAnswerWithoutAWideEvent(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, ready)
	unready := newTestRouter(t, func(context.Context) error { return errors.New("the model server is down") })

	require.Equal(t, http.StatusOK, router.request(t, http.MethodGet, health.LivenessRoute, "").Code)
	require.Equal(t, http.StatusOK, router.request(t, http.MethodGet, health.ReadinessRoute, "").Code)
	require.Equal(t, http.StatusOK, unready.request(t, http.MethodGet, health.LivenessRoute, "").Code)
	answer := unready.request(t, http.MethodGet, health.ReadinessRoute, "")
	require.Equal(t, http.StatusServiceUnavailable, answer.Code)
	require.NotContains(t, answer.Body.String(), "is down", "a probe names no error text")
	require.Zero(t, router.emitted.count())
}

func TestEveryRequestHasAWideEventWithTheVersionAndCountsInTheMetrics(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, ready)

	answer := router.request(t, http.MethodPost, APIBasePath+"/echo", `{"text":"hello"}`)

	require.Equal(t, http.StatusNoContent, answer.Code)
	require.Equal(t, 1, router.emitted.count())
	require.Equal(t, "1.2.3", router.emitted.last("version"))
	require.Equal(t, 1, testutil.CollectAndCount(router.registry, "test_http_requests_total"))
}

func TestAPanicIsThe500ErrorBodyAndARecordedFailure(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, ready)

	answer := router.request(t, http.MethodGet, APIBasePath+"/panic", "")

	require.Equal(t, http.StatusInternalServerError, answer.Code)
	require.JSONEq(t, `{"code":"internal","message":"Internal error."}`, answer.Body.String())
	require.Equal(t, "failure", router.emitted.last("outcome"))
}

func TestABodyOverTheLimitIsNotRead(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, ready)

	answer := router.request(t, http.MethodPost, APIBasePath+"/echo", `{"text":"`+strings.Repeat("a", MaximumBodyBytes)+`"}`)

	require.Equal(t, http.StatusRequestEntityTooLarge, answer.Code)
}

func TestNewRouterFailsWithoutOneOfItsOptions(t *testing.T) {
	t.Parallel()
	probes, err := health.New(&health.Options{})
	require.NoError(t, err)
	ui := http.NotFoundHandler()
	complete := RouterOptions{
		ListenAddress: listenAddress, Controllers: []Controller{routes{}}, Health: probes, Emitter: &recorder{}, Metrics: prometheus.NewRegistry(),
		Traces: noop.NewTracerProvider(), UI: ui, Namespace: "test", Version: "1.2.3",
	}
	for _, testCase := range []struct {
		name  string
		unset func(options *RouterOptions)
	}{
		{"the listen address", func(options *RouterOptions) { options.ListenAddress = "" }},
		{"a listen address without a port", func(options *RouterOptions) { options.ListenAddress = "localhost" }},
		{"the controllers", func(options *RouterOptions) { options.Controllers = nil }},
		{"the health", func(options *RouterOptions) { options.Health = nil }},
		{"the emitter", func(options *RouterOptions) { options.Emitter = nil }},
		{"the metrics", func(options *RouterOptions) { options.Metrics = nil }},
		{"the traces", func(options *RouterOptions) { options.Traces = nil }},
		{"the frontend", func(options *RouterOptions) { options.UI = nil }},
		{"the namespace", func(options *RouterOptions) { options.Namespace = "" }},
		{"the version", func(options *RouterOptions) { options.Version = "" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			options := complete
			testCase.unset(&options)
			_, err := NewRouter(&options)
			require.Error(t, err)
		})
	}
	_, err = NewRouter(nil)
	require.Error(t, err)
}

// freeAddress is a loopback address nothing listens on right now.
func freeAddress(t *testing.T) string {
	t.Helper()
	var configuration net.ListenConfig
	listener, err := configuration.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	return address
}

func TestTheListenerServesUntilItsContextEndsThenWithdrawsReadinessAndDrains(t *testing.T) {
	t.Parallel()
	probes, err := health.New(&health.Options{})
	require.NoError(t, err)
	address := freeAddress(t)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	listener, err := NewListener(&ListenerOptions{HTTPServer: NewHTTPServer(address, handler), Health: probes})
	require.NoError(t, err)
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- listener.Run(ctx) }()
	client := &http.Client{Timeout: time.Second}
	t.Cleanup(client.CloseIdleConnections)
	require.Eventually(t, func() bool {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/", http.NoBody)
		if err != nil {
			return false
		}
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		return response.Body.Close() == nil && response.StatusCode == http.StatusNoContent
	}, 5*time.Second, 5*time.Millisecond)

	client.CloseIdleConnections()
	stop()

	require.NoError(t, <-done)
	readiness := httptest.NewRecorder()
	probes.ReadinessHandler().ServeHTTP(readiness, httptest.NewRequestWithContext(t.Context(), http.MethodGet, health.ReadinessRoute, http.NoBody))
	require.Equal(t, http.StatusServiceUnavailable, readiness.Code, "the readiness is withdrawn before the drain")
}

func TestAListenerThatCannotListenFailsAtOnce(t *testing.T) {
	t.Parallel()
	var configuration net.ListenConfig
	taken, err := configuration.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, taken.Close()) })
	listener, err := NewListener(&ListenerOptions{HTTPServer: NewHTTPServer(taken.Addr().String(), http.NotFoundHandler())})
	require.NoError(t, err)

	err = listener.Run(t.Context())

	require.ErrorContains(t, err, "listen on")
}

func TestNewListenerValidatesItsOptions(t *testing.T) {
	t.Parallel()
	httpServer := NewHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	_, err := NewListener(nil)
	require.Error(t, err)
	_, err = NewListener(&ListenerOptions{})
	require.Error(t, err)
	_, err = NewListener(&ListenerOptions{HTTPServer: httpServer, ShutdownBudget: -time.Second})
	require.Error(t, err)
	_, err = NewListener(&ListenerOptions{HTTPServer: httpServer, ReadinessDelay: -time.Second})
	require.Error(t, err)
}

func TestRecoveringTurnsAPanicIntoAnError(t *testing.T) {
	t.Parallel()
	err := Recovering(func() error { panic("the worker broke") })()
	require.ErrorContains(t, err, "the worker broke")
	require.NoError(t, Recovering(func() error { return nil })())
}

func TestTheGuardRefusesWhatAnotherSitesPageCouldSend(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, ready)
	json := func(origin string) http.Header {
		header := http.Header{"Content-Type": {"application/json; charset=utf-8"}}
		if origin != "" {
			header.Set("Origin", origin)
		}
		return header
	}
	for _, testCase := range []struct {
		name, method, path, host string
		header                   http.Header
		status                   int
		code                     string
	}{
		{"the server's own page", http.MethodPost, APIBasePath + "/echo", listenAddress, json("http://" + listenAddress), http.StatusNoContent, ""},
		{"no origin (curl)", http.MethodPost, APIBasePath + "/echo", listenAddress, json(""), http.StatusNoContent, ""},
		{"the dev server through its proxy, which sets the target's host", http.MethodPost, APIBasePath + "/echo", "127.0.0.1:8080", json("http://localhost:5173"), http.StatusNoContent, ""},
		{"the dev server on 127.0.0.1", http.MethodPost, APIBasePath + "/echo", "127.0.0.1:8080", json("http://127.0.0.1:5173"), http.StatusNoContent, ""},
		{"localhost by name", http.MethodPost, APIBasePath + "/echo", "localhost:8080", json("http://localhost:8080"), http.StatusNoContent, ""},
		{"IPv6 loopback", http.MethodPost, APIBasePath + "/echo", "[::1]:8080", json("http://[::1]:8080"), http.StatusNoContent, ""},
		{"another site's page", http.MethodPost, APIBasePath + "/echo", listenAddress, json("https://evil.example"), http.StatusForbidden, "forbidden"},
		{"another port of this computer", http.MethodPost, APIBasePath + "/echo", listenAddress, json("http://127.0.0.1:9999"), http.StatusForbidden, "forbidden"},
		{"a null origin (sandboxed frame)", http.MethodPost, APIBasePath + "/echo", listenAddress, json("null"), http.StatusForbidden, "forbidden"},
		{"a rebound DNS name", http.MethodPost, APIBasePath + "/echo", "evil.example:8080", json("http://evil.example:8080"), http.StatusForbidden, "forbidden"},
		{"a rebound DNS name for the frontend", http.MethodGet, "/", "evil.example:8080", http.Header{}, http.StatusForbidden, "forbidden"},
		{"a form post", http.MethodPost, APIBasePath + "/echo", listenAddress, http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"a text/plain post", http.MethodPost, APIBasePath + "/echo", listenAddress, http.Header{"Content-Type": {"text/plain"}}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"a post without a content type", http.MethodPost, APIBasePath + "/echo", listenAddress, http.Header{}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"a get needs no content type", http.MethodGet, APIBasePath + "/panic", listenAddress, http.Header{}, http.StatusInternalServerError, "internal"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			answer := router.send(t, sent{method: testCase.method, path: testCase.path, host: testCase.host, header: testCase.header}, `{"text":"hello"}`)

			require.Equal(t, testCase.status, answer.Code, answer.Body.String())
			if testCase.code != "" {
				require.Contains(t, answer.Body.String(), `"code":"`+testCase.code+`"`)
			}
		})
	}
}

func TestOnAnAddressOfAllInterfacesAnyHostIsServed(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	probes, err := health.New(&health.Options{})
	require.NoError(t, err)
	engine, err := NewRouter(&RouterOptions{
		ListenAddress: ":8080", Controllers: []Controller{routes{}}, Health: probes, Emitter: &recorder{}, Metrics: prometheus.NewRegistry(),
		Traces: noop.NewTracerProvider(), UI: http.NotFoundHandler(), Namespace: "test", Version: "1.2.3",
	})
	require.NoError(t, err)
	router := testRouter{engine: engine}

	answer := router.send(t, sent{method: http.MethodPost, path: APIBasePath + "/echo", host: "chat.example:8080", header: http.Header{"Content-Type": {"application/json"}}}, `{}`)

	require.Equal(t, http.StatusNoContent, answer.Code)
}
