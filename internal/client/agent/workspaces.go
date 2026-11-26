package agent

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/govengers/aikido/agent"
	"github.com/govengers/aikido/llm"
	"github.com/govengers/aikido/tools"
	"github.com/govengers/aikido/tools/shell"
	"github.com/govengers/aikido/vfs/osfs"
)

// The defaults of the tool calls' time limits (Timeouts): a call ends after DefaultToolTimeout; a
// bash call may name a longer one (go test), up to DefaultMaximumToolTimeout. Servers and watchers
// run in the background (processes.go). The shell's output stays below the turn's
// maximumResultBytes once encoded as JSON.
const (
	DefaultToolTimeout        = 30 * time.Second
	DefaultMaximumToolTimeout = 10 * time.Minute
	// sessionToolGrace lets a call's own deadline report to the model before aikido's ends it.
	sessionToolGrace = time.Minute

	shellOutputBytes   = 24 << 10
	maximumWorkspaces  = 32
	neutralMountPoint  = "the working directory the message names"
	mountPointInPrompt = "the workspace is "
)

// workspace is the tools of one conversation over one directory: the shell in it keeps its
// working directory and /tmp from call to call.
type workspace struct {
	conversation uuid.UUID
	directory    string
	tools        *tools.Registry
	storage      *osfs.Storage
	processes    *processes
}

// Timeouts bound the tool calls: Default every call, Maximum what a bash call may ask for.
type Timeouts struct {
	Default time.Duration
	Maximum time.Duration
}

// release kills the workspace's background processes and closes its storage; a close error leaves
// only a handle open.
func (w *workspace) release() error {
	w.processes.close()
	return w.storage.Close()
}

// workspaces keeps the tools of the recent conversations, at most maximumWorkspaces, the least
// recently used one evicted. It is safe for concurrent use.
//
// shortcut: an evicted or replaced workspace's storage is closed even if a turn still runs on it
// (32 newer conversations during that turn, or a second message of the conversation with another
// directory, which the frontend does not send while a turn runs); count the turns on a workspace
// once that can happen.
type workspaces struct {
	mutex    sync.Mutex
	order    *list.List // of *workspace, the most recently used first
	entries  map[uuid.UUID]*list.Element
	commands []HostCommand
	timeouts Timeouts
}

func newWorkspaces(commands []HostCommand, timeouts Timeouts) *workspaces {
	return &workspaces{order: list.New(), entries: map[uuid.UUID]*list.Element{}, commands: commands, timeouts: timeouts}
}

// of is the tools of conversation in directory: the kept ones, or new ones, which replace those
// of the conversation's earlier directory.
func (w *workspaces) of(conversation uuid.UUID, directory string) (*tools.Registry, error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	if element, found := w.entries[conversation]; found {
		kept := element.Value.(*workspace)
		if kept.directory == directory {
			w.order.MoveToFront(element)
			return kept.tools, nil
		}
		w.remove(element)
	}
	built, err := newWorkspace(directory, w.commands, w.timeouts)
	if err != nil {
		return nil, err
	}
	built.conversation = conversation
	w.entries[conversation] = w.order.PushFront(built)
	if w.order.Len() > maximumWorkspaces {
		w.remove(w.order.Back())
	}
	return built.tools, nil
}

// remove forgets a workspace and closes its storage; a close error leaves only a handle open.
func (w *workspaces) remove(element *list.Element) {
	removed := w.order.Remove(element).(*workspace)
	delete(w.entries, removed.conversation)
	_ = removed.release()
}

// close closes every kept storage.
func (w *workspaces) close() error {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	var failures []error
	for element := w.order.Front(); element != nil; element = w.order.Front() {
		removed := w.order.Remove(element).(*workspace)
		delete(w.entries, removed.conversation)
		failures = append(failures, removed.release())
	}
	return errors.Join(failures...)
}

// newWorkspace is the tools over directory: the file tools and the shell over one storage that
// refuses symbolic links and hides hidden paths (.env, .git), the shell with the host commands,
// and the background processes of the host commands, every call under timeouts.
func newWorkspace(directory string, commands []HostCommand, timeouts Timeouts) (*workspace, error) {
	storage, err := osfs.New(&osfs.Options{Directory: directory, RefuseSymlinks: true})
	if err != nil {
		return nil, fmt.Errorf("agent: tools of %s: %w", directory, err)
	}
	managed := newProcesses(directory, commands)
	registry, err := newTools(storage, directory, commands, managed, timeouts)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("agent: tools of %s: %w", directory, err), storage.Close())
	}
	return &workspace{directory: directory, tools: registry, storage: storage, processes: managed}, nil
}

