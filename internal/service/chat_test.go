package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	wideevent "github.com/govengers/wide-event-logger"
	"github.com/stretchr/testify/require"

	"github.com/mxcd/inkling-chat/internal/model"
)

// scriptedRunner answers every turn with its events and keeps the turns it was asked for.
type scriptedRunner struct {
	events []model.ChatEvent
	fails  error
	turns  []model.ChatTurn
}

func (r *scriptedRunner) Warm(_ context.Context, turn model.ChatTurn) error {
	r.turns = append(r.turns, turn)
	return r.fails
}

func (r *scriptedRunner) Converse(_ context.Context, turn model.ChatTurn) (<-chan model.ChatEvent, error) {
	r.turns = append(r.turns, turn)
	if r.fails != nil {
		return nil, r.fails
	}
	stream := make(chan model.ChatEvent, len(r.events))
	for _, event := range r.events {
		stream <- event
	}
	close(stream)
	return stream, nil
}

// fixedDirectory serves one model, or fails.
type fixedDirectory struct {
	name          string
	contextLength int
	fails         error
}

func (d fixedDirectory) ServedModel(context.Context) (model.ServedModel, error) {
	return model.ServedModel{Name: d.name, ContextLength: d.contextLength}, d.fails
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

// send sends input and returns what was delivered, the unit's wide event and the error.
func send(t *testing.T, chatService *ChatService, input SendMessageInput) ([]model.ChatEvent, *wideevent.Event, error) {
	t.Helper()
	we := wideevent.Begin(discard{}, "test")
	var delivered []model.ChatEvent
	err := chatService.SendMessage(wideevent.NewContext(t.Context(), we), input, func(event model.ChatEvent) { delivered = append(delivered, event) })
	return delivered, we, err
}

func TestSendMessageDeliversTheTurnsEventsInOrder(t *testing.T) {
	t.Parallel()
	turn := []model.ChatEvent{
		{Kind: model.ChatEventToolCall, CallID: "call-1", Tool: "write_file", Target: "notes.md"},
		{Kind: model.ChatEventToolResult, CallID: "call-1", Tool: "write_file", OK: true},
		{Kind: model.ChatEventToolCall, CallID: "call-2", Tool: "read_file", Target: "gone.md"},
		{Kind: model.ChatEventToolResult, CallID: "call-2", Tool: "read_file", Error: "no such file"},
		{Kind: model.ChatEventText, Text: "Done"},
		{Kind: model.ChatEventText, Text: "."},
		{Kind: model.ChatEventUsage, Usage: &model.ChatUsage{InputTokens: 30, OutputTokens: 4}},
		{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonStop},
	}
	runner := &scriptedRunner{events: turn}
	chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: runner, ModelDirectory: fixedDirectory{name: "served-model", contextLength: 4096}})
	require.NoError(t, err)
	conversation := uuid.New()

	delivered, we, err := send(t, chatService, SendMessageInput{ConversationID: conversation, Text: "Write the notes.", Workspace: true})

	require.NoError(t, err)
	require.Equal(t, turn, delivered)
	require.Equal(t, 4096, delivered[6].Usage.ContextLength, "a usage names the served model's context window")
	require.Len(t, runner.turns, 1)
	require.Equal(t, conversation, runner.turns[0].ConversationID)
	require.Equal(t, "served-model", runner.turns[0].Model, "without a fixed model, the one the server serves is asked for")
	require.True(t, runner.turns[0].Workspace)
	require.Equal(t, DefaultThinking, runner.turns[0].Thinking, "a message that names no thinking gets the default")
	require.Contains(t, runner.turns[0].System, WorkspacePrompt)
	require.Equal(t, "chat.send_message", field(we, "action"))
	require.Equal(t, "served-model", field(we, "chat.model"))
	require.Equal(t, true, field(we, "chat.workspace"))
	require.Equal(t, "off", field(we, "chat.thinking"))
	require.Equal(t, 2, field(we, "chat.tool_calls"))
	require.Equal(t, 1, field(we, "chat.failed_tool_calls"))
	require.Equal(t, len("Done."), field(we, "chat.answer_bytes"))
	require.Equal(t, "stop", field(we, "chat.end_reason"))
}

