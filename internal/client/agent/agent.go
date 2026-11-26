package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/govengers/aikido/agent"
	"github.com/govengers/aikido/llm"
	"github.com/govengers/aikido/tools"

	"github.com/mxcd/inkling-chat/internal/model"
)

// The bounds of one turn: its model calls, its whole time and the size of one tool answer the
// model reads. Agentic work (read, edit, go build, go test, fix, test again) takes dozens of
// calls. The time allows a prompt that fills the model's 1M-token context: the local server
// reads one in about 80 minutes (extrapolated; 708 tokens a second at 232K tokens), a follow-up
// only its new tokens.
const (
	maximumTurns       = 60
	runTimeout         = 2 * time.Hour
	maximumResultBytes = 32 << 10
	// maximumTargetRunes bounds the target a tool step shows: a shell command's first line, cut.
	maximumTargetRunes = 80
)

// warmUpText is the user message of a warm-up. A server's prompt cache keeps what comes before
// it: the system prompt and the tool definitions.
const warmUpText = "Hello."

// Options configure a Runtime. Client is the model client, History and Locker keep the
// transcripts and serialize a conversation's turns; all three are required. Commands are the host
// programs the shell offers (FindGo, FindBun). Timeouts bound the tool calls; zero fields take
// DefaultToolTimeout and DefaultMaximumToolTimeout. PromptCaching marks the requests with
// prompt-cache breakpoints, for a provider that caches only what is marked (Anthropic's models on
// OpenRouter); an OpenAI-compatible server caches prefixes on its own and does not know the marks.
// Temperature, when set, is sent with every model call.
type Options struct {
	Client        llm.Client
	History       agent.History
	Locker        agent.Locker
	Commands      []HostCommand
	Timeouts      Timeouts
	PromptCaching bool
	// Temperature is the sampling temperature of every model call; nil leaves the model server's
	// default, which on Strata is greedy decoding.
	Temperature *float32
}

// Runtime runs the chat's turns. A turn with tools gets the file tools, the shell and the host
// commands of its directory, kept per conversation. It is safe for concurrent use; Close it.
type Runtime struct {
	options    Options
	workspaces *workspaces
}

// New fails without its dependencies.
func New(options *Options) (*Runtime, error) {
	if options == nil || options.Client == nil || options.History == nil || options.Locker == nil {
		return nil, errors.New("agent: Options needs the Client, the History and the Locker")
	}
	resolved := *options
	if resolved.Timeouts.Default <= 0 {
		resolved.Timeouts.Default = DefaultToolTimeout
	}
	if resolved.Timeouts.Maximum <= 0 {
		resolved.Timeouts.Maximum = DefaultMaximumToolTimeout
	}
	resolved.Timeouts.Maximum = max(resolved.Timeouts.Maximum, resolved.Timeouts.Default)
	return &Runtime{options: resolved, workspaces: newWorkspaces(slices.Clone(options.Commands), resolved.Timeouts)}, nil
}

// Close releases the directories of the kept tools.
func (r *Runtime) Close() error {
	return r.workspaces.close()
}

// Converse runs one turn and streams its events; the channel ends with a ChatEventEnd and
// closes. Read it to its end: a reader that stops must cancel ctx.
func (r *Runtime) Converse(ctx context.Context, turn model.ChatTurn) (<-chan model.ChatEvent, error) {
	offered := tools.NewRegistry()
	if turn.Workspace {
		var err error
		if offered, err = r.workspaces.of(turn.ConversationID, turn.Directory); err != nil {
			return nil, err
		}
	}
	session, err := agent.NewSession(&agent.SessionOptions{
		ID: turn.ConversationID.String(), Client: r.options.Client, Tools: offered, History: r.options.History, Locker: r.options.Locker,
		// one model call may take as long as the turn: aikido's 180 s default would cut a long prompt read
		Model: turn.Model, SystemPrompt: turn.System, MaximumTurns: maximumTurns, RunTimeout: runTimeout, LLMCallTimeout: runTimeout, MaximumToolResultBytes: maximumResultBytes,
		ToolCallTimeout:      r.options.Timeouts.Maximum + sessionToolGrace,
		DisablePromptCaching: !r.options.PromptCaching, Temperature: r.options.Temperature, Thinking: thinking(turn.Thinking),
	})
	if err != nil {
		return nil, fmt.Errorf("agent: session of conversation %s: %w", turn.ConversationID, err)
	}
	measured := &timingsLog{}
	events, err := session.Run(withTimingsLog(ctx, measured), turn.Text)
	if err != nil {
		return nil, fmt.Errorf("agent: run of conversation %s: %w", turn.ConversationID, err)
	}
	translated := make(chan model.ChatEvent)
	go forward(ctx, events, translated, measured)
	return translated, nil
}

