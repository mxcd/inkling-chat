package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mxcd/inkling-chat/internal/model"
)

// responseHeaderTimeout bounds how long a model call waits for its response's headers, as
// aikido's own default client does: far above a legitimate wait, it only ends a hung connection.
const responseHeaderTimeout = 10 * time.Minute

// timingsLog collects what the model server measured of the calls of one turn, in the order the
// calls were answered. A turn's calls run one after another, so the n-th usage event belongs to
// the n-th answered call. Safe for concurrent use: the transport writes, the translator reads.
type timingsLog struct {
	mutex   sync.Mutex
	calls   []*model.ServerTimings // nil for an answered call that sent no timings (yet)
	claimed int
}

type timingsLogKey struct{}

func withTimingsLog(ctx context.Context, log *timingsLog) context.Context {
	return context.WithValue(ctx, timingsLogKey{}, log)
}

func timingsLogFrom(ctx context.Context) *timingsLog {
	log, _ := ctx.Value(timingsLogKey{}).(*timingsLog)
	return log
}

// answered opens the entry of a call whose server answered; set fills it.
func (l *timingsLog) answered() int {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	l.calls = append(l.calls, nil)
	return len(l.calls) - 1
}

func (l *timingsLog) set(call int, timings *model.ServerTimings) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	l.calls[call] = timings
}

// next is the timings of the next call a usage event reports, nil when its server sent none.
func (l *timingsLog) next() *model.ServerTimings {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.claimed >= len(l.calls) {
		return nil
	}
	l.claimed++
	return l.calls[l.claimed-1]
}

// timingsTransport reads the llama.cpp-style "timings" object a model server (Strata,
// llama.cpp) sends in the last chunk of a chat completion stream, which aikido's client does not
// read, and keeps it in the timingsLog of the request's context. The body reaches aikido
// unchanged.
type timingsTransport struct {
	base http.RoundTripper
}

func (t *timingsTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	log := timingsLogFrom(request.Context())
	if err != nil || log == nil || response.StatusCode != http.StatusOK || !strings.HasSuffix(request.URL.Path, "/chat/completions") {
		return response, err //nolint:wrapcheck // a RoundTripper passes its base's error on as it is
	}
	response.Body = &timingsBody{ReadCloser: response.Body, log: log, call: log.answered()}
	return response, nil
}

// timingsBody passes a stream's bytes on and reads its server-sent events line by line on the way.
type timingsBody struct {
	io.ReadCloser
	log     *timingsLog
	call    int
	partial []byte // the start of a line whose end has not arrived yet
}

func (b *timingsBody) Read(buffer []byte) (int, error) {
	read, err := b.ReadCloser.Read(buffer)
	data := buffer[:read]
	for {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			break
		}
		b.line(append(b.partial, data[:end]...))
		b.partial, data = b.partial[:0], data[end+1:]
	}
	b.partial = append(b.partial, data...)
	return read, err //nolint:wrapcheck // a Reader passes io.EOF on as it is
}

// line keeps the timings of a data line that carries them.
func (b *timingsBody) line(line []byte) {
	payload, found := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !found || !bytes.Contains(payload, []byte(`"timings"`)) {
		return
	}
	var chunk struct {
		Timings *struct {
			PromptN            int     `json:"prompt_n"`
			PromptMs           float64 `json:"prompt_ms"`
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
			DraftN             int     `json:"draft_n"`
			DraftNAccepted     int     `json:"draft_n_accepted"`
		} `json:"timings"`
	}
	if json.Unmarshal(payload, &chunk) != nil || chunk.Timings == nil {
		return // a text that names "timings" is no timings object; aikido reads the chunk itself
	}
	measured := chunk.Timings
	b.log.set(b.call, &model.ServerTimings{
		PromptProcessing: time.Duration(measured.PromptMs * float64(time.Millisecond)), PromptTokens: measured.PromptN,
		PromptTokensPerSecond: measured.PromptPerSecond, GenerationTokensPerSecond: measured.PredictedPerSecond,
		DraftTokens: measured.DraftN, DraftAccepted: measured.DraftNAccepted,
	})
}

// newHTTPClient is the model client's HTTP client: aikido's default, which bounds the wait for
// headers, with the timings transport in front.
func newHTTPClient() *http.Client {
	base := http.DefaultTransport
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := transport.Clone()
		cloned.ResponseHeaderTimeout = responseHeaderTimeout
		base = cloned
	}
	return &http.Client{Transport: &timingsTransport{base: base}}
}
