package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	wideevent "github.com/govengers/wide-event-logger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/mxcd/inkling-chat/internal/model"
	"github.com/mxcd/inkling-chat/internal/modeltest"
)

func TestNewClientFailsWithoutOneOfItsOptions(t *testing.T) {
	t.Parallel()
	complete := ClientOptions{BaseURL: "http://127.0.0.1:1/v1", APIKey: "local", Namespace: "test", Registerer: prometheus.NewRegistry(), Traces: noop.NewTracerProvider()}
	for _, testCase := range []struct {
		name  string
		unset func(options *ClientOptions)
	}{
		{"the base URL", func(options *ClientOptions) { options.BaseURL = "" }},
		{"the API key", func(options *ClientOptions) { options.APIKey = "" }},
		{"the namespace", func(options *ClientOptions) { options.Namespace = "" }},
		{"the registerer", func(options *ClientOptions) { options.Registerer = nil }},
		{"the traces", func(options *ClientOptions) { options.Traces = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			options := complete
			options.Registerer = prometheus.NewRegistry()
			testCase.unset(&options)
			_, err := NewClient(&options)
			require.Error(t, err)
		})
	}
	_, err := NewClient(nil)
	require.Error(t, err)
	_, err = NewClient(&complete)
	require.NoError(t, err)
}

func TestTheClientsMetricsAreRegisteredOnceOnTheGivenRegistry(t *testing.T) {
	t.Parallel()
	registry := prometheus.NewRegistry()
	options := &ClientOptions{BaseURL: "http://127.0.0.1:1/v1", APIKey: "local", Namespace: "test", Registerer: registry, Traces: noop.NewTracerProvider()}
	_, err := NewClient(options)
	require.NoError(t, err)

	_, err = NewClient(options)

	require.Error(t, err, "a second client on the same registry would count twice")
}

func TestTheDirectoryNamesTheModelTheServerServes(t *testing.T) {
	t.Parallel()
	server := modeltest.NewServer(t, []string{"inkling-2-mini-sq-20", "another"})
	directory, err := NewDirectory(&DirectoryOptions{BaseURL: server.BaseURL() + "/", APIKey: "local"})
	require.NoError(t, err)
	we := wideevent.Begin(discard{}, "test")

	served, err := directory.ServedModel(wideevent.NewContext(t.Context(), we))

	require.NoError(t, err)
	require.Equal(t, model.ServedModel{Name: "inkling-2-mini-sq-20", ContextLength: modeltest.ContextLength}, served)
	require.Equal(t, 2, field(we, "http.model.models"), "the request is on the unit's wide event (LOG-004)")
	require.NoError(t, directory.Check(t.Context()))
}

func TestTheDirectoryIsUnavailableWhileTheServerDoesNotListAModel(t *testing.T) {
	t.Parallel()
	failing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Connection", "close")
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(failing.Close)
	garbled := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Connection", "close")
		_, err := writer.Write([]byte("<html>"))
		require.NoError(t, err)
	}))
	t.Cleanup(garbled.Close)
	stopped := httptest.NewServer(http.NotFoundHandler())
	stopped.Close()
	for _, testCase := range []struct {
		name, baseURL string
	}{
		{"an empty list", modeltest.NewServer(t, nil).BaseURL()},
		{"a failing server", failing.URL},
		{"an answer that is no model list", garbled.URL},
		{"a server that is not running", stopped.URL},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			directory, err := NewDirectory(&DirectoryOptions{BaseURL: testCase.baseURL, APIKey: "local"})
			require.NoError(t, err)

			_, err = directory.ServedModel(t.Context())

			require.ErrorIs(t, err, model.ErrUnavailable)
		})
	}
}

func TestTheReadinessCheckFailsWhileTheServerDoesNotAnswer(t *testing.T) {
	t.Parallel()
	stopped := httptest.NewServer(http.NotFoundHandler())
	stopped.Close()
	directory, err := NewDirectory(&DirectoryOptions{BaseURL: stopped.URL, APIKey: "local"})
	require.NoError(t, err)

	require.Error(t, directory.Check(t.Context()))
}

func TestNewDirectoryFailsWithoutItsOptions(t *testing.T) {
	t.Parallel()
	_, err := NewDirectory(nil)
	require.Error(t, err)
	_, err = NewDirectory(&DirectoryOptions{BaseURL: "http://127.0.0.1:1/v1"})
	require.Error(t, err)
}

// discard is an emitter for events a test only reads the fields of.
type discard struct{}

func (discard) Emit(*wideevent.Event) {}

// field is the value an event holds under key, nil without one.
func field(we *wideevent.Event, key string) any {
	for _, recorded := range we.Fields() {
		if recorded.Key == key {
			return recorded.Value
		}
	}
	return nil
}