func newTools(storage *osfs.Storage, directory string, hostCommands []HostCommand, managed *processes, timeouts Timeouts) (*tools.Registry, error) {
	inner := tools.NewRegistry()
	if err := agent.RegisterVFSTools(inner, &agent.VFSToolOptions{Storage: storage, HideHiddenPaths: true}); err != nil {
		return nil, fmt.Errorf("file tools: %w", err)
	}
	if len(hostCommands) > 0 {
		if err := registerProcesses(inner, managed); err != nil {
			return nil, fmt.Errorf("background processes: %w", err)
		}
		if err := registerHTTPRequest(inner); err != nil {
			return nil, fmt.Errorf("http requests: %w", err)
		}
	}
	commands := make([]shell.Command, 0, len(hostCommands))
	for _, host := range hostCommands {
		// no timeout of its own: the call's deadline ends it with its process group
		command, err := shell.External(&shell.ExternalOptions{
			Name: host.Name, Path: host.Path, Directory: directory, FollowWorkingDirectory: true,
			Environment: host.Environment, Allow: host.Allow, Summary: host.Summary,
		})
		if err != nil {
			return nil, fmt.Errorf("%s command: %w", host.Name, err)
		}
		commands = append(commands, command)
	}
	registry := tools.NewRegistry()
	for _, definition := range inner.Definitions() {
		if err := registry.Register(definition, limited(inner, definition.Name, timeouts.Default)); err != nil {
			return nil, err
		}
	}
	err := registerShell(registry, &shell.Options{
		Storage: storage, MountPoint: directory, HideHiddenPaths: true, Commands: commands, Timeout: -1, MaximumOutputBytes: shellOutputBytes,
	}, timeouts)
	if err != nil {
		return nil, fmt.Errorf("shell: %w", err)
	}
	return registry, nil
}

// limited dispatches the tool name of inner with a deadline of timeout.
func limited(inner *tools.Registry, name string, timeout time.Duration) tools.Handler {
	return func(ctx context.Context, arguments json.RawMessage, environment tools.Environment) (tools.Result, error) {
		callContext, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		result, err := inner.Dispatch(callContext, llm.ToolCall{Name: name, Arguments: string(arguments)}, environment)
		if err != nil && ctx.Err() == nil && errors.Is(callContext.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("%s timed out after %s: %w", name, timeout, err)
		}
		return result, err
	}
}

// shellOutput is what the model reads of a bash call, aikido's shape plus a timed-out note.
type shellOutput struct {
	Output    string `json:"output"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated"`
	TimedOut  string `json:"timed_out,omitempty"`
}

// registerShell adds the shell to registry. Its description does not name the directory: the tool
// definitions, and with them the prompt start the model server caches, are then the same for
// every directory; the message names the directory instead. A call ends after timeouts.Default
// unless it names timeout_seconds, up to timeouts.Maximum; one that runs out of time returns its
// output so far with exit code 124.
func registerShell(registry *tools.Registry, options *shell.Options, timeouts Timeouts) error {
	described := tools.NewRegistry() // aikido writes the description; its handler is not used
	if err := shell.Register(described, options); err != nil {
		return err
	}
	bash, err := shell.New(options)
	if err != nil {
		return err
	}
	definition := described.Definitions()[0]
	definition.Description = strings.Replace(definition.Description, mountPointInPrompt+options.MountPoint, mountPointInPrompt+neutralMountPoint, 1) +
		fmt.Sprintf(" A call ends after %s unless it sets timeout_seconds (at most %d); a call that runs out of time stops with exit code 124. "+
			"Never start servers or watchers here (no &): use start_process. The listed host programs (go, bun) run on the user's computer: "+
			"they get only their own environment, so variables the script sets (ADDR=... go run) do not reach them, and their /tmp is the computer's, "+
			"not this shell's in-memory /tmp; build into the working directory instead. The shell cannot run other programs, including ones you built: use start_process. It has no network: check a server with http_request.",
			timeouts.Default, int(timeouts.Maximum.Seconds()))
	definition.Parameters = tools.Object(map[string]any{
		"command":         tools.String("The bash script to run."),
		"description":     tools.String("What the command does, in 5-10 words."),
		"timeout_seconds": tools.Integer(fmt.Sprintf("How long the call may run, for go test or a build; %d when left out, at most %d.", int(timeouts.Default.Seconds()), int(timeouts.Maximum.Seconds()))),
	}, "command")
	return registry.Register(definition, func(ctx context.Context, arguments json.RawMessage, _ tools.Environment) (tools.Result, error) {
		var parsed struct {
			Command        string `json:"command"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(arguments, &parsed); err != nil {
			return tools.Result{}, fmt.Errorf("%w for %s: %w", tools.ErrInvalidArguments, definition.Name, err)
		}
		timeout := callTimeout(parsed.TimeoutSeconds, timeouts)
		callContext, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		result, err := bash.Run(callContext, parsed.Command)
		output := shellOutput{Output: result.Output, ExitCode: result.ExitCode, Truncated: result.Truncated}
		if err != nil && ctx.Err() == nil && errors.Is(callContext.Err(), context.DeadlineExceeded) {
			output.ExitCode = 124
			output.TimedOut = fmt.Sprintf("stopped after %s; a longer task may set timeout_seconds, a server belongs in start_process", timeout)
			return tools.Result{Content: output}, nil
		}
		if err != nil {
			return tools.Result{}, err
		}
		return tools.Result{Content: output}, nil
	})
}

// callTimeout is the time a bash call asked for, within the bounds; the default without one.
func callTimeout(seconds int, timeouts Timeouts) time.Duration {
	if seconds <= 0 {
		return timeouts.Default
	}
	return min(time.Duration(seconds)*time.Second, timeouts.Maximum)
}

// llmTool is a tool definition.
func llmTool(name, description string, parameters json.RawMessage) llm.ToolDefinition {
	return llm.ToolDefinition{Name: name, Description: description, Parameters: parameters}
}
