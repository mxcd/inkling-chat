package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/govengers/aikido/tools"
)

// The bounds of the background processes of one workspace: how many run at once, how much of
// their output is kept (the newest), how long a start waits for the first output and how long a
// stop waits for the process to end on SIGTERM before it is killed.
const (
	maximumProcesses     = 8
	processOutputBytes   = 64 << 10
	processTailBytes     = 8 << 10
	processStartGrace    = 2 * time.Second
	processStopGrace     = 3 * time.Second
	maximumProcessWaitMs = 30_000
)

// processes are the background processes of one workspace: host commands (go run, bun dev) that
// run past a tool call, for servers and watchers. Each runs in its own process group, so stopping
// it ends what it started. It is safe for concurrent use.
type processes struct {
	directory string
	commands  map[string]HostCommand
	// ended closes with the workspace; every process it started is killed then
	ended chan struct{}

	mutex   sync.Mutex
	running map[int]*process
	next    int
}

func newProcesses(directory string, commands []HostCommand) *processes {
	byName := make(map[string]HostCommand, len(commands))
	for _, command := range commands {
		byName[command.Name] = command
	}
	return &processes{directory: directory, commands: byName, ended: make(chan struct{}), running: map[int]*process{}}
}

// process is one started host command and the newest part of its output.
type process struct {
	id      int
	line    string
	command *exec.Cmd
	output  *tailBuffer
	done    chan struct{}
	exit    int // valid once done is closed
}

