package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/govengers/aikido/llm"
	"github.com/govengers/aikido/tools"
	"github.com/stretchr/testify/require"

	"github.com/mxcd/inkling-chat/internal/model"
	"github.com/mxcd/inkling-chat/internal/modeltest"
)

// run calls the shell of registry with command and returns its output.
func run(t *testing.T, registry *tools.Registry, command string) string {
	t.Helper()
	arguments, err := json.Marshal(map[string]string{"command": command})
	require.NoError(t, err)
	result, err := registry.Dispatch(t.Context(), llm.ToolCall{Name: "bash", Arguments: string(arguments)}, tools.Environment{})
	require.NoError(t, err)
	encoded, err := json.Marshal(result.Content)
	require.NoError(t, err)
	var output struct {
		Output string `json:"output"`
	}
	require.NoError(t, json.Unmarshal(encoded, &output))
	return output.Output
}

func TestAConversationKeepsItsShellInItsDirectoryAndGetsAFreshOneInAnother(t *testing.T) {
	t.Parallel()
	kept := newWorkspaces(nil, testTimeouts)
	t.Cleanup(func() { require.NoError(t, kept.close()) })
	first, second := temporaryDirectory(t), temporaryDirectory(t)
	require.NoError(t, os.Mkdir(filepath.Join(first, "app"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(first, "app", "main.go"), []byte("package main\n"), 0o600))
	conversation, other := uuid.New(), uuid.New()

	registry, err := kept.of(conversation, first)
	require.NoError(t, err)
	run(t, registry, "cd app")
	again, err := kept.of(conversation, first)
	require.NoError(t, err)
	require.Same(t, registry, again, "the same conversation and directory keep their tools")
	require.Equal(t, filepath.Join(first, "app")+"\n", run(t, again, "pwd"), "the shell keeps its working directory")
	otherRegistry, err := kept.of(other, first)
	require.NoError(t, err)
	require.NotSame(t, registry, otherRegistry, "another conversation gets tools of its own")
	require.Equal(t, first+"\n", run(t, otherRegistry, "pwd"))
	moved, err := kept.of(conversation, second)
	require.NoError(t, err)

	require.NotSame(t, registry, moved)
	require.Equal(t, second+"\n", run(t, moved, "pwd"), "another directory gets a fresh shell")
	require.Equal(t, 2, kept.order.Len(), "the conversation's earlier directory is forgotten")
}

func TestTheLeastRecentlyUsedWorkspaceIsEvictedAtTheCap(t *testing.T) {
	t.Parallel()
	kept := newWorkspaces(nil, testTimeouts)
	t.Cleanup(func() { require.NoError(t, kept.close()) })
	directory := temporaryDirectory(t)
	conversations := make([]uuid.UUID, maximumWorkspaces+1)
	for index := range conversations {
		conversations[index] = uuid.New()
		_, err := kept.of(conversations[index], directory)
		require.NoError(t, err)
		if index == 0 {
			continue
		}
		_, err = kept.of(conversations[0], directory) // the first stays the most recently used
		require.NoError(t, err)
	}

	require.Equal(t, maximumWorkspaces, kept.order.Len())
	require.Contains(t, kept.entries, conversations[0])
	require.NotContains(t, kept.entries, conversations[1], "the least recently used one went")
}

func TestTheToolDefinitionsAreTheSameInEveryDirectory(t *testing.T) {
	t.Parallel()
	first, err := newWorkspace(temporaryDirectory(t), nil, testTimeouts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.storage.Close()) })
	second, err := newWorkspace(temporaryDirectory(t), nil, testTimeouts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.storage.Close()) })

	firstJSON, err := json.Marshal(first.tools.Definitions())
	require.NoError(t, err)
	secondJSON, err := json.Marshal(second.tools.Definitions())
	require.NoError(t, err)

	require.JSONEq(t, string(firstJSON), string(secondJSON), "the cached prompt start does not depend on the directory")
	require.NotContains(t, string(firstJSON), first.directory)
	require.Contains(t, string(firstJSON), neutralMountPoint)
}

func TestTheToolsHideHiddenFilesAndRefuseSymbolicLinks(t *testing.T) {
	t.Parallel()
	directory := temporaryDirectory(t)
	require.NoError(t, os.WriteFile(filepath.Join(directory, ".env"), []byte("SECRET=1\n"), 0o600))
	outside := temporaryDirectory(t)
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside\n"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(directory, "link.txt")))
	require.NoError(t, os.Symlink(filepath.Join(directory, ".env"), filepath.Join(directory, "public.txt")))
	built, err := newWorkspace(directory, nil, testTimeouts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, built.storage.Close()) })

	require.NotContains(t, run(t, built.tools, "cat .env; cat link.txt; cat public.txt; ls -a"), "SECRET")
	require.NotContains(t, run(t, built.tools, "cat link.txt"), "outside")
	for _, path := range []string{".env", "link.txt", "public.txt"} {
		_, err := built.tools.Dispatch(t.Context(), llm.ToolCall{Name: "read_file", Arguments: `{"path":"` + path + `"}`}, tools.Environment{})
		require.Error(t, err, path)
	}
}