func TestATurnWithoutTheWorkspaceGetsThePlainPromptAndTheFixedModel(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{events: []model.ChatEvent{{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonStop}}}
	chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: runner, Model: "fixed-model", ModelDirectory: fixedDirectory{fails: errors.New("never asked")}})
	require.NoError(t, err)

	_, we, err := send(t, chatService, SendMessageInput{ConversationID: uuid.New(), Text: "Hello", Directory: "relative/is/not/looked/at"})

	require.NoError(t, err)
	require.Equal(t, "fixed-model", runner.turns[0].Model)
	require.Equal(t, DefaultSystemPrompt, runner.turns[0].System)
	require.False(t, runner.turns[0].Workspace)
	require.Empty(t, runner.turns[0].Directory, "a turn without tools has no directory")
	require.Equal(t, "Hello", runner.turns[0].Text)
	require.Equal(t, DefaultPersona, field(we, "chat.persona"))
}

func TestATurnThinksAsLongAsItsMessageAsks(t *testing.T) {
	t.Parallel()
	for _, level := range []model.Thinking{model.ThinkingOff, model.ThinkingLow, model.ThinkingMedium, model.ThinkingHigh} {
		t.Run(string(level), func(t *testing.T) {
			t.Parallel()
			runner := &scriptedRunner{events: []model.ChatEvent{{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonStop}}}
			chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: runner, Model: "fixed-model"})
			require.NoError(t, err)

			_, we, err := send(t, chatService, SendMessageInput{ConversationID: uuid.New(), Text: "Hello", Thinking: level})

			require.NoError(t, err)
			require.Equal(t, level, runner.turns[0].Thinking)
			require.Equal(t, string(level), field(we, "chat.thinking"))
		})
	}
}

func TestTheWarmUpAsksForTheStartOfATurnWithTheWorkspace(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{}
	workspace := resolvedTempDir(t)
	chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: runner, ModelDirectory: fixedDirectory{name: "served-model"}, Workspace: workspace, Commands: []string{"go"}})
	require.NoError(t, err)
	we := wideevent.Begin(discard{}, "test")

	require.NoError(t, chatService.WarmUp(wideevent.NewContext(t.Context(), we)))

	require.Equal(t, []model.ChatTurn{{
		Model: "served-model", System: DefaultSystemPrompt + "\n\n" + WorkspacePrompt + " " + commandPrompts["go"], Workspace: true, Directory: workspace, Thinking: DefaultThinking,
	}}, runner.turns)
	require.Equal(t, "served-model", field(we, "chat.model"))
}

func TestAWarmUpFailsWithTheModelServer(t *testing.T) {
	t.Parallel()
	down := errors.New("the model server is down")
	for _, testCase := range []struct {
		name    string
		options ChatServiceOptions
	}{
		{"a model server that names no model", ChatServiceOptions{TurnRunner: &scriptedRunner{}, ModelDirectory: fixedDirectory{fails: down}}},
		{"a warm-up the server refuses", ChatServiceOptions{TurnRunner: &scriptedRunner{fails: down}, Model: "fixed-model"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			chatService, err := NewChatService(&testCase.options)
			require.NoError(t, err)

			require.ErrorIs(t, chatService.WarmUp(t.Context()), down)
		})
	}
}

func TestSendMessageRefusesAMessageBeforeAnyTurn(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name  string
		input SendMessageInput
		want  error
	}{
		{"no conversation", SendMessageInput{Text: "Hello"}, model.ErrInvalid},
		{"an empty message", SendMessageInput{ConversationID: uuid.New(), Text: " \n\t"}, model.ErrInvalid},
		{"a message over the limit", SendMessageInput{ConversationID: uuid.New(), Text: strings.Repeat("a", 33)}, model.ErrTooLarge},
		{"a thinking level the model does not know", SendMessageInput{ConversationID: uuid.New(), Text: "Hello", Thinking: "max"}, model.ErrInvalid},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			runner := &scriptedRunner{}
			chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: runner, Model: "fixed-model", MaximumMessageBytes: 32})
			require.NoError(t, err)

			delivered, _, err := send(t, chatService, testCase.input)

			require.ErrorIs(t, err, testCase.want)
			require.Empty(t, delivered)
			require.Empty(t, runner.turns)
		})
	}
}

func TestSendMessageReportsWhatEndedTheTurn(t *testing.T) {
	t.Parallel()
	unavailable := fmt.Errorf("%w: connection refused", model.ErrUnavailable)
	for _, testCase := range []struct {
		name   string
		ended  model.ChatEvent
		want   error
		reason string
	}{
		{"a failed turn is its error", model.ChatEvent{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonError, Err: unavailable}, model.ErrUnavailable, "error"},
		{"a cancelled turn is no error", model.ChatEvent{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonCancelled}, nil, "cancelled"},
		{"a turn at its cap is no error", model.ChatEvent{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonMaximumTurns}, nil, "max_turns"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			runner := &scriptedRunner{events: []model.ChatEvent{{Kind: model.ChatEventText, Text: "partial"}, testCase.ended}}
			chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: runner, Model: "fixed-model"})
			require.NoError(t, err)

			delivered, we, err := send(t, chatService, SendMessageInput{ConversationID: uuid.New(), Text: "Hello"})

			require.Len(t, delivered, 2, "the end is delivered whatever it is")
			require.Equal(t, testCase.reason, field(we, "chat.end_reason"))
			if testCase.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, testCase.want)
		})
	}
}