// Warm has the model server read the start of a turn like turn, its system prompt and the tools
// it is offered and its thinking, in the request a turn sends, asking for one token: a server that
// caches prompt prefixes then answers the first such turn from its cache. The tool definitions do
// not depend on the directory, so one warm-up serves every directory. turn's conversation and
// text are not used; nothing is recorded in a history.
func (r *Runtime) Warm(ctx context.Context, turn model.ChatTurn) (err error) {
	var definitions []llm.ToolDefinition
	if turn.Workspace {
		built, buildErr := newWorkspace(turn.Directory, r.workspaces.commands, r.options.Timeouts)
		if buildErr != nil {
			return fmt.Errorf("agent: warm-up: %w", buildErr)
		}
		defer func() { err = errors.Join(err, built.release()) }()
		definitions = built.tools.Definitions()
	}
	_, err = llm.Collect(ctx, r.options.Client, llm.Request{
		Model:         turn.Model,
		Messages:      []llm.Message{{Role: llm.RoleSystem, Content: turn.System}, {Role: llm.RoleUser, Content: warmUpText}},
		Tools:         definitions,
		Thinking:      thinking(turn.Thinking),
		MaximumTokens: 1,
	})
	if err != nil {
		return fmt.Errorf("agent: warm-up: %w", classify(err))
	}
	return nil
}

// thinking is a turn's thinking as the model server's effort. Off is the effort "none", sent
// explicitly: a server's default may be another; an empty level leaves the server's default.
func thinking(level model.Thinking) *llm.ThinkingConfiguration {
	switch level {
	case "":
		return nil
	case model.ThinkingOff:
		return llm.ThinkingByEffort("none")
	default:
		return llm.ThinkingByEffort(llm.ThinkingEffort(level))
	}
}

// forward hands aikido's events on as the service reads them. It reads events to their end
// whatever happens, so aikido's run never blocks on it, and stops sending once ctx ended, so a
// reader that left does not hold it (CON-005). The translator recovers, so no event can panic
// it (CON-001).
func forward(ctx context.Context, events <-chan agent.Event, translated chan<- model.ChatEvent, measured *timingsLog) {
	defer close(translated)
	turn := translator{measured: measured}
	for event := range events {
		converted, relevant := turn.translate(event)
		if !relevant {
			continue
		}
		select {
		case translated <- converted:
		case <-ctx.Done():
		}
	}
}

// translator turns the events of one run into the model's. aikido reports a failed run as an
// error event followed by the end; the model's one end event carries both. measured holds what
// the server measured of the run's calls, nil for none.
type translator struct {
	failure  error
	measured *timingsLog
}

// translate is read with a recovery: an event it cannot read (a call or result without its
// body) fails the turn's end instead of panicking the forwarding goroutine.
func (t *translator) translate(event agent.Event) (converted model.ChatEvent, relevant bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.failure = fmt.Errorf("agent: an event of kind %s: %v", event.Kind, recovered)
			converted, relevant = model.ChatEvent{}, false
		}
	}()
	return t.read(event)
}

// read is an aikido event as the service reads it; images are not (the model writes none).
func (t *translator) read(event agent.Event) (model.ChatEvent, bool) {
	switch event.Kind {
	case agent.EventText:
		return model.ChatEvent{Kind: model.ChatEventText, Text: event.Text}, true
	case agent.EventThinking:
		return model.ChatEvent{Kind: model.ChatEventThinking, Text: event.Text}, true
	case agent.EventUsage:
		used := usage(event.Usage)
		if t.measured != nil {
			used.Server = t.measured.next()
		}
		return model.ChatEvent{Kind: model.ChatEventUsage, Usage: used}, true
	case agent.EventToolCall:
		return model.ChatEvent{Kind: model.ChatEventToolCall, CallID: event.ToolCall.ID, Tool: event.ToolCall.Name, Target: target(event.ToolCall.Arguments)}, true
	case agent.EventToolResult:
		return model.ChatEvent{Kind: model.ChatEventToolResult, CallID: event.ToolResult.CallID, Tool: event.ToolResult.Name, OK: event.ToolResult.OK, Error: event.ToolResult.Error}, true
	case agent.EventError:
		t.failure = classify(event.Err)
		return model.ChatEvent{}, false
	case agent.EventEnd:
		return t.end(event.EndReason), true
	default:
		return model.ChatEvent{}, false
	}
}