func TestAScriptedTurnRunsGoInTheShellAndTheModelReadsTheResult(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go binary")
	}
	goCommand, err := FindGo(t.Context(), "")
	require.NoError(t, err)
	server := modeltest.NewServer(t, []string{servedModel},
		modeltest.ToolCall("call-1", "bash", `{"command":"go version && go build ./... && echo built"}`),
		modeltest.ToolCall("call-2", "bash", `{"command":"go install ./..."}`),
		modeltest.Text("It builds."),
	)
	runtime, directory := newRuntime(t, server, *goCommand)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module example.com/scratch\n\ngo 1.26\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600))

	events := converse(t, runtime, model.ChatTurn{ConversationID: uuid.New(), Text: "Build it.", Model: servedModel, Workspace: true, Directory: directory})

	require.Equal(t, model.ChatEvent{Kind: model.ChatEventToolCall, CallID: "call-1", Tool: "bash", Target: "go version && go build ./... && echo built"}, events[1])
	require.True(t, events[2].OK, "the shell ran: %+v", events[2])
	asked := server.Asked()
	require.Len(t, asked, 3)
	require.Contains(t, asked[1], "go version go1", "the go command's output reaches the model")
	require.Contains(t, asked[1], `built\\n`)
	require.Contains(t, asked[2], "refused: go install writes to the user's GOBIN")
	require.Contains(t, asked[0], `"name":"bash"`)
	require.Contains(t, asked[0], "go: the host's Go toolchain", "the shell's description lists go")
}

// testTimeouts keep a slow test short: a call has 300 ms unless it asks for more, up to 5 s.
var testTimeouts = Timeouts{Default: 300 * time.Millisecond, Maximum: 5 * time.Second}

// bash calls the shell of registry with arguments and returns what the model reads.
func bash(t *testing.T, registry *tools.Registry, arguments string) shellOutput {
	t.Helper()
	result, err := registry.Dispatch(t.Context(), llm.ToolCall{Name: "bash", Arguments: arguments}, tools.Environment{})
	require.NoError(t, err)
	encoded, err := json.Marshal(result.Content)
	require.NoError(t, err)
	var output shellOutput
	require.NoError(t, json.Unmarshal(encoded, &output))
	return output
}

func TestABashCallThatRunsOutOfTimeStopsWithItsOutputSoFar(t *testing.T) {
	t.Parallel()
	built, err := newWorkspace(temporaryDirectory(t), nil, testTimeouts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, built.release()) })

	stopped := bash(t, built.tools, `{"command":"echo before; sleep 3; echo after"}`)
	require.Equal(t, 124, stopped.ExitCode)
	require.Contains(t, stopped.Output, "before")
	require.NotContains(t, stopped.Output, "after")
	require.Contains(t, stopped.TimedOut, "start_process")

	longer := bash(t, built.tools, `{"command":"sleep 1; echo done","timeout_seconds":3}`)
	require.Equal(t, 0, longer.ExitCode, longer.Output)
	require.Contains(t, longer.Output, "done")
	require.Empty(t, longer.TimedOut)
}

func TestACallAsksForAtMostTheMaximumTime(t *testing.T) {
	t.Parallel()
	require.Equal(t, testTimeouts.Default, callTimeout(0, testTimeouts))
	require.Equal(t, 2*time.Second, callTimeout(2, testTimeouts))
	require.Equal(t, testTimeouts.Maximum, callTimeout(3600, testTimeouts))
}

