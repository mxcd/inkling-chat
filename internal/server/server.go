// Package server builds the chat's HTTP side: the router with the request's span, the recovery,
// the wide-event middleware, the request metrics, the request guard and the body limit, the
// health routes, the controllers below the API's base path and the frontend last; and the
// listener that serves a server until shutdown and drains it.
package server

import (
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"
	telemetryginadapter "github.com/govengers/telemetry/ginadapter"
	"github.com/govengers/telemetry/health"
	"github.com/govengers/telemetry/tracing"
	wideevent "github.com/govengers/wide-event-logger"
	wideeventginadapter "github.com/govengers/wide-event-logger/ginadapter"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/mxcd/inkling-chat/internal/controller"
	"github.com/mxcd/inkling-chat/internal/model"
)

// APIBasePath is where the controllers' routes are mounted.
const APIBasePath = "/api/v1"

// apiPrefix is the path below which an unknown route is the API's 404, never the frontend
// (API-010): the versioned API and the probes.
const apiPrefix = "/api"

// MaximumBodyBytes bounds every request body (API-005): four times the longest message.
const MaximumBodyBytes = 16 << 20

// probes are the health routes: no wide event, no span, no request metrics (LAY-004).
var probes = []string{health.LivenessRoute, health.ReadinessRoute}

// developmentOrigins are the frontend's dev server (just dev), whose pages call the API through
// its proxy.
var developmentOrigins = []string{"http://localhost:5173", "http://127.0.0.1:5173"}

// Controller is implemented by every controller; the router mounts them below APIBasePath.
type Controller interface {
	RegisterRoutes(router gin.IRouter)
}

// RouterOptions configure the router; every field is required. Health answers the probes,
// Emitter takes the requests' wide events, Metrics is the registry of the request metrics below
// Namespace, Traces the provider of the requests' spans (a no-op one without a collector), UI
// serves the frontend, Version goes on every wide event (OBS-002). ListenAddress is the API's
// listen address: on a loopback one, requests must name a loopback host.
type RouterOptions struct {
	ListenAddress string
	Controllers   []Controller
	Health        *health.Health
	Emitter       wideevent.Emitter
	Metrics       prometheus.Registerer
	Traces        trace.TracerProvider
	UI            http.Handler
	Namespace     string
	Version       string
}

// NewRouter builds the router; it fails without one of its options or when the request metrics
// are registered already.
func NewRouter(options *RouterOptions) (*gin.Engine, error) {
	if options == nil || options.ListenAddress == "" || len(options.Controllers) == 0 || options.Health == nil || options.Emitter == nil || options.Metrics == nil ||
		options.Traces == nil || options.UI == nil || options.Namespace == "" || options.Version == "" {
		return nil, errors.New("server: RouterOptions needs the ListenAddress, the Controllers, the Health, the Emitter, the Metrics, the Traces, the UI, the Namespace and the Version")
	}
	middleware, err := newMiddleware(options)
	if err != nil {
		return nil, err
	}
	router := gin.New()
	// no proxy is trusted: a client cannot choose the address it is recorded with
	if err := router.SetTrustedProxies(nil); err != nil {
		return nil, fmt.Errorf("server: trusted proxies: %w", err)
	}
	router.Use(middleware...)
	telemetryginadapter.RegisterHealthRoutes(router, options.Health)
	api := router.Group(APIBasePath)
	for _, mounted := range options.Controllers {
		mounted.RegisterRoutes(api)
	}
	router.NoRoute(notFoundOr(options.UI)) // last (API-010)
	return router, nil
}

