set shell := ["bash", "-euo", "pipefail", "-c"]

# inkling-chat, from the govengers service template (govengers/standards templates/service). The service has no
# database, no generated code and no API clients, so the template's generate, db and openapi recipes are left out.
# Tools are pinned in go.mod (TCH-002): go get -tool gotest.tools/gotestsum@v1.13.0 golang.org/x/vuln/cmd/govulncheck@v1.9.0

image := "ghcr.io/mxcd/inkling-chat"
version := `git describe --tags --always --dirty 2>/dev/null || echo dev`
gotestsum := "go tool gotestsum"
coverage_threshold := "80"
coverage_excluded := "/cmd/|/internal/modeltest/|/test/"
# Per-test budget (TST-017)
test_budget := "5s"
e2e_inputs := "test/e2e"
# The local model server: the Strata install with the patched Inkling engine (start, stop, status)
model_server := env("INKLING_FAST", home_directory() / "models/strata-0.2.20/inkling-fast")

default:
    @just --list

# Vet and lint with the govengers baseline (.golangci.yml, copied from govengers/standards)
lint:
    go vet ./...
    go vet -tags e2e ./test/e2e/...
    golangci-lint run ./...

# Unit tests of changed and affected packages; the Go test cache replays the rest (TST-014). Fails below the
# coverage threshold (TST-010) and when a test took longer than the budget (TST-017)
test *flags:
    mkdir -p sandbox
    {{gotestsum}} --format testname --jsonfile sandbox/unit.json -- -race -coverprofile=sandbox/unit.raw.cover {{flags}} ./...
    grep -v -E '{{coverage_excluded}}' sandbox/unit.raw.cover > sandbox/unit.cover
    go tool cover -func=sandbox/unit.cover > sandbox/unit.func
    awk -v threshold={{coverage_threshold}} '/^total:/ { found = 1; sub("%", "", $3); print "coverage " $3 "%"; if ($3 + 0 < threshold) { print "coverage is below " threshold "%"; exit 1 } } END { if (!found) { print "no coverage total"; exit 1 } }' sandbox/unit.func
    "{{just_executable()}}" test-budget sandbox/unit.json

# Fail when a test in the go test JSON took longer than the budget, also tests replayed from the cache (TST-017).
# On a failure it drops the test cache: a replayed result keeps its slow duration, so the next run measures again
test-budget report:
    #!/usr/bin/env bash
    set -euo pipefail
    slow="$({{gotestsum}} tool slowest --jsonfile {{report}} --threshold {{test_budget}})"
    if [ -n "$slow" ]; then
        printf 'tests over the %s budget (TST-017):\n%s\n' "{{test_budget}}" "$slow"
        go clean -testcache
        exit 1
    fi

# Every unit test, uncached and shuffled: the nightly run and before a release (TST-014, TST-005)
test-all: (test "-count=1" "-shuffle=on")

# Reproducible, coverage-instrumented server with the built frontend and the compiled suite: their hash says whether
# the suite has anything new to test (TST-015)
e2e-build: ui-build
    mkdir -p sandbox/e2e-build
    go build -cover -trimpath -buildvcs=false -ldflags "-X main.version=e2e" -o sandbox/e2e-build/server ./cmd/server
    go test -c -tags e2e -trimpath -buildvcs=false -o sandbox/e2e-build/ ./test/e2e/...

# The end-to-end suite against the built server, always and shuffled (TST-011, TST-005); the nightly run calls it
test-e2e-all *flags: e2e-build
    rm -rf sandbox/coverage && mkdir -p sandbox/coverage
    E2E_BINARY={{justfile_directory()}}/sandbox/e2e-build/server GOCOVERDIR={{justfile_directory()}}/sandbox/coverage {{gotestsum}} --format testname --jsonfile sandbox/e2e.json -- -tags e2e -trimpath -buildvcs=false -count=1 -shuffle=on {{flags}} ./test/e2e/...
    go tool covdata percent -i=sandbox/coverage
    "{{just_executable()}}" test-budget sandbox/e2e.json

# The end-to-end suite, skipped when server, suite and inputs equal a green run (TST-015)
test-e2e: e2e-build
    #!/usr/bin/env bash
    set -euo pipefail
    key=$(find sandbox/e2e-build {{e2e_inputs}} -type f -print0 | sort -z | xargs -0 shasum -a 256 | shasum -a 256 | cut -c1-32)
    passed="sandbox/e2e-passed/$key"
    if [ -f "$passed/e2e.json" ]; then
        echo "e2e skipped: server, suite and inputs equal the green run $key"
        cp "$passed/e2e.json" sandbox/e2e.json
        exit 0
    fi
    "{{just_executable()}}" test-e2e-all
    mkdir -p "$passed"
    cp sandbox/e2e.json "$passed/e2e.json"

# Scan for known vulnerabilities in reachable code (TCH-008)
vulncheck:
    go tool govulncheck ./...

# Build the server, frontend embedded, with the version from git (DEP-005)
build: ui-build
    go build -trimpath -ldflags "-s -w -X main.version={{version}}" -o sandbox/server ./cmd/server

# Build the image like CI does (DEP-002)
docker-build:
    docker build -f docker/server.Dockerfile --build-arg VERSION={{version}} -t {{image}}:{{version}} .

# ---- running it ----

# Backend and frontend for development: open the frontend's dev server (http://localhost:5173), which proxies the
# API to the Go server (DEP-001). The model server must run: just model-start
dev: ui-install
    #!/usr/bin/env bash
    set -euo pipefail
    trap 'kill 0' EXIT
    DEV_MODE=true READINESS_DELAY=0s go run ./cmd/server &
    cd ui && bun run dev

# The single binary with the embedded frontend on http://127.0.0.1:8080. The model server must run: just model-start
serve: build
    DEV_MODE=true READINESS_DELAY=0s sandbox/server

# Start the local model server (pack sq-20: lossless, sq-10: smaller and faster on prose); stop it with model-stop
model-start pack="sq-20":
    "{{model_server}}" start {{pack}}

model-stop:
    "{{model_server}}" stop

# ---- frontend (ui/) ----

# The frontend's dependencies, as bun.lock pins them
ui-install:
    cd ui && bun install --frozen-lockfile

# The frontend built into internal/web/html/dist, where the binary embeds it
ui-build: ui-install
    cd ui && bun run build

# The frontend's type checks, tests and build; CI's TypeScript job
test-typescript: ui-install
    cd ui && bun run typecheck
    cd ui && bun test
    cd ui && bun run build

# Everything CI runs on a change, in one go
check: lint test test-e2e test-typescript vulncheck