func TestABackgroundProcessRunsPastItsCallAndStopsWithWhatItStarted(t *testing.T) {
	t.Parallel()
	directory := temporaryDirectory(t)
	require.NoError(t, os.Mkdir(filepath.Join(directory, "app"), 0o750))
	shellCommand := HostCommand{Name: "sh", Path: "/bin/sh", Allow: func([]string) error { return nil }, Summary: "test shell"}
	built, err := newWorkspace(directory, []HostCommand{shellCommand}, testTimeouts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, built.release()) })
	dispatch := func(name, arguments string) map[string]any {
		t.Helper()
		result, err := built.tools.Dispatch(t.Context(), llm.ToolCall{Name: name, Arguments: arguments}, tools.Environment{})
		require.NoError(t, err)
		encoded, err := json.Marshal(result.Content)
		require.NoError(t, err)
		var decoded any
		require.NoError(t, json.Unmarshal(encoded, &decoded))
		if list, ok := decoded.([]any); ok {
			return list[0].(map[string]any)
		}
		return decoded.(map[string]any)
	}

	// the call returns after the start grace, far beyond the 300 ms a call has
	started := dispatch("start_process", `{"program":"sh","args":["-c","pwd; echo serving; sleep 60"],"directory":"app"}`)
	require.Equal(t, true, started["running"])
	require.Contains(t, started["output"], "serving")
	require.Contains(t, started["output"], "/app\n", "it runs in the directory it names")
	id := started["id"].(float64)

	listed := dispatch("process_output", `{}`)
	require.Equal(t, id, listed["id"])

	stopped := dispatch("stop_process", `{"id":1}`)
	require.Equal(t, false, stopped["running"])
	require.NotEqual(t, float64(0), stopped["exit_code"], "a signal ended it")

	_, err = built.tools.Dispatch(t.Context(), llm.ToolCall{Name: "start_process", Arguments: `{"program":"rm","args":["-rf","/"]}`}, tools.Environment{})
	require.ErrorContains(t, err, "cannot run in the background")
	_, err = built.tools.Dispatch(t.Context(), llm.ToolCall{Name: "start_process", Arguments: `{"program":"sh","args":["-c","true"],"directory":"../.."}`}, tools.Environment{})
	require.ErrorContains(t, err, "outside the working directory")
}

func TestTheTailKeepsTheNewestOutput(t *testing.T) {
	t.Parallel()
	buffer := newTailBuffer(4)
	_, _ = buffer.Write([]byte("abcdef"))
	output, dropped := buffer.tail(3)
	require.Equal(t, "def", output)
	require.Equal(t, int64(3), dropped)
}

func TestABackgroundProcessGetsItsEnvironmentAndMayBeAProgramBuiltInTheDirectory(t *testing.T) {
	t.Parallel()
	directory := temporaryDirectory(t)
	//nolint:gosec // G306: the test program must be executable
	require.NoError(t, os.WriteFile(filepath.Join(directory, "server"), []byte("#!/bin/sh\necho \"addr=$ADDR home=$HOME\"\nsleep 60\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("x"), 0o600))
	shellCommand := HostCommand{Name: "sh", Path: "/bin/sh", Environment: []string{"HOME=/home/test"}, Allow: func([]string) error { return nil }, Summary: "test shell"}
	managed := newProcesses(directory, []HostCommand{shellCommand})
	t.Cleanup(managed.close)

	started, err := managed.start("./server", nil, ".", map[string]string{"ADDR": ":20080"})
	require.NoError(t, err)
	require.True(t, started.Running)
	require.Contains(t, started.Output, "addr=:20080 home=/home/test", "the extra variable and the command's own environment")

	viaHost, err := managed.start("sh", []string{"-c", "echo $ADDR"}, "", map[string]string{"ADDR": "x"})
	require.NoError(t, err)
	require.Contains(t, viaHost.Output, "x")

	for _, testCase := range []struct {
		name, program string
		environment   map[string]string
		want          string
	}{
		{"a reserved variable", "sh", map[string]string{"PATH": "/evil"}, "cannot be set"},
		{"a name that is no variable", "sh", map[string]string{"A=B": "x"}, "cannot be set"},
		{"a file that is not executable", "./notes.txt", nil, "not an executable file"},
		{"a program outside", "/bin/ls", nil, "outside the working directory"},
		{"a program by name", "ls", nil, "cannot run in the background"},
	} {
		_, err := managed.start(testCase.program, []string{"-c", "true"}, "", testCase.environment)
		require.ErrorContains(t, err, testCase.want, testCase.name)
	}
}

func TestHTTPRequestReachesOnlyTestServersOnThisComputer(t *testing.T) {
	t.Parallel()
	for _, refused := range []string{"https://127.0.0.1:20080/", "http://example.com:20080/", "http://127.0.0.1:8080/api", "http://localhost/", "http://10.0.0.1:20080/"} {
		_, err := localURL(refused)
		require.Error(t, err, refused)
	}
	for _, allowed := range []string{"http://127.0.0.1:20080/health", "http://localhost:29999/", "http://[::1]:40000/x"} {
		_, err := localURL(allowed)
		require.NoError(t, err, allowed)
	}

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", request.Header.Get("Content-Type"))
		_, _ = writer.Write([]byte(request.Method + " " + string(body))) //nolint:gosec // G705: a test echo server
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	port := listener.Addr().(*net.TCPAddr).Port
	if port < minimumRequestPort {
		t.Skipf("the system gave port %d, below the tool's range", port)
	}
	answered, err := send(t.Context(), http.DefaultClient, "POST", fmt.Sprintf("http://127.0.0.1:%d/", port), `{"a":1}`)
	require.NoError(t, err)
	require.Equal(t, httpResponse{Status: 200, ContentType: "application/json", Body: `POST {"a":1}`}, answered)
}