// newMiddleware is the chain every request runs: the span outside the recovery, so it sees the
// 500 of a panic (TRC-005), then the wide event, the request metrics, the guard and the body
// limit.
func newMiddleware(options *RouterOptions) ([]gin.HandlerFunc, error) {
	spans := otelgin.Middleware(options.Namespace,
		otelgin.WithTracerProvider(options.Traces),
		otelgin.WithPropagators(propagation.TraceContext{}),
		otelgin.WithGinFilter(func(ginContext *gin.Context) bool { return !slices.Contains(probes, ginContext.FullPath()) }),
	)
	version := options.Version
	wideEvents, err := wideeventginadapter.NewMiddleware(&wideeventginadapter.Options{
		Emitter:   options.Emitter,
		SkipPaths: probes,
		PreEnricher: func(we *wideevent.Event, ginContext *gin.Context) {
			tracing.RecordTraceID(ginContext.Request.Context(), we) // the span is in the context: otelgin runs first
			we.String("version", version)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("server: wide events: %w", err)
	}
	requestMetrics, err := telemetryginadapter.NewMetricsMiddleware(&telemetryginadapter.MetricsOptions{
		Registerer: options.Metrics, Namespace: options.Namespace, SkipRoutes: probes,
	})
	if err != nil {
		return nil, fmt.Errorf("server: request metrics: %w", err)
	}
	host, _, err := net.SplitHostPort(options.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("server: listen address: %w", err)
	}
	return []gin.HandlerFunc{spans, recovery(), wideEvents, requestMetrics, guard(isLoopback(host)), limitBody(MaximumBodyBytes)}, nil
}

// recovery answers a panic with the 500 error body and prints nothing, so no header reaches a
// log (LOG-009); the wide-event middleware inside it recorded the panic. A handler that aborts
// with http.ErrAbortHandler panics on, so net/http drops the connection as the handler meant.
func recovery() gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if err, isError := recovered.(error); isError && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}
			if !ginContext.Writer.Written() {
				controller.RespondError(ginContext, fmt.Errorf("panic: %v", recovered))
			}
			ginContext.Abort()
		}()
		ginContext.Next()
	}
}

// guard refuses what a page of another site could make a browser send: the API has no
// authentication and its tools run programs on this computer. With loopbackOnly a request must
// name a loopback host, so a DNS name rebound to 127.0.0.1 reaches nothing. An API request with an
// Origin must come from the server's own pages or the dev server's; one with a body must be JSON,
// which a cross-site form cannot send without asking first (CORS preflight).
func guard(loopbackOnly bool) gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		request := ginContext.Request
		if loopbackOnly && !isLoopbackHost(request.Host) {
			controller.RespondError(ginContext, fmt.Errorf("%w: host %q is not a loopback name", model.ErrForbidden, request.Host))
			return
		}
		if !isAPIPath(request.URL.Path) {
			ginContext.Next()
			return
		}
		if origin := request.Header.Get("Origin"); origin != "" && !ownOrigin(origin, request.Host) && !slices.Contains(developmentOrigins, origin) {
			controller.RespondError(ginContext, fmt.Errorf("%w: origin %q", model.ErrForbidden, origin))
			return
		}
		if carriesBody(request.Method) {
			mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				controller.RespondError(ginContext, fmt.Errorf("%w: content type %q", model.ErrUnsupportedMediaType, request.Header.Get("Content-Type")))
				return
			}
		}
		ginContext.Next()
	}
}

// ownOrigin says whether origin is a page of the host the request was sent to.
func ownOrigin(origin, host string) bool {
	parsed, err := url.Parse(origin)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host == host
}

func carriesBody(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
}

// isLoopbackHost says whether a Host header, with or without a port, names this computer.
func isLoopbackHost(host string) bool {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	return isLoopback(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
}

// isLoopback says whether a host name or address is this computer's loopback; an empty one (all
// interfaces) is not.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func limitBody(maximum int64) gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		if ginContext.Request.Body != nil {
			ginContext.Request.Body = http.MaxBytesReader(ginContext.Writer, ginContext.Request.Body, maximum)
		}
		ginContext.Next()
	}
}

// notFoundOr answers a request no route took: the API's 404 error body below the API's prefix,
// the frontend otherwise, which opens a deep link with index.html.
func notFoundOr(ui http.Handler) gin.HandlerFunc {
	return func(ginContext *gin.Context) {
		if isAPIPath(ginContext.Request.URL.Path) {
			controller.RespondError(ginContext, model.ErrNotFound)
			return
		}
		ginContext.Status(http.StatusOK) // gin presets 404 for an unrouted request; the frontend's own status replaces this one
		ui.ServeHTTP(ginContext.Writer, ginContext.Request)
	}
}

func isAPIPath(path string) bool {
	return path == apiPrefix || strings.HasPrefix(path, apiPrefix+"/")
}
