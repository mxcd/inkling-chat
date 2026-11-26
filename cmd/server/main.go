// Command server is inkling-chat: a chat agent on a model server that speaks the OpenAI chat
// completions API (the local Strata engine with Inkling 2 Mini), its API, the embedded frontend
// and /metrics on an internal listener. Conversations live in memory; the agent's tools work in
// the directory a conversation chooses below WORKSPACE_ROOTS, the workspace directory without one.
//
// Development: `just dev` runs it behind the frontend's dev server.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	aikidoagent "github.com/govengers/aikido/agent"
	historymemory "github.com/govengers/aikido/agent/history/memory"
	"github.com/govengers/config/v2"
	"github.com/govengers/telemetry"
	"github.com/govengers/telemetry/health"
	"github.com/govengers/telemetry/tracing"
	wideevent "github.com/govengers/wide-event-logger"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/sync/errgroup"

	"github.com/mxcd/inkling-chat/internal/client/agent"
	"github.com/mxcd/inkling-chat/internal/controller"
	"github.com/mxcd/inkling-chat/internal/server"
	"github.com/mxcd/inkling-chat/internal/service"
	"github.com/mxcd/inkling-chat/internal/web"
)

// version is set at build time with -ldflags "-X main.version=<version>" (DEP-005).
var version = "dev"

const (
	// namespace names the service in its metrics and spans.
	namespace = "inkling_chat"
	// traceFlushTimeout bounds the last step of the drain (TRC-009).
	traceFlushTimeout = 3 * time.Second
	// warmUpTimeout bounds the startup's warm-up call: the model server reads a few thousand
	// tokens, after a turn that may be running.
	warmUpTimeout = 2 * time.Minute
	// workspacePermissions are those of a workspace directory the service creates.
	workspacePermissions = 0o750
	// goLookupTimeout bounds the startup's go env, which asks go for its caches.
	goLookupTimeout = 30 * time.Second
)

// configuration is every value the service reads from the environment (CFG-001).
type configuration struct {
	address         string
	metricsAddress  string
	developmentMode bool
	readinessDelay  time.Duration
	modelBaseURL    url.URL
	modelAPIKey     string
	modelName       string
	workspacePath   string
	workspaceRoots  []string
	goBinary        string
	bunBinary       string
	warmUp          bool
	promptCaching   bool
	temperature     float64
	toolTimeouts    agent.Timeouts
}

// observability is what every layer records with: the wide events' emitter, the metrics
// registry and the tracer provider.
type observability struct {
	emitter wideevent.Emitter
	metrics *prometheus.Registry
	traces  *tracing.Provider
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run bootstraps every layer in order (LAY-002) and serves until SIGTERM. There is no database:
// the steps between the metrics registry and the clients are left out.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	values, err := loadConfiguration()
	if err != nil {
		return err
	}
	config.Print()
	gin.SetMode(gin.ReleaseMode) // gin's route listing is not the service's log
	emitter, err := newEmitter(values.developmentMode)
	if err != nil {
		return err
	}
	traces, err := newTraces(ctx)
	if err != nil {
		return err
	}
	defer flushTraces(traces) // last: after the servers stopped (TRC-009)
	observed := &observability{emitter: emitter, metrics: telemetry.NewRegistry(), traces: traces}
	clients, err := newClients(ctx, values, observed)
	if err != nil {
		return err
	}
	defer clients.close()
	chatService, err := service.NewChatService(&service.ChatServiceOptions{
		TurnRunner: clients.runtime, Model: values.modelName, ModelDirectory: clients.directory,
		Workspace: clients.workspace, WorkspaceRoots: values.workspaceRoots, Commands: clients.commandNames(),
	})
	if err != nil {
		return err
	}
	listeners, err := newListeners(values, chatService, clients.directory, observed)
	if err != nil {
		return err
	}
	recordStartup(emitter, values, clients)
	if values.warmUp {
		go warmUp(ctx, chatService, emitter)
	}
	return serve(ctx, listeners)
}

