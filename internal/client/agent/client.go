// Package agent is the chat's model side: the client of the model server (an OpenAI-compatible
// API, the local Strata engine) and the runtime that runs a conversation's turns on it with
// govengers/aikido. The service (internal/service ChatService) decides what a turn is; this
// package runs it.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/govengers/aikido/llm"
	"github.com/govengers/aikido/llm/openrouter"
	"github.com/govengers/aikido/llm/oteladapter"
	llmprometheus "github.com/govengers/aikido/llm/prometheusadapter"
	"github.com/govengers/aikido/llm/wideeventadapter"
	wideevent "github.com/govengers/wide-event-logger"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"

	"github.com/mxcd/inkling-chat/internal/model"
)

const (
	// directoryTimeout bounds one request for the model list (RES-001).
	directoryTimeout = 2 * time.Second
	// maximumModelListBytes bounds what is read of a model list.
	maximumModelListBytes = 1 << 20
)

// ClientOptions configure the model client. BaseURL is the API root of the model server
// ("http://127.0.0.1:18081/v1"), APIKey what it gets as bearer token: aikido's client requires
// one, a local server ignores it. Every call counts in Registerer's metrics below Namespace, is a
// span of Traces and adds its tokens and duration to the wide event of its unit (LOG-004). All
// of these are required. MaximumAttempts is how often a call that got no answer, a 429 or a 5xx
// is sent; zero keeps aikido's three, one disables the retry.
type ClientOptions struct {
	BaseURL         string
	APIKey          string
	Namespace       string
	Registerer      prometheus.Registerer
	Traces          trace.TracerProvider
	MaximumAttempts int
}

// NewClient builds the observed model client. The server speaks the chat completions protocol
// OpenRouter speaks, so aikido's OpenRouter client is pointed at it; its transport also reads the
// timings a local server measures, which a Runtime's turn reports with every call's usage.
func NewClient(options *ClientOptions) (llm.Client, error) {
	if options == nil || options.BaseURL == "" || options.APIKey == "" || options.Namespace == "" || options.Registerer == nil || options.Traces == nil {
		return nil, errors.New("agent: ClientOptions needs the BaseURL, the APIKey, the Namespace, the Registerer and the Traces")
	}
	provider, err := openrouter.NewClient(&openrouter.Options{
		APIKey: options.APIKey, BaseURL: strings.TrimRight(options.BaseURL, "/"), XTitle: options.Namespace, MaximumAttempts: options.MaximumAttempts,
		HTTPClient: newHTTPClient(),
	})
	if err != nil {
		return nil, fmt.Errorf("agent: model client: %w", err)
	}
	metrics, err := llmprometheus.New(&llmprometheus.Options{Registerer: options.Registerer, Namespace: options.Namespace})
	if err != nil {
		return nil, fmt.Errorf("agent: model metrics: %w", err)
	}
	spans, err := oteladapter.New(&oteladapter.Options{TracerProvider: options.Traces})
	if err != nil {
		return nil, fmt.Errorf("agent: model spans: %w", err)
	}
	observed, err := llm.NewObservedClient(&llm.ObservedClientOptions{Client: provider, Name: "model", Observers: []llm.Observer{metrics, spans, wideeventadapter.New()}})
	if err != nil {
		return nil, fmt.Errorf("agent: observed model client: %w", err)
	}
	return observed, nil
}

// DirectoryOptions configure a Directory: the model server's API root and its bearer token, both
// required.
type DirectoryOptions struct {
	BaseURL string
	APIKey  string
}

// Directory reads the model server's model list: the readiness check of the one system every
// turn needs (LAY-004), and the name of the model a server with one model serves.
type Directory struct {
	modelsURL  string
	apiKey     string
	httpClient *http.Client
}

// NewDirectory fails without its options.
func NewDirectory(options *DirectoryOptions) (*Directory, error) {
	if options == nil || options.BaseURL == "" || options.APIKey == "" {
		return nil, errors.New("agent: DirectoryOptions needs the BaseURL and the APIKey")
	}
	return &Directory{
		modelsURL:  strings.TrimRight(options.BaseURL, "/") + "/models",
		apiKey:     options.APIKey,
		httpClient: &http.Client{Timeout: directoryTimeout},
	}, nil
}

// Check fails while the model server does not answer its model list.
func (d *Directory) Check(ctx context.Context) error {
	_, err := d.models(ctx)
	return err
}

// ServedModel is the name of the first model the server lists; it fails with
// model.ErrUnavailable while the server does not answer or lists none.
func (d *Directory) ServedModel(ctx context.Context) (model.ServedModel, error) {
	start := time.Now()
	served, err := d.models(ctx)
	we := wideevent.FromContext(ctx)
	we.Duration("http.model.models_duration", time.Since(start)).Int("http.model.models", len(served))
	if err != nil {
		return model.ServedModel{}, fmt.Errorf("%w: %w", model.ErrUnavailable, err)
	}
	if len(served) == 0 {
		return model.ServedModel{}, fmt.Errorf("%w: the model server lists no model", model.ErrUnavailable)
	}
	return served[0], nil
}

// models is the server's model list. The context window is OpenRouter's context_length, or vLLM's
// max_model_len for a server that names only that.
func (d *Directory) models(ctx context.Context) ([]model.ServedModel, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.modelsURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("agent: model list request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+d.apiKey)
	response, err := d.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("agent: model list: %w", err)
	}
	defer response.Body.Close() // read to its limit below: a failed close loses nothing
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent: model list: status %d", response.StatusCode)
	}
	var listed struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
			MaxModelLen   int    `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maximumModelListBytes)).Decode(&listed); err != nil {
		return nil, fmt.Errorf("agent: model list: %w", err)
	}
	served := make([]model.ServedModel, 0, len(listed.Data))
	for _, entry := range listed.Data {
		served = append(served, model.ServedModel{Name: entry.ID, ContextLength: max(entry.ContextLength, entry.MaxModelLen)})
	}
	return served, nil
}