// end is the turn's one end event. A turn its caller cancelled is no failure, whatever the
// cancelled model call reported; otherwise a failure the run reported, or one of an event that
// could not be read, makes it a failed end whatever aikido's reason says.
func (t *translator) end(reason agent.EndReason) model.ChatEvent {
	ended := model.ChatEvent{Kind: model.ChatEventEnd, Reason: endReason(reason)}
	switch {
	case ended.Reason == model.ChatEndReasonCancelled:
	case t.failure != nil:
		ended.Reason, ended.Err = model.ChatEndReasonError, t.failure
	case ended.Reason == model.ChatEndReasonTimeout:
		ended.Err = fmt.Errorf("%w: the turn ran out of time", model.ErrUnavailable)
	}
	return ended
}

// usage is a model call's usage as the model names it.
func usage(reported *llm.Usage) *model.ChatUsage {
	return &model.ChatUsage{
		InputTokens: reported.PromptTokens, CachedInputTokens: reported.CacheReadTokens, OutputTokens: reported.CompletionTokens,
		Latency: reported.Latency, Duration: reported.Duration, TokensPerSecond: reported.TokensPerSecond,
	}
}

// target is the file or pattern a file tool's call names, or the shell's command, its first
// line cut to maximumTargetRunes: what a reader wants to see next to the tool's name.
func target(arguments string) string {
	var named struct {
		Path    string   `json:"path"`
		Source  string   `json:"source"`
		Pattern string   `json:"pattern"`
		Command string   `json:"command"`
		Program string   `json:"program"`
		Args    []string `json:"args"`
		ID      int      `json:"id"`
		URL     string   `json:"url"`
	}
	if err := json.Unmarshal([]byte(arguments), &named); err != nil {
		return "" // models can emit invalid JSON; the tool call itself reports that to the model
	}
	for _, name := range []string{named.Path, named.Source, named.Pattern, named.URL} {
		if name != "" {
			return name
		}
	}
	switch {
	case named.Program != "":
		return shortened(strings.Join(append([]string{named.Program}, named.Args...), " "))
	case named.ID > 0:
		return fmt.Sprintf("#%d", named.ID)
	default:
		return shortened(named.Command)
	}
}

// shortened is a command's first line, cut to maximumTargetRunes with an ellipsis when it was
// longer or had more lines.
func shortened(command string) string {
	command = strings.TrimSpace(command)
	line, rest, more := strings.Cut(command, "\n")
	runes := []rune(strings.TrimSpace(line))
	if len(runes) > maximumTargetRunes {
		return string(runes[:maximumTargetRunes-1]) + "…"
	}
	if more && strings.TrimSpace(rest) != "" {
		return string(runes) + " …"
	}
	return string(runes)
}

// classify maps what ended a run to the model's sentinels (ARC-005): a request the server
// refused, or a server that did not answer.
func classify(err error) error {
	if errors.Is(err, llm.ErrInvalidRequest) {
		return fmt.Errorf("%w: %w", model.ErrRejected, err)
	}
	return fmt.Errorf("%w: %w", model.ErrUnavailable, err)
}

// endReason is aikido's end reason as the model names it; aikido's values are stable.
func endReason(reason agent.EndReason) model.ChatEndReason {
	switch reason {
	case agent.EndReasonStop:
		return model.ChatEndReasonStop
	// shortcut: aikido's repeated_tool_calls by its stable value, the constant once a released aikido has it
	case agent.EndReasonMaximumTurns, agent.EndReason("repeated_tool_calls"):
		return model.ChatEndReasonMaximumTurns
	case agent.EndReasonTimeout:
		return model.ChatEndReasonTimeout
	case agent.EndReasonCancelled:
		return model.ChatEndReasonCancelled
	default: // agent.EndReasonError, and a reason a later aikido adds
		return model.ChatEndReasonError
	}
}
