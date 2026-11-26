// Package service holds the chat's rules: what a message must be and what a turn of the agent
// is. It knows no transport (ARC-003).
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"
	wideevent "github.com/govengers/wide-event-logger"

	"github.com/mxcd/inkling-chat/internal/model"
)

// defaultMaximumMessageBytes bounds one message: about the model's whole context of 1M tokens at
// four bytes a token, so a pasted document fits; the model server refuses what its context cannot
// hold.
const defaultMaximumMessageBytes = 4 << 20

// DefaultThinking is the thinking of a message that names none, and of the warm-up, so the
// prefix the model server pins is the one of the common turn.
const DefaultThinking = model.ThinkingOff

// WorkspacePrompt is added for a turn with tools. It names no directory, so the system prompt,
// which the model server caches, is the same for every directory: each message names it.
const WorkspacePrompt = `You have tools that work in one directory on the user's computer, the working directory each message names: the file tools list_files, read_file, write_file, edit_file, multi_edit, move_file, delete_file, grep and glob, and bash, a virtual shell over the same directory with only the commands its description lists. Use them when the user asks you to create, change, find, read or check files. Read a file before you change it, and prefer edit_file or multi_edit over rewriting a whole file; use bash for what the file tools cannot do. A bash call stops after a short time unless it sets timeout_seconds; start servers and watchers with start_process and check them with process_output, never in bash. Stop the processes you started when you are done. When you only answer a question, do not use tools.`

// commandPrompts are added to WorkspacePrompt for the host commands the shell has, by name.
var commandPrompts = map[string]string{
	"go":  `The shell has the host's go command, which runs in the shell's current directory. After you change Go code, run go build ./..., go vet ./... and go test ./... and fix what they report before you say you are done.`,
	"bun": `The shell has the host's bun command for JavaScript and TypeScript, which runs in the shell's current directory: bun install, bun run, bun test, bun x. Check your changes with bun test or the project's scripts.`,
}

// directoryLine starts the text of a message with tools: the directory its tools work in.
const directoryLine = "Working directory: %s\n\n%s"

// TurnRunner runs one turn of the agent and streams its events, which end with a
// model.ChatEventEnd (agent.Runtime in main, a scripted one in tests). Warm sends only the start
// of a turn, its system prompt and tools, so the model server caches it.
type TurnRunner interface {
	Converse(ctx context.Context, turn model.ChatTurn) (<-chan model.ChatEvent, error)
	Warm(ctx context.Context, turn model.ChatTurn) error
}

// ModelDirectory names the model the model server serves and its context window (agent.Directory
// in main).
type ModelDirectory interface {
	ServedModel(ctx context.Context) (model.ServedModel, error)
}

// ChatServiceOptions configure a ChatService. TurnRunner is required. Model is the name the
// model server is asked for; left empty, every turn asks ModelDirectory, which is required then,
// for the model the server serves, so a server restarted with another model keeps working.
// Workspace is the directory of a turn with tools that names none; WorkspaceRoots are the
// directories a message's directory must lie in, none allowing none. Both are absolute; their
// symbolic links are resolved. Commands name the host commands the turns' shell has (go, bun),
// which the prompt then names. MaximumMessageBytes defaults to 4 MiB.
type ChatServiceOptions struct {
	TurnRunner          TurnRunner
	Model               string
	ModelDirectory      ModelDirectory
	Workspace           string
	WorkspaceRoots      []string
	Commands            []string
	MaximumMessageBytes int
}

// ChatService answers the messages of conversations.
type ChatService struct {
	options ChatServiceOptions
}

