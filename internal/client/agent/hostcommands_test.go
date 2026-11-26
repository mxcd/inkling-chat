package agent

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheGoCommandRefusesWhatChangesTheUsersSetup(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		arguments []string
		refused   bool
	}{
		{nil, false},
		{[]string{"build", "./..."}, false},
		{[]string{"test", "-run", "TestX", "./..."}, false},
		{[]string{"vet", "./..."}, false},
		{[]string{"run", "."}, false},
		{[]string{"mod", "tidy"}, false},
		{[]string{"get", "golang.org/x/text@latest"}, false},
		{[]string{"list", "-m", "all"}, false},
		{[]string{"fmt", "./..."}, false},
		{[]string{"version"}, false},
		{[]string{"env"}, false},
		{[]string{"env", "GOPATH"}, false},
		{[]string{"doc", "fmt.Println"}, false},
		{[]string{"generate", "./..."}, false},
		{[]string{"env", "-w", "GOPROXY=direct"}, true},
		{[]string{"env", "--w=GOFLAGS=-mod=mod"}, true},
		{[]string{"env", "-u", "GOPROXY"}, true},
		{[]string{"install", "golang.org/x/tools/cmd/stringer@latest"}, true},
		{[]string{"clean", "-modcache"}, true},
		{[]string{"clean", "-cache"}, true},
		{[]string{"telemetry", "on"}, true},
		{[]string{"work", "use", "../other"}, true},
		{[]string{"-C", "/etc", "build"}, true},
	} {
		t.Run(strings.Join(testCase.arguments, " "), func(t *testing.T) {
			t.Parallel()
			err := allowGo(testCase.arguments)
			if testCase.refused {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestTheBunCommandRefusesWhatChangesTheUsersSetup(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		arguments []string
		refused   bool
	}{
		{nil, false},
		{[]string{"install"}, false},
		{[]string{"add", "zod"}, false},
		{[]string{"remove", "zod"}, false},
		{[]string{"run", "build"}, false},
		{[]string{"test"}, false},
		{[]string{"x", "prettier", "--check", "."}, false},
		{[]string{"pm", "ls"}, false},
		{[]string{"add", "-g", "typescript"}, true},
		{[]string{"install", "--global", "typescript"}, true},
		{[]string{"upgrade"}, true},
		{[]string{"link"}, true},
		{[]string{"unlink"}, true},
		{[]string{"pm", "cache", "rm"}, true},
	} {
		t.Run(strings.Join(testCase.arguments, " "), func(t *testing.T) {
			t.Parallel()
			err := allowBun(testCase.arguments)
			if testCase.refused {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestFindGoGivesTheHostsCachesAndAMinimalEnvironment(t *testing.T) {
	t.Parallel()
	binary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go binary")
	}
	found, err := FindGo(t.Context(), "")
	require.NoError(t, err)

	absolute, err := filepath.Abs(binary)
	require.NoError(t, err)
	require.Equal(t, absolute, found.Path)
	names := make([]string, 0, len(found.Environment))
	for _, variable := range found.Environment {
		name, _, _ := strings.Cut(variable, "=")
		names = append(names, name)
	}
	require.Subset(t, []string{"HOME", "PATH", "TMPDIR", "GOPATH", "GOCACHE", "GOMODCACHE", "GOPROXY"}, names, "nothing else is passed")
	require.Subset(t, names, []string{"HOME", "PATH", "TMPDIR", "GOCACHE", "GOMODCACHE"})
	require.Contains(t, found.Environment, "PATH="+filepath.Dir(absolute)+":"+systemPath)
	require.Error(t, found.Allow([]string{"install"}))
}

func TestAMissingHostCommandIsAnError(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "nothing")
	_, err := FindGo(t.Context(), missing)
	require.Error(t, err)
	_, err = FindBun(missing)
	require.Error(t, err)
}

func TestFindBunNamesTheHostsBun(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("no bun binary")
	}
	found, err := FindBun("")
	require.NoError(t, err)
	require.Equal(t, BunCommandName, found.Name)
	require.Error(t, found.Allow([]string{"add", "-g", "x"}))
}