func loadConfiguration() (*configuration, error) {
	values := &configuration{}
	roots := []string{} // without a home directory and WORKSPACE_ROOTS, chats work only in the workspace
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, home)
	}
	err := config.LoadConfig([]config.Value{
		config.String("LISTEN_ADDRESS").Default("127.0.0.1:8080").NotEmpty().Into(&values.address),         // loopback: the API has no authentication
		config.String("METRICS_ADDRESS").Default("127.0.0.1:9090").NotEmpty().Into(&values.metricsAddress), // internal listener (MET-001)
		config.Bool("DEV_MODE").Default(false).Into(&values.developmentMode),
		config.Duration("READINESS_DELAY").Default(5 * time.Second).Into(&values.readinessDelay),
		config.URL("MODEL_BASE_URL").Schemes("http", "https").Default("http://127.0.0.1:18081/v1").Into(&values.modelBaseURL),
		config.String("MODEL_API_KEY").Default("local").NotEmpty().Sensitive().Into(&values.modelAPIKey), // a local server ignores it
		config.String("MODEL_NAME").Default("").Into(&values.modelName),                                  // empty: the model the server serves
		config.String("WORKSPACE_PATH").Default("workspace").NotEmpty().Into(&values.workspacePath),
		config.StringArray("WORKSPACE_ROOTS").Default(roots).Into(&values.workspaceRoots),                                   // a conversation's directory lies below one
		config.String("GO_BINARY").Default("").Into(&values.goBinary),                                                       // empty: the go in PATH
		config.String("BUN_BINARY").Default("").Into(&values.bunBinary),                                                     // empty: the bun in PATH
		config.Duration("TOOL_TIMEOUT").Default(agent.DefaultToolTimeout).Into(&values.toolTimeouts.Default),                // every tool call
		config.Duration("TOOL_TIMEOUT_MAXIMUM").Default(agent.DefaultMaximumToolTimeout).Into(&values.toolTimeouts.Maximum), // what a bash call may ask for
		config.Bool("MODEL_WARM_UP").Default(true).Into(&values.warmUp),                                                     // the model server reads the file tools' prompt start at startup
		config.Bool("MODEL_PROMPT_CACHING").Default(false).Into(&values.promptCaching),                                      // breakpoints for Anthropic models on OpenRouter
		config.Float("MODEL_TEMPERATURE").Default(-1).Into(&values.temperature),                                             // negative: the model server's default
	})
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	return values, nil
}

func newEmitter(development bool) (wideevent.Emitter, error) {
	if development {
		return wideevent.NewDevEmitter(&wideevent.DevEmitterOptions{Writer: os.Stderr})
	}
	return wideevent.NewJSONEmitter(&wideevent.JSONEmitterOptions{Writer: os.Stderr}), nil
}