// NewChatService fails without the TurnRunner, without both the Model and the ModelDirectory,
// with a negative limit, or with a workspace or root that is no absolute path of a directory.
func NewChatService(options *ChatServiceOptions) (*ChatService, error) {
	if options == nil || options.TurnRunner == nil {
		return nil, errors.New("chat service: TurnRunner is required")
	}
	if options.Model == "" && options.ModelDirectory == nil {
		return nil, errors.New("chat service: Model or ModelDirectory is required")
	}
	resolved := *options
	if resolved.MaximumMessageBytes == 0 {
		resolved.MaximumMessageBytes = defaultMaximumMessageBytes
	}
	if resolved.MaximumMessageBytes < 0 {
		return nil, fmt.Errorf("chat service: MaximumMessageBytes %d is negative", resolved.MaximumMessageBytes)
	}
	resolved.WorkspaceRoots = make([]string, 0, len(options.WorkspaceRoots))
	for _, root := range options.WorkspaceRoots {
		directory, err := existingDirectory(root)
		if err != nil {
			return nil, fmt.Errorf("chat service: workspace root: %w", err)
		}
		resolved.WorkspaceRoots = append(resolved.WorkspaceRoots, directory)
	}
	if resolved.Workspace != "" {
		directory, err := existingDirectory(resolved.Workspace)
		if err != nil {
			return nil, fmt.Errorf("chat service: workspace: %w", err)
		}
		resolved.Workspace = directory
	}
	return &ChatService{options: resolved}, nil
}

// SendMessageInput is one message of a conversation. The conversation is created by its first
// message. Workspace lets the model use the tools of Directory for this message (the default
// workspace when empty); Thinking is how long it reasons first, DefaultThinking when empty;
// Persona is the ID of its system prompt, DefaultPersona when empty.
type SendMessageInput struct {
	ConversationID uuid.UUID
	Text           string
	Workspace      bool
	Directory      string
	Thinking       model.Thinking
	Persona        string
}

// SendMessage runs the agent's turn for a message and hands every event of it to deliver, in
// order, the end last. Before anything was delivered it returns model.ErrInvalid for a message
// it refuses and model.ErrUnavailable for a model server that names no model; after the end of
// a turn that failed it returns the turn's error (model.ErrUnavailable, model.ErrRejected). A
// turn its caller cancelled is no error.
func (s *ChatService) SendMessage(ctx context.Context, input SendMessageInput, deliver func(model.ChatEvent)) error {
	we := wideevent.FromContext(ctx)
	we.String("action", "chat.send_message").UUID("chat.conversation_id", input.ConversationID).
		Int("chat.message_bytes", len(input.Text)).Bool("chat.workspace", input.Workspace)
	if input.Thinking == "" {
		input.Thinking = DefaultThinking
	}
	if input.Persona == "" {
		input.Persona = DefaultPersona
	}
	we.String("chat.thinking", string(input.Thinking)).String("chat.persona", input.Persona)
	if err := s.validate(input); err != nil {
		return err
	}
	chosen, known := persona(input.Persona)
	if !known {
		return &model.InvalidInputError{Message: fmt.Sprintf("There is no persona %q.", input.Persona)}
	}
	turn := model.ChatTurn{
		ConversationID: input.ConversationID, Text: input.Text, System: s.systemPrompt(chosen, input.Workspace), Workspace: input.Workspace, Thinking: input.Thinking,
	}
	if input.Workspace {
		directory, err := s.directory(input.Directory)
		if err != nil {
			return err
		}
		we.String("chat.directory", directory)
		turn.Directory = directory
		if directory != "" {
			turn.Text = fmt.Sprintf(directoryLine, directory, input.Text)
		}
	}
	served, err := s.servedModel(ctx)
	if err != nil {
		return fmt.Errorf("chat service: model of conversation %s: %w", input.ConversationID, err)
	}
	we.String("chat.model", served.Name)
	turn.Model = served.Name
	events, err := s.options.TurnRunner.Converse(ctx, turn)
	if err != nil {
		return fmt.Errorf("chat service: turn of conversation %s: %w", input.ConversationID, err)
	}
	summary := turnSummary{}
	for event := range events {
		summary.count(event)
		if event.Usage != nil {
			event.Usage.ContextLength = served.ContextLength
		}
		deliver(event)
	}
	summary.record(we)
	return summary.err
}

// WarmUp has the model server read the start of every turn with tools of the default persona
// (the system prompt, the tool definitions and DefaultThinking) once, so a server that caches
// prompt prefixes answers the first such turn from its cache, in any directory. Nothing depends
// on it: a caller logs its error.
func (s *ChatService) WarmUp(ctx context.Context) error {
	served, err := s.servedModel(ctx)
	if err != nil {
		return fmt.Errorf("chat service: model of the warm-up: %w", err)
	}
	wideevent.FromContext(ctx).String("chat.model", served.Name)
	turn := model.ChatTurn{Model: served.Name, System: s.systemPrompt(personas[0], true), Workspace: true, Directory: s.options.Workspace, Thinking: DefaultThinking}
	if err := s.options.TurnRunner.Warm(ctx, turn); err != nil {
		return fmt.Errorf("chat service: warm-up: %w", err)
	}
	return nil
}

