package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/govengers/aikido/tools"
)

// The reach of http_request: servers the model started on this computer, on the ports the tool
// descriptions give it. The user's own apps (this chat, the model server, dev servers) stay below.
const (
	minimumRequestPort    = 20000
	maximumResponseBytes  = 16 << 10
	maximumRequestBodyLen = 64 << 10
)

// httpResponse is what the model reads of a request.
type httpResponse struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Body        string `json:"body"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// registerHTTPRequest adds http_request: one HTTP request to a server on this computer.
func registerHTTPRequest(registry *tools.Registry) error {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	description := fmt.Sprintf("Sends one HTTP request to a server you started on this computer (start_process) and returns the status, content type and body (at most 16 KiB). "+
		"Only http://127.0.0.1 or http://localhost on ports %d to 65535; redirects are not followed.", minimumRequestPort)
	return registry.Register(llmTool("http_request", description, tools.Object(map[string]any{
		"url":    tools.String("The URL, e.g. http://127.0.0.1:20080/health."),
		"method": tools.Enum("The method; GET when left out.", http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead),
		"body":   tools.String("The request body, sent as JSON when it parses as JSON."),
	}, "url")), func(ctx context.Context, arguments json.RawMessage, _ tools.Environment) (tools.Result, error) {
		var parsed struct {
			URL    string `json:"url"`
			Method string `json:"method"`
			Body   string `json:"body"`
		}
		if err := json.Unmarshal(arguments, &parsed); err != nil {
			return tools.Result{}, fmt.Errorf("%w for http_request: %w", tools.ErrInvalidArguments, err)
		}
		response, err := send(ctx, client, parsed.Method, parsed.URL, parsed.Body)
		return tools.Result{Content: response}, err
	})
}

func send(ctx context.Context, client *http.Client, method, address, body string) (httpResponse, error) {
	target, err := localURL(address)
	if err != nil {
		return httpResponse{}, err
	}
	if method == "" {
		method = http.MethodGet
	}
	if len(body) > maximumRequestBodyLen {
		return httpResponse{}, fmt.Errorf("the body has %d bytes, more than %d", len(body), maximumRequestBodyLen)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), strings.NewReader(body))
	if err != nil {
		return httpResponse{}, fmt.Errorf("request: %w", err)
	}
	if body != "" && json.Valid([]byte(body)) {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return httpResponse{}, fmt.Errorf("request to %s: %w", target, err)
	}
	defer response.Body.Close() // read to its limit below: a failed close loses nothing
	read, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBytes+1))
	if err != nil {
		return httpResponse{}, fmt.Errorf("response of %s: %w", target, err)
	}
	truncated := len(read) > maximumResponseBytes
	read = read[:min(len(read), maximumResponseBytes)]
	return httpResponse{Status: response.StatusCode, ContentType: response.Header.Get("Content-Type"), Body: string(bytes.ToValidUTF8(read, []byte("?"))), Truncated: truncated}, nil
}

// localURL is address if it is http to a loopback host on a port from minimumRequestPort.
func localURL(address string) (*url.URL, error) {
	target, err := url.Parse(address)
	if err != nil || target.Scheme != "http" {
		return nil, fmt.Errorf("%q is no http URL", address)
	}
	host := target.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("%q is not on this computer: only 127.0.0.1 and localhost", address)
	}
	port, err := strconv.Atoi(target.Port())
	if err != nil || port < minimumRequestPort || port > 65535 {
		return nil, fmt.Errorf("%q: the port must be from %d to 65535, where your test servers run", address, minimumRequestPort)
	}
	return target, nil
}