// processView is what the model reads about a process.
type processView struct {
	ID       int    `json:"id"`
	Command  string `json:"command"`
	Running  bool   `json:"running"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Output   string `json:"output,omitempty"`
	Dropped  int64  `json:"dropped_bytes,omitempty"`
}

func (p *process) view(tail int) processView {
	view := processView{ID: p.id, Command: p.line, Running: true}
	select {
	case <-p.done:
		view.Running = false
		view.ExitCode = &p.exit
	default:
	}
	if tail > 0 {
		view.Output, view.Dropped = p.output.tail(tail)
	}
	return view
}

// start runs program with arguments and the extra variables environment in the workspace's
// subdirectory directory ("" or "." for its root) and returns once the process ended or after
// processStartGrace, with its output so far. program is a host command, or a program inside the
// workspace (a path with a slash: a binary go build made), which runs with the first host
// command's environment.
func (p *processes) start(program string, arguments []string, directory string, environment map[string]string) (processView, error) {
	workingDirectory, err := inside(p.directory, directory)
	if err != nil {
		return processView{}, err
	}
	host, err := p.resolve(program, workingDirectory, arguments)
	if err != nil {
		return processView{}, err
	}
	variables, err := extraEnvironment(environment)
	if err != nil {
		return processView{}, err
	}
	host.Environment = append(slices.Clone(host.Environment), variables...)
	p.mutex.Lock()
	if p.count() >= maximumProcesses {
		p.mutex.Unlock()
		return processView{}, fmt.Errorf("%d background processes are running; stop one first", maximumProcesses)
	}
	p.next++
	started := &process{id: p.next, line: strings.Join(append([]string{program}, arguments...), " "), output: newTailBuffer(processOutputBytes), done: make(chan struct{})}
	end := started.prepare(host, arguments, workingDirectory)
	if err := started.command.Start(); err != nil {
		p.mutex.Unlock()
		end()
		return processView{}, fmt.Errorf("start %s: %w", program, err)
	}
	go func() { // the workspace's end kills the process; its own end releases the context
		select {
		case <-p.ended:
		case <-started.done:
		}
		end()
	}()
	p.running[started.id] = started
	p.mutex.Unlock()
	go started.wait()
	select {
	case <-started.done:
	case <-time.After(processStartGrace):
	}
	return started.view(processTailBytes), nil
}

// prepare builds the process's command in its own process group and returns what ends its
// lifetime: a canceled lifetime kills the whole group, what the command started too.
func (p *process) prepare(host HostCommand, arguments []string, workingDirectory string) context.CancelFunc {
	lifetime, end := context.WithCancel(context.Background())
	//nolint:gosec // G204: a host command the operator configured or a program in the workspace, its arguments checked by its Allow
	p.command = exec.CommandContext(lifetime, host.Path, arguments...)
	p.command.Cancel = func() error { return syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL) }
	p.command.Dir = workingDirectory
	p.command.Env = host.Environment
	p.command.Stdout = p.output
	p.command.Stderr = p.output
	p.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return end
}

// resolve is the command program names: a host command whose Allow accepts the arguments, or an
// executable file inside the workspace.
func (p *processes) resolve(program string, workingDirectory string, arguments []string) (HostCommand, error) {
	if host, found := p.commands[program]; found {
		return host, host.Allow(slices.Clone(arguments))
	}
	if !strings.Contains(program, "/") || len(p.commands) == 0 {
		return HostCommand{}, fmt.Errorf("%q cannot run in the background; %s can, or a program built inside the working directory (a path like ./server)", program, strings.Join(p.names(), " and "))
	}
	path := program
	if !filepath.IsAbs(path) {
		path = filepath.Join(workingDirectory, path)
	}
	directory, err := inside(p.directory, filepath.Dir(path))
	if err != nil {
		return HostCommand{}, err
	}
	path = filepath.Join(directory, filepath.Base(path))
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return HostCommand{}, fmt.Errorf("%q is not an executable file inside the working directory (go build -o it there first)", program)
	}
	base := p.commands[p.names()[0]]
	return HostCommand{Name: program, Path: path, Environment: base.Environment, Allow: func([]string) error { return nil }}, nil
}

// reservedVariables are what a process's environment must keep: the host command's own.
var reservedVariables = []string{"PATH", "HOME", "TMPDIR"}

// extraEnvironment is environment as NAME=value, sorted; a name that is not a plain variable name
// or that is reserved is refused.
func extraEnvironment(environment map[string]string) ([]string, error) {
	variables := make([]string, 0, len(environment))
	for name, value := range environment {
		if !validVariable(name) || slices.Contains(reservedVariables, name) {
			return nil, fmt.Errorf("environment variable %q cannot be set", name)
		}
		variables = append(variables, name+"="+value)
	}
	slices.Sort(variables)
	return variables, nil
}

func validVariable(name string) bool {
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	for _, character := range name {
		letter := character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z'
		if character != '_' && !letter && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func (p *process) wait() {
	err := p.command.Wait()
	p.exit = 0
	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
		p.exit = exitErr.ExitCode()
	} else if err != nil {
		p.exit = -1
	}
	close(p.done)
}

// output is the process's state and the newest tail bytes of its output, after waiting up to wait
// for it to end; without an id it lists every process.
func (p *processes) output(ctx context.Context, id int, tail int, wait time.Duration) ([]processView, error) {
	if id == 0 {
		p.mutex.Lock()
		defer p.mutex.Unlock()
		views := make([]processView, 0, len(p.running))
		for _, known := range p.running {
			views = append(views, known.view(0))
		}
		slices.SortFunc(views, func(a, b processView) int { return a.ID - b.ID })
		return views, nil
	}
	known, err := p.find(id)
	if err != nil {
		return nil, err
	}
	if wait > 0 {
		select {
		case <-known.done:
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	return []processView{known.view(tail)}, nil
}

// stop ends a process with its group: SIGTERM, then SIGKILL after processStopGrace.
func (p *processes) stop(id int) (processView, error) {
	known, err := p.find(id)
	if err != nil {
		return processView{}, err
	}
	known.signal(syscall.SIGTERM)
	select {
	case <-known.done:
	case <-time.After(processStopGrace):
		known.signal(syscall.SIGKILL)
		<-known.done
	}
	p.mutex.Lock()
	delete(p.running, id)
	p.mutex.Unlock()
	return known.view(processTailBytes), nil
}

// close kills every process of the workspace.
func (p *processes) close() {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	select {
	case <-p.ended:
	default:
		close(p.ended)
	}
	clear(p.running)
}

func (p *process) signal(signal syscall.Signal) {
	select {
	case <-p.done:
	default:
		_ = syscall.Kill(-p.command.Process.Pid, signal) // the group: what the command started too
	}
}

func (p *processes) find(id int) (*process, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if known, found := p.running[id]; found {
		return known, nil
	}
	return nil, fmt.Errorf("no background process %d; process_output without an id lists them", id)
}

// count is the processes still running; the caller holds the mutex.
func (p *processes) count() int {
	running := 0
	for _, known := range p.running {
		select {
		case <-known.done:
		default:
			running++
		}
	}
	return running
}

func (p *processes) names() []string {
	names := make([]string, 0, len(p.commands))
	for name := range p.commands {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// inside is the subdirectory relative of root, which must stay inside root with its symbolic
// links resolved.
func inside(root, relative string) (string, error) {
	candidate := filepath.Join(root, relative)
	if filepath.IsAbs(relative) {
		candidate = filepath.Clean(relative)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("directory %q: %w", relative, err)
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("directory %q: %w", root, err)
	}
	if resolved != base && !strings.HasPrefix(resolved, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("directory %q is outside the working directory", relative)
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", fmt.Errorf("directory %q is not a directory", relative)
	}
	return resolved, nil
}

// tailBuffer keeps the newest limit bytes written to it and counts what it dropped.
type tailBuffer struct {
	mutex   sync.Mutex
	limit   int
	data    []byte
	dropped int64
}

func newTailBuffer(limit int) *tailBuffer { return &tailBuffer{limit: limit} }

func (b *tailBuffer) Write(written []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.data = append(b.data, written...)
	if excess := len(b.data) - b.limit; excess > 0 {
		b.dropped += int64(excess)
		b.data = append(b.data[:0], b.data[excess:]...)
	}
	return len(written), nil
}

// tail is the newest count bytes and how many bytes before them are not shown.
func (b *tailBuffer) tail(count int) (string, int64) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	start := max(0, len(b.data)-count)
	return string(b.data[start:]), b.dropped + int64(start)
}

// registerProcesses adds the tools of the background processes to registry.
func registerProcesses(registry *tools.Registry, managed *processes) error {
	names := managed.names()
	start := `Starts a long-running program in the background on the user's computer (a server: go run ./cmd/server, a watcher: bun run dev, or a binary you built: ./server) and returns its id and first output after about two seconds. Use it instead of bash for anything that does not end by itself, and to run a program you built. Set variables with env, not in a shell. The user's own apps use ports below 20000: give a test server a port from 20000 to 29999 and check the output for a bind error. Check it with http_request, read the output with process_output, end it with stop_process.`
	err := registry.Register(llmTool("start_process", start, tools.Object(map[string]any{
		"program":   tools.String("The program: " + strings.Join(names, " or ") + ", or the path of a program inside the working directory such as ./server."),
		"args":      tools.Array(tools.String("One argument."), "The arguments, one per item, without shell quoting."),
		"directory": tools.String(`The directory to run in, relative to the working directory; "." when left out.`),
		"env":       map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": `Extra environment variables, e.g. {"ADDR": ":20080"}; PATH, HOME and TMPDIR stay as they are.`},
	}, "program", "args")), func(_ context.Context, arguments json.RawMessage, _ tools.Environment) (tools.Result, error) {
		var parsed struct {
			Program   string            `json:"program"`
			Args      []string          `json:"args"`
			Directory string            `json:"directory"`
			Env       map[string]string `json:"env"`
		}
		if err := json.Unmarshal(arguments, &parsed); err != nil {
			return tools.Result{}, fmt.Errorf("%w for start_process: %w", tools.ErrInvalidArguments, err)
		}
		view, err := managed.start(parsed.Program, parsed.Args, parsed.Directory, parsed.Env)
		return tools.Result{Content: view}, err
	})
	if err != nil {
		return err
	}
	err = registry.Register(llmTool("process_output", "Shows a background process: whether it runs, its exit code and the newest part of its output. Without an id it lists all background processes.", tools.Object(map[string]any{
		"id":           tools.Integer("The process id start_process returned."),
		"tail_bytes":   tools.Integer("How many of the newest output bytes to show; 8192 when left out, at most 65536."),
		"wait_seconds": tools.Integer("Wait up to this many seconds (at most 30) for the process to end before answering."),
	})), func(ctx context.Context, arguments json.RawMessage, _ tools.Environment) (tools.Result, error) {
		var parsed struct {
			ID          int `json:"id"`
			TailBytes   int `json:"tail_bytes"`
			WaitSeconds int `json:"wait_seconds"`
		}
		if err := json.Unmarshal(arguments, &parsed); err != nil {
			return tools.Result{}, fmt.Errorf("%w for process_output: %w", tools.ErrInvalidArguments, err)
		}
		tail := parsed.TailBytes
		if tail <= 0 {
			tail = processTailBytes
		}
		wait := time.Duration(min(max(parsed.WaitSeconds*1000, 0), maximumProcessWaitMs)) * time.Millisecond
		views, err := managed.output(ctx, parsed.ID, min(tail, processOutputBytes), wait)
		return tools.Result{Content: views}, err
	})
	if err != nil {
		return err
	}
	return registry.Register(llmTool("stop_process", "Stops a background process and what it started (SIGTERM, SIGKILL after three seconds) and shows its last output.", tools.Object(map[string]any{
		"id": tools.Integer("The process id start_process returned."),
	}, "id")), func(_ context.Context, arguments json.RawMessage, _ tools.Environment) (tools.Result, error) {
		var parsed struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(arguments, &parsed); err != nil {
			return tools.Result{}, fmt.Errorf("%w for stop_process: %w", tools.ErrInvalidArguments, err)
		}
		view, err := managed.stop(parsed.ID)
		return tools.Result{Content: view}, err
	})
}