// servedModel is the model a turn asks for: the configured one, whose context window is unknown,
// or the one the model server serves.
func (s *ChatService) servedModel(ctx context.Context) (model.ServedModel, error) {
	if s.options.Model != "" {
		return model.ServedModel{Name: s.options.Model}, nil
	}
	return s.options.ModelDirectory.ServedModel(ctx)
}

// systemPrompt is the persona's prompt and, for a turn with tools, what the tools are.
func (s *ChatService) systemPrompt(chosen model.Persona, workspace bool) string {
	prompt := personaPrompt(chosen)
	if !workspace {
		return prompt
	}
	prompt += "\n\n" + WorkspacePrompt
	for _, command := range s.options.Commands {
		if text, known := commandPrompts[command]; known {
			prompt += " " + text
		}
	}
	return prompt
}

// directory is the directory a message's tools work in: the default workspace for none, else
// requested with its symbolic links resolved, which must be an existing directory in one of the
// roots. A refusal says the user what is wrong.
func (s *ChatService) directory(requested string) (string, error) {
	if requested == "" {
		return s.options.Workspace, nil
	}
	resolved, err := existingDirectory(requested)
	if err != nil {
		return "", &model.InvalidInputError{Message: "Cannot use the directory: " + err.Error() + "."}
	}
	if !slices.ContainsFunc(s.options.WorkspaceRoots, func(root string) bool { return within(root, resolved) }) {
		return "", &model.InvalidInputError{Message: fmt.Sprintf("The directory %s is outside the folders the chat may work in (%s). WORKSPACE_ROOTS names them.",
			resolved, strings.Join(s.options.WorkspaceRoots, ", "))}
	}
	return resolved, nil
}

// existingDirectory is path, which must be absolute, cleaned and with its symbolic links
// resolved; it fails for a path that is not an existing directory. Its errors are written for a
// user.
func existingDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%q is not an absolute path; write it from the root, like /Users/you/project", path)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("%s does not exist", path)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return resolved, nil
}

// within says whether path is root or lies below it; both are cleaned absolute paths.
func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (s *ChatService) validate(input SendMessageInput) error {
	switch {
	case input.ConversationID == uuid.Nil:
		return fmt.Errorf("%w: the conversation id is missing", model.ErrInvalid)
	case strings.TrimSpace(input.Text) == "":
		return fmt.Errorf("%w: the message is empty", model.ErrInvalid)
	case len(input.Text) > s.options.MaximumMessageBytes:
		return fmt.Errorf("%w: the message has %d bytes, more than %d", model.ErrTooLarge, len(input.Text), s.options.MaximumMessageBytes)
	case !input.Thinking.Valid():
		return fmt.Errorf("%w: thinking %q is not off, low, medium or high", model.ErrInvalid, input.Thinking)
	default:
		return nil
	}
}

// turnSummary is what a turn did, counted while its events pass and recorded once at its end
// (LOG-006).
type turnSummary struct {
	toolCalls       int
	failedToolCalls int
	textBytes       int
	reason          model.ChatEndReason
	err             error
}

func (t *turnSummary) count(event model.ChatEvent) {
	switch event.Kind {
	case model.ChatEventToolCall:
		t.toolCalls++
	case model.ChatEventToolResult:
		if !event.OK {
			t.failedToolCalls++
		}
	case model.ChatEventText:
		t.textBytes += len(event.Text)
	case model.ChatEventEnd:
		t.reason = event.Reason
		t.err = event.Err
	case model.ChatEventThinking, model.ChatEventUsage:
		// the model client's observers record the tokens and times of every call (LOG-004)
	}
}

func (t *turnSummary) record(we *wideevent.Event) {
	we.String("chat.end_reason", string(t.reason)).
		Int("chat.tool_calls", t.toolCalls).
		Int("chat.failed_tool_calls", t.failedToolCalls).
		Int("chat.answer_bytes", t.textBytes)
}
