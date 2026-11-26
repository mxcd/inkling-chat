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
)

// systemPath is what a host command's PATH holds after the binary's own directory: the system's
// tools (cgo's compiler, git for modules, sh for package scripts), nothing a user installed.
const systemPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// The names of the host commands the shell may offer.
const (
	GoCommandName  = "go"
	BunCommandName = "bun"
)

// goCaches are the settings of the host's go the model's go reuses, so its builds share the
// module and build caches and fetch modules where the host does.
var goCaches = []string{"GOPATH", "GOCACHE", "GOMODCACHE", "GOPROXY"}

// HostCommand is a host program the shell offers: its name in scripts, its binary, the complete
// environment it runs with, what it refuses and the line the shell's description shows. It runs
// in the shell's current directory.
type HostCommand struct {
	Name        string
	Path        string
	Environment []string
	Allow       func(arguments []string) error
	Summary     string
}

// FindGo finds the go binary, binary or the go in PATH when empty, and asks it for its caches.
// The environment is explicit and minimal: HOME, PATH (the binary's directory and the system's),
// TMPDIR and the caches. It fails when there is no go binary or it does not answer.
func FindGo(ctx context.Context, binary string) (*HostCommand, error) {
	found, environment, err := hostBinary(GoCommandName, binary)
	if err != nil {
		return nil, err
	}
	//nolint:gosec // G204: the operator's configured go binary
	answer, err := exec.CommandContext(ctx, found, append([]string{"env", "-json"}, goCaches...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("go command: %s env: %w", found, err)
	}
	var settings map[string]string
	if err := json.Unmarshal(answer, &settings); err != nil {
		return nil, fmt.Errorf("go command: %s env: %w", found, err)
	}
	for _, name := range goCaches {
		if value := settings[name]; value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return &HostCommand{
		Name: GoCommandName, Path: found, Environment: environment, Allow: allowGo,
		Summary: "the host's Go toolchain in the current directory (build, test, vet, run, mod, get, list, fmt, version, env, doc, generate; not install, clean, telemetry, work, env -w or env -u)",
	}, nil
}

// FindBun finds the bun binary, binary or the bun in PATH when empty. Its environment is HOME,
// PATH (the binary's directory and the system's), TMPDIR and the host's BUN_INSTALL when set, so
// it shares the host's package cache. It fails when there is no bun binary.
func FindBun(binary string) (*HostCommand, error) {
	found, environment, err := hostBinary(BunCommandName, binary)
	if err != nil {
		return nil, err
	}
	if installed := os.Getenv("BUN_INSTALL"); installed != "" {
		environment = append(environment, "BUN_INSTALL="+installed)
	}
	return &HostCommand{
		Name: BunCommandName, Path: found, Environment: environment, Allow: allowBun,
		Summary: "the host's Bun in the current directory (install, add, remove, run, test, build, x, init, pm; nothing global: no -g, link, unlink, upgrade or pm cache rm)",
	}, nil
}

// hostBinary is the absolute path of binary, or of name in PATH when binary is empty, and the
// environment every host command starts with.
func hostBinary(name, binary string) (string, []string, error) {
	if binary == "" {
		binary = name
	}
	found, err := exec.LookPath(binary)
	if err != nil {
		return "", nil, fmt.Errorf("%s command: %w", name, err)
	}
	found, err = filepath.Abs(found)
	if err != nil {
		return "", nil, fmt.Errorf("%s command: %w", name, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil, fmt.Errorf("%s command: %w", name, err)
	}
	return found, []string{"HOME=" + home, "PATH=" + filepath.Dir(found) + ":" + systemPath, "TMPDIR=" + os.TempDir()}, nil
}

// goRefusedCommands are the go subcommands that change the user's setup outside the directory:
// installed binaries, the caches, telemetry and go.work files, which may lie in a parent directory.
var goRefusedCommands = map[string]string{
	"install":   "go install writes to the user's GOBIN; use go build -o or go run",
	"clean":     "go clean can empty the user's module and build caches",
	"telemetry": "go telemetry changes the user's settings",
	"work":      "go work can change a go.work file outside the directory",
}

// allowGo is the go command's Allow: it refuses what changes the user's global setup and lets
// everything else run (build, test, vet, run, mod, get, list, fmt, version, env, doc, generate).
func allowGo(arguments []string) error {
	if len(arguments) == 0 {
		return nil
	}
	command := arguments[0]
	if strings.HasPrefix(command, "-") {
		return errors.New("flags before the subcommand (such as -C) are not allowed; cd to the directory instead")
	}
	if reason, refused := goRefusedCommands[command]; refused {
		return fmt.Errorf("refused: %s", reason)
	}
	if command == "env" && slices.ContainsFunc(arguments[1:], isFlag("w", "u")) {
		return errors.New("refused: go env -w and -u change the user's settings")
	}
	return nil
}

// bunRefusedCommands are the bun subcommands that change the user's setup outside the directory.
var bunRefusedCommands = map[string]string{
	"upgrade": "bun upgrade replaces the user's bun",
	"link":    "bun link changes the user's global links",
	"unlink":  "bun unlink changes the user's global links",
}

// allowBun is the bun command's Allow: it refuses global installs and links, upgrading bun and
// emptying the user's package cache, and lets everything else run.
func allowBun(arguments []string) error {
	if slices.ContainsFunc(arguments, isFlag("g", "global")) {
		return errors.New("refused: -g and --global change the user's global packages; add the package to the project instead")
	}
	if len(arguments) == 0 {
		return nil
	}
	if reason, refused := bunRefusedCommands[arguments[0]]; refused {
		return fmt.Errorf("refused: %s", reason)
	}
	if arguments[0] == "pm" && slices.Contains(arguments, "cache") && slices.ContainsFunc(arguments, func(argument string) bool { return argument == "rm" || argument == "clean" }) {
		return errors.New("refused: bun pm cache rm empties the user's package cache")
	}
	return nil
}

// isFlag says whether an argument is one of the flags names, in any spelling the flag parsers
// take (-w, --w, -w=x).
func isFlag(names ...string) func(argument string) bool {
	return func(argument string) bool {
		name, _, _ := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		return strings.HasPrefix(argument, "-") && slices.Contains(names, name)
	}
}
