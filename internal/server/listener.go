package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/govengers/telemetry/health"
)

// The timeouts of the API's server (LAY-006). The answer's stream moves its own write deadline
// (controller.ChatController).
const (
	readHeaderTimeout  = 10 * time.Second
	readTimeout        = 30 * time.Second
	writeTimeout       = 60 * time.Second
	idleTimeout        = 120 * time.Second
	maximumHeaderBytes = 64 << 10
)

// defaultShutdownBudget is the drain's time: below a 30 s termination grace period (LAY-003).
const defaultShutdownBudget = 20 * time.Second

// NewHTTPServer is the API's server on address: every phase of a connection is bounded.
func NewHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler, MaxHeaderBytes: maximumHeaderBytes,
		ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, WriteTimeout: writeTimeout, IdleTimeout: idleTimeout,
	}
}

// ListenerOptions configure a Listener. HTTPServer is what it serves, on the server's address;
// it is required. Health is the readiness it withdraws before the drain, nil for a server no
// load balancer reaches; ReadinessDelay is how long the load balancer then gets to notice.
// ShutdownBudget defaults to 20 s.
type ListenerOptions struct {
	HTTPServer     *http.Server
	Health         *health.Health
	ReadinessDelay time.Duration
	ShutdownBudget time.Duration
}

// Listener serves one HTTP server until shutdown and drains it.
type Listener struct {
	options ListenerOptions
}

// NewListener fails without the HTTPServer or with a negative duration.
func NewListener(options *ListenerOptions) (*Listener, error) {
	if options == nil || options.HTTPServer == nil {
		return nil, errors.New("server: ListenerOptions needs the HTTPServer")
	}
	resolved := *options
	if resolved.ShutdownBudget == 0 {
		resolved.ShutdownBudget = defaultShutdownBudget
	}
	if resolved.ShutdownBudget < 0 || resolved.ReadinessDelay < 0 {
		return nil, fmt.Errorf("server: ShutdownBudget %s and ReadinessDelay %s must not be negative", resolved.ShutdownBudget, resolved.ReadinessDelay)
	}
	return &Listener{options: resolved}, nil
}

// Run serves until ctx ends, then withdraws the readiness, waits for the load balancer and
// drains the requests in flight; it returns after the drain (LAY-003). A server that cannot
// listen fails at once.
func (l *Listener) Run(ctx context.Context) error {
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- Recovering(l.options.HTTPServer.ListenAndServe)() }()
	select {
	case err := <-serveErrors:
		return fmt.Errorf("server: listen on %s: %w", l.options.HTTPServer.Addr, err)
	case <-ctx.Done():
	}
	drainContext := context.WithoutCancel(ctx)
	if l.options.Health != nil {
		l.options.Health.SetDraining()
		wait(drainContext, l.options.ReadinessDelay)
	}
	shutdownContext, cancel := context.WithTimeout(drainContext, l.options.ShutdownBudget)
	defer cancel()
	if err := l.options.HTTPServer.Shutdown(shutdownContext); err != nil {
		return errors.Join(fmt.Errorf("server: drain of %s: %w", l.options.HTTPServer.Addr, err), l.options.HTTPServer.Close())
	}
	return nil
}

func wait(ctx context.Context, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// Recovering turns a panic of work into its error, so a goroutine reports it instead of ending
// the process (CON-001).
func Recovering(work func() error) func() error {
	return func() (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("panic: %v\n%s", recovered, debug.Stack())
			}
		}()
		return work()
	}
}