// newTraces builds the tracer provider from OTEL_*: a no-op one without a collector (TRC-002).
func newTraces(ctx context.Context) (*tracing.Provider, error) {
	traces, err := tracing.New(ctx, &tracing.Options{ServiceName: namespace, ServiceVersion: version})
	if err != nil {
		return nil, fmt.Errorf("tracing: %w", err)
	}
	otel.SetTracerProvider(traces.TracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return traces, nil
}

func flushTraces(traces *tracing.Provider) {
	flushContext, cancel := context.WithTimeout(context.Background(), traceFlushTimeout)
	defer cancel()
	if err := traces.Shutdown(flushContext); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

// clientSet is the model side: the runtime of the turns, the directory of the model server's
// models, the absolute workspace of the turns that name no directory, the host commands the
// runtime's shell offers and why the others are missing.
type clientSet struct {
	runtime   *agent.Runtime
	directory *agent.Directory
	workspace string
	commands  []agent.HostCommand
	missing   map[string]error
}

func (c *clientSet) commandNames() []string {
	names := make([]string, 0, len(c.commands))
	for _, command := range c.commands {
		names = append(names, command.Name)
	}
	return names
}

// findCommands looks up the host's go and bun; a missing one is left out, and the shell works
// without it.
func findCommands(ctx context.Context, values *configuration) ([]agent.HostCommand, map[string]error) {
	lookupContext, cancel := context.WithTimeout(ctx, goLookupTimeout)
	defer cancel()
	var found []agent.HostCommand
	missing := map[string]error{}
	goCommand, err := agent.FindGo(lookupContext, values.goBinary)
	if err != nil {
		missing[agent.GoCommandName] = err
	} else {
		found = append(found, *goCommand)
	}
	bunCommand, err := agent.FindBun(values.bunBinary)
	if err != nil {
		missing[agent.BunCommandName] = err
	} else {
		found = append(found, *bunCommand)
	}
	return found, missing
}

func newClients(ctx context.Context, values *configuration, observed *observability) (*clientSet, error) {
	if err := os.MkdirAll(values.workspacePath, workspacePermissions); err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	workspace, err := filepath.Abs(values.workspacePath)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	commands, missing := findCommands(ctx, values)
	baseURL := values.modelBaseURL.String()
	modelClient, err := agent.NewClient(&agent.ClientOptions{
		BaseURL: baseURL, APIKey: values.modelAPIKey, Namespace: namespace, Registerer: observed.metrics, Traces: observed.traces.TracerProvider(),
	})
	if err != nil {
		return nil, err
	}
	runtime, err := agent.New(&agent.Options{Client: modelClient, History: historymemory.New(), Locker: aikidoagent.NewLocalLocker(), Commands: commands, Timeouts: values.toolTimeouts, PromptCaching: values.promptCaching, Temperature: temperature(values.temperature)})
	if err != nil {
		return nil, err
	}
	directory, err := agent.NewDirectory(&agent.DirectoryOptions{BaseURL: baseURL, APIKey: values.modelAPIKey})
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	return &clientSet{runtime: runtime, directory: directory, workspace: workspace, commands: commands, missing: missing}, nil
}

func (c *clientSet) close() {
	if err := c.runtime.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

// listenerSet is the API's listener and the internal one of /metrics.
type listenerSet struct {
	api     *server.Listener
	metrics *server.Listener
}

func newListeners(values *configuration, chatService *service.ChatService, directory *agent.Directory, observed *observability) (*listenerSet, error) {
	probes, err := health.New(&health.Options{
		Checks:         map[string]health.Check{"model": directory.Check},
		OnCheckFailure: recordUnready(observed.emitter),
	})
	if err != nil {
		return nil, fmt.Errorf("health probes: %w", err)
	}
	router, err := newRouter(values.address, chatService, probes, observed)
	if err != nil {
		return nil, err
	}
	api, err := server.NewListener(&server.ListenerOptions{
		HTTPServer: server.NewHTTPServer(values.address, router), Health: probes, ReadinessDelay: values.readinessDelay,
	})
	if err != nil {
		return nil, err
	}
	metricsServer, err := telemetry.NewMetricsServer(&telemetry.MetricsServerOptions{Registry: observed.metrics, Address: values.metricsAddress})
	if err != nil {
		return nil, fmt.Errorf("metrics server: %w", err)
	}
	metrics, err := server.NewListener(&server.ListenerOptions{HTTPServer: metricsServer})
	if err != nil {
		return nil, err
	}
	return &listenerSet{api: api, metrics: metrics}, nil
}

func newRouter(address string, chatService *service.ChatService, probes *health.Health, observed *observability) (*gin.Engine, error) {
	chatController, err := controller.NewChatController(&controller.ChatControllerOptions{ChatService: chatService})
	if err != nil {
		return nil, err
	}
	ui, err := web.Handler()
	if err != nil {
		return nil, fmt.Errorf("frontend: %w", err)
	}
	return server.NewRouter(&server.RouterOptions{
		ListenAddress: address, Controllers: []server.Controller{chatController}, Health: probes, Emitter: observed.emitter, Metrics: observed.metrics,
		Traces: observed.traces.TracerProvider(), UI: ui, Namespace: namespace, Version: version,
	})
}

// recordUnready gives every failed readiness check a wide event of its own: the probes run
// outside any request (LOG-001).
func recordUnready(emitter wideevent.Emitter) func(name string, err error) {
	return func(name string, err error) {
		we := wideevent.Begin(emitter, "readiness")
		we.String("readiness.check", name).Failure(err)
		we.Emit()
	}
}

// warmUp has the model server read the start of a turn with the file tools once, in the
// background, so startup and readiness do not wait for it. Its wide event records the outcome; a
// failure changes nothing else.
func warmUp(ctx context.Context, chatService *service.ChatService, emitter wideevent.Emitter) {
	we := wideevent.Begin(emitter, "warm_up")
	defer we.Emit()
	warmContext, cancel := context.WithTimeout(wideevent.NewContext(ctx, we), warmUpTimeout)
	defer cancel()
	if err := server.Recovering(func() error { return chatService.WarmUp(warmContext) })(); err != nil {
		we.Failure(err)
		return
	}
	we.Success()
}

// recordStartup reports which version starts with what (OBS-002), and which host commands the
// shell has: one line says which are missing.
func recordStartup(emitter wideevent.Emitter, values *configuration, clients *clientSet) {
	we := wideevent.Begin(emitter, "startup")
	we.String("version", version).
		String("listen.address", values.address).
		String("metrics.address", values.metricsAddress).
		String("model.base_url", values.modelBaseURL.Redacted()).
		String("model.name", values.modelName).
		String("workspace.path", clients.workspace).
		String("workspace.roots", strings.Join(values.workspaceRoots, ","))
	for _, command := range clients.commands {
		we.String("command."+command.Name, command.Path)
	}
	for name, err := range clients.missing {
		we.Err("command."+name+".missing", err).Bool("degraded", true) // the shell runs without it
	}
	we.Success()
	we.Emit()
}

// serve runs both listeners until ctx ends; a listener that fails stops the other.
func serve(ctx context.Context, listeners *listenerSet) error {
	group, groupContext := errgroup.WithContext(ctx)
	group.Go(server.Recovering(func() error { return listeners.api.Run(groupContext) }))
	group.Go(server.Recovering(func() error { return listeners.metrics.Run(groupContext) }))
	return group.Wait()
}

// temperature is MODEL_TEMPERATURE for the model client: nil for a negative value, which leaves the
// model server's default.
func temperature(value float64) *float32 {
	if value < 0 {
		return nil
	}
	return new(float32(value))
}