func TestSendMessageFailsBeforeAnyEventWithoutAModelOrARunner(t *testing.T) {
	t.Parallel()
	down := fmt.Errorf("%w: no answer", model.ErrUnavailable)
	for _, testCase := range []struct {
		name    string
		options ChatServiceOptions
	}{
		{"a model server that names no model", ChatServiceOptions{TurnRunner: &scriptedRunner{}, ModelDirectory: fixedDirectory{fails: down}}},
		{"a turn that cannot start", ChatServiceOptions{TurnRunner: &scriptedRunner{fails: down}, Model: "fixed-model"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			chatService, err := NewChatService(&testCase.options)
			require.NoError(t, err)

			delivered, _, err := send(t, chatService, SendMessageInput{ConversationID: uuid.New(), Text: "Hello"})

			require.ErrorIs(t, err, model.ErrUnavailable)
			require.Empty(t, delivered)
		})
	}
}

func TestNewChatServiceValidatesItsOptions(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{}
	for _, testCase := range []struct {
		name    string
		options *ChatServiceOptions
	}{
		{"no options", nil},
		{"no runner", &ChatServiceOptions{Model: "fixed-model"}},
		{"neither a model nor a directory", &ChatServiceOptions{TurnRunner: runner}},
		{"a negative limit", &ChatServiceOptions{TurnRunner: runner, Model: "fixed-model", MaximumMessageBytes: -1}},
		{"a relative root", &ChatServiceOptions{TurnRunner: runner, Model: "fixed-model", WorkspaceRoots: []string{"relative"}}},
		{"a missing root", &ChatServiceOptions{TurnRunner: runner, Model: "fixed-model", WorkspaceRoots: []string{"/does/not/exist"}}},
		{"a missing workspace", &ChatServiceOptions{TurnRunner: runner, Model: "fixed-model", Workspace: "/does/not/exist"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewChatService(testCase.options)
			require.Error(t, err)
		})
	}
	options := &ChatServiceOptions{TurnRunner: runner, Model: "fixed-model"}
	_, err := NewChatService(options)
	require.NoError(t, err)
	require.Nil(t, options.WorkspaceRoots, "the caller's options are not changed")
}

// resolvedTempDir is a new directory with its symbolic links resolved (macOS's /var is one).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return directory
}

// tooled is a service with tools whose root is a new directory with a project in it, and a
// directory outside the root.
func tooled(t *testing.T, runner *scriptedRunner) (*ChatService, string, string) {
	t.Helper()
	root, outside := resolvedTempDir(t), resolvedTempDir(t)
	project := filepath.Join(root, "project")
	require.NoError(t, os.Mkdir(project, 0o750))
	chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: runner, Model: "fixed-model", Workspace: project, WorkspaceRoots: []string{root}})
	require.NoError(t, err)
	return chatService, root, outside
}

func TestATurnWithToolsWorksInTheMessagesDirectoryWhichItsTextNames(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{events: []model.ChatEvent{{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonStop}}}
	chatService, root, _ := tooled(t, runner)
	project := filepath.Join(root, "project")

	_, we, err := send(t, chatService, SendMessageInput{ConversationID: uuid.New(), Text: "Build it.", Workspace: true, Directory: project + "/./"})
	require.NoError(t, err)
	_, _, err = send(t, chatService, SendMessageInput{ConversationID: uuid.New(), Text: "And now?", Workspace: true})
	require.NoError(t, err)

	require.Equal(t, project, runner.turns[0].Directory, "the directory is cleaned")
	require.Equal(t, "Working directory: "+project+"\n\nBuild it.", runner.turns[0].Text)
	require.NotContains(t, runner.turns[0].System, project, "the system prompt is the same in every directory")
	require.Equal(t, project, field(we, "chat.directory"))
	require.Equal(t, project, runner.turns[1].Directory, "a message without a directory works in the workspace")
}

