//go:build e2e

// Package e2e drives the built server through its public API (TST-011): the binary named by
// E2E_BINARY, started on free ports with an explicit environment, against a scripted model
// server and a workspace of its own.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	startupTimeout  = 15 * time.Second
	shutdownTimeout = 15 * time.Second
	pollInterval    = 20 * time.Millisecond
)

// application is one running server.
type application struct {
	baseURL    string
	metricsURL string
	workspace  string
	root       string
	client     *http.Client
}

// start runs the binary against the model server at modelBaseURL and waits until it serves. The
// test's cleanup stops it with SIGTERM and requires a clean exit: a server built with -cover
// writes its coverage only then.
func start(t *testing.T, modelBaseURL string) *application {
	t.Helper()
	binary := os.Getenv("E2E_BINARY")
	require.NotEmpty(t, binary, "E2E_BINARY names the server to test (just test-e2e)")
	started := &application{
		baseURL: "http://" + freeAddress(t), metricsURL: "http://" + freeAddress(t), workspace: t.TempDir(), root: t.TempDir(),
		client: &http.Client{Timeout: 30 * time.Second},
	}
	t.Cleanup(started.client.CloseIdleConnections)
	// not the test's context: it ends before the cleanup, which stops the server itself and waits for it
	command := exec.CommandContext(context.WithoutCancel(t.Context()), binary) //nolint:gosec // G702: the harness runs the binary it was built to test (E2E_BINARY)
	command.Dir = t.TempDir()                                                  // no .env of a checkout is read: the environment below is all there is (CFG-005)
	command.Env = []string{
		"LISTEN_ADDRESS=" + strings.TrimPrefix(started.baseURL, "http://"),
		"METRICS_ADDRESS=" + strings.TrimPrefix(started.metricsURL, "http://"),
		"MODEL_BASE_URL=" + modelBaseURL,
		"WORKSPACE_PATH=" + started.workspace,
		"WORKSPACE_ROOTS=" + started.root,
		"READINESS_DELAY=0s",
		"MODEL_WARM_UP=false", // the scripted server answers in order: a warm-up would take a test's first answer
		"GOCOVERDIR=" + os.Getenv("GOCOVERDIR"),
	}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	require.NoError(t, command.Start())
	t.Cleanup(func() { stop(t, command, &output) })
	require.Eventually(t, func() bool {
		status, _, err := started.get(t, "/api/health")
		return err == nil && status == http.StatusOK
	}, startupTimeout, pollInterval, "the server did not start:\n%s", &output)
	return started
}

func stop(t *testing.T, command *exec.Cmd, output *bytes.Buffer) {
	t.Helper()
	require.NoError(t, command.Process.Signal(syscall.SIGTERM))
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	select {
	case err := <-exited:
		require.NoError(t, err, "the server did not exit cleanly on SIGTERM:\n%s", output)
	case <-time.After(shutdownTimeout):
		require.NoError(t, command.Process.Kill())
		require.Fail(t, "the server did not exit on SIGTERM", "%s", output)
	}
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

// get requests path of the API's listener.
func (a *application) get(t *testing.T, path string) (int, string, error) {
	t.Helper()
	return a.do(t, http.MethodGet, a.baseURL+path, "")
}

// post sends body to path of the API's listener and requires an answer.
func (a *application) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	status, answer, err := a.do(t, http.MethodPost, a.baseURL+path, body)
	require.NoError(t, err)
	return status, answer
}

func (a *application) do(t *testing.T, method, url, body string) (int, string, error) {
	t.Helper()
	header := http.Header{}
	if body != "" {
		header.Set("Content-Type", "application/json")
	}
	return a.send(t, method, url, body, header)
}

// send sends a request with exactly these headers.
func (a *application) send(t *testing.T, method, url, body string, header http.Header) (int, string, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	require.NoError(t, err)
	request.Header = header
	response, err := a.client.Do(request)
	if err != nil {
		return 0, "", err
	}
	answer, err := io.ReadAll(response.Body)
	return response.StatusCode, string(answer), errors.Join(err, response.Body.Close())
}