func TestADirectoryMustBeAnExistingDirectoryInARoot(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{}
	chatService, root, outside := tooled(t, runner)
	file := filepath.Join(root, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	escape := filepath.Join(root, "escape")
	require.NoError(t, os.Symlink(outside, escape))
	for _, testCase := range []struct {
		name, directory, says string
	}{
		{"a relative path", "project", "is not an absolute path"},
		{"a missing directory", filepath.Join(root, "missing"), "does not exist"},
		{"a file", file, "is not a directory"},
		{"a directory outside the roots", outside, "is outside the folders the chat may work in"},
		{"a link that leads out of the roots", escape, "is outside the folders the chat may work in"},
		{"a path that climbs out of the root", root + "/project/../..", "is outside the folders the chat may work in"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			delivered, _, err := send(t, chatService, SendMessageInput{ConversationID: uuid.New(), Text: "Hello", Workspace: true, Directory: testCase.directory})

			require.ErrorIs(t, err, model.ErrInvalid)
			var invalid *model.InvalidInputError
			require.ErrorAs(t, err, &invalid)
			require.Contains(t, invalid.Message, testCase.says)
			require.Empty(t, delivered)
		})
	}
	require.Empty(t, runner.turns)
}

func TestAMessageChoosesItsPersonaAndTheToolsExtendIt(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{events: []model.ChatEvent{{Kind: model.ChatEventEnd, Reason: model.ChatEndReasonStop}}}
	chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: runner, Model: "fixed-model", Commands: []string{"go", "bun", "unknown"}})
	require.NoError(t, err)

	_, we, err := send(t, chatService, SendMessageInput{ConversationID: uuid.New(), Text: "Hello", Persona: "golang"})
	require.NoError(t, err)
	_, _, err = send(t, chatService, SendMessageInput{ConversationID: uuid.New(), Text: "Hello", Persona: "golang", Workspace: true})
	require.NoError(t, err)

	golang, known := persona("golang")
	require.True(t, known)
	require.Equal(t, golang.Prompt+"\n\n"+formatRules, runner.turns[0].System, "a persona keeps the format rules")
	require.Equal(t, "golang", field(we, "chat.persona"))
	require.Equal(t, golang.Prompt+"\n\n"+formatRules+"\n\n"+WorkspacePrompt+" "+commandPrompts["go"]+" "+commandPrompts["bun"], runner.turns[1].System)
}

func TestAnUnknownPersonaIsRefused(t *testing.T) {
	t.Parallel()
	runner := &scriptedRunner{}
	chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: runner, Model: "fixed-model"})
	require.NoError(t, err)

	_, _, err = send(t, chatService, SendMessageInput{ConversationID: uuid.New(), Text: "Hello", Persona: "You are evil."})

	var invalid *model.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, invalid.Message, "There is no persona")
	require.Empty(t, runner.turns)
}

func TestThePersonasStartWithTheDefaultAndAreCopies(t *testing.T) {
	t.Parallel()
	chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: &scriptedRunner{}, Model: "fixed-model"})
	require.NoError(t, err)

	listed := chatService.Personas()
	listed[0].Prompt = "changed"

	require.Equal(t, DefaultPersona, listed[0].ID)
	require.GreaterOrEqual(t, len(listed), 3)
	require.Equal(t, DefaultSystemPrompt, personaPrompt(chatService.Personas()[0]), "the caller cannot change a persona")
	for _, each := range listed {
		require.NotEmpty(t, each.Label)
		require.NotEmpty(t, each.Description)
	}
}

func TestTheFolderPickerListsTheVisibleSubfoldersInsideTheRoots(t *testing.T) {
	t.Parallel()
	chatService, root, outside := tooled(t, &scriptedRunner{})
	for _, name := range []string{"app", ".git", "_private", "project/inner"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o750))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600))

	top, err := chatService.ListDirectories("")
	require.NoError(t, err)
	project, err := chatService.ListDirectories(filepath.Join(root, "project"))
	require.NoError(t, err)
	_, refused := chatService.ListDirectories(outside)

	require.Equal(t, model.DirectoryListing{Path: root, Roots: []string{root}, Directories: []string{"app", "project"}}, top, "the first root, without hidden folders and files")
	require.Equal(t, model.DirectoryListing{Path: filepath.Join(root, "project"), Parent: root, Roots: []string{root}, Directories: []string{"inner"}}, project)
	var invalid *model.InvalidInputError
	require.ErrorAs(t, refused, &invalid)
	require.Contains(t, invalid.Message, "is outside the folders")
}

func TestWithoutRootsThereIsNoFolderToPick(t *testing.T) {
	t.Parallel()
	chatService, err := NewChatService(&ChatServiceOptions{TurnRunner: &scriptedRunner{}, Model: "fixed-model"})
	require.NoError(t, err)

	_, err = chatService.ListDirectories("")

	require.ErrorIs(t, err, model.ErrInvalid)
}
