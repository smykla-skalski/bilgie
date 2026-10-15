# bilgie

Developer cache manager for macOS, Linux and Windows with configurable size limits. Monitors and cleans caches for Go, npm, Docker, Homebrew, and other dev tools.

## Project Structure

```
cmd/bilgie/           - CLI entrypoint
internal/
  appname/            - Single source of the project name (Name)
  cache/              - Cache size scanning
  auto/               - Free-space tiers, tick decision (cooldowns, hysteresis, forecast), auto run, per-OS scheduler (launchd, systemd/cron, schtasks), notifiers, first-run marker (Arm and Disarm flip it under the run lock)
  cli/                - Cobra command implementations (status, clean, config, tick, auto, history, doctor, discover, install-agent, arm, disarm)
  discover/           - Read-only suggestions of the providers a machine needs: tiers (regenerable, opt-in, protected), unmanaged-directory entries
  report/             - Concise per-provider summaries (text and JSON summary objects) for clean and auto
  doctor/             - Pure health checks behind `bilgie doctor` (agent, last run, free-space trend, swap and memory, config, notifier)
  config/             - Config loading, defaults, validation
  fsx/                - Reads of user-controlled paths that never block on a FIFO, socket or device
  osshim/             - Per-OS shims (lock, process list, kill tree, open files, free space, memory and swap)
  provider/           - Provider interface + implementations (see Provider Types)
pkg/size/             - Human-readable size parsing/formatting
```

Names derived from the project name (binary, config and state dirs, launchd label, systemd unit,
cron tag, Task Scheduler task, notifier title) come from `appname.Name`; never hardcode the name elsewhere.

## Tech Stack

**Language:** Go 1.26 source (`go` directive); 1.27.2 toolchain (`go.mod`, `.mise.toml`)
**CLI Framework:** Cobra + Viper
**Terminal UI:** Lipgloss (tables, styling)
**Testing:** testify (assert/require)
**Linting:** golangci-lint 2.x, linter set in `.golangci.yml`
**Dependency Mgmt:** mise

## Development Workflow

### Adding New Command

1. Create `internal/cli/{command}.go` with Cobra command
2. Add flags in `init()` function
3. Create `run{Command}WithLoader()` for testability
4. Register in `cmd/bilgie/main.go`
5. Add tests in `internal/cli/{command}_test.go`
6. Test: `go run ./cmd/bilgie {command} --help`

### Adding New Provider

1. Add default config in `internal/config/defaults.go`; spell cache-root paths with `Platform` helpers (`p.cache`, `p.data`) and list OS-specific providers in `providerOSes` (`internal/config/platform.go`)
2. For command-based providers: set `clean_cmd` field
3. For file-based providers (no CLI): add to `fileBasedProviders` in `registry.go`; for caches with trees that are only valid whole (installed packages, extracted crates, bundles) add a `treeSpecs` entry in `tree.go` instead, so smart clean never deletes inside a tree
4. Test size calculation and clean operation
5. Verify `Available()` returns correct result

Command-based provider pattern (most common):
```go
// In defaults.go
"tool-name": {
    Enabled:  true,
    Paths:    []string{p.cache("tool-name")},
    MaxSize:  "5G",
    CleanCmd: "tool-name clean-cache",
},
```

### Running Tests

```bash
go test ./...                       # all tests
go test ./internal/cli/...          # CLI tests only
go test -run TestScanProvider ./... # specific test
go test -v -race ./...              # with race detection
```

`TestRunHistory_ReadsStateDirUnderHome` fails locally when `GOTMPDIR` is exported as an uncleaned path (for example `//` from `mktemp -d "$TMPDIR/..."` with a trailing-slash `TMPDIR`): `t.TempDir()` keeps the `//`, the state dir is cleaned, and the prefix check breaks. Unset `GOTMPDIR` or export a cleaned absolute path.

## Architecture

### Provider Types

`NewProvider` in `internal/provider/registry.go` picks the type; a name it does not special-case needs `clean_cmd` (Command).

| Type | Clean Method | Example |
|------|--------------|---------|
| Command | Full mode runs `clean_cmd` (bounded by `clean_timeout`, default 2m); smart mode trims files by `max_age`, then `max_size` | go-build, homebrew |
| File | Deletes files directly; `skip_if_open` leaves paths with open files alone | gh, app-updaters, copilot, golangci-lint, node-gyp |
| Tree | Deletes whole trees only (`treeSpecs` in `tree.go`), never inside one | npm, cargo, yarn, gradle, go-mod, xcode-* |
| Entry | Evicts whole top-level entries, oldest first, to `max_size` only; `skip_prefixes` protects names | huggingface, playwright, lima, chrome-devtools-mcp |
| Docker | Docker prune, skipped when the daemon is down; `docker-volumes` is a separate opt-in | docker, docker-volumes |
| Dir pattern | `type: dir-pattern`: removes stale directories matching globs after `min_idle`; skips matches with open files, `.git`, or (`owner_marker`) no owner marker | sail-dirs, agent-scratch-dirs, sail-e2e-dirs |
| Project artifacts | `type: project-artifacts`: removes build artifacts of idle projects under the project roots | project-artifacts |
| Custom | Own implementation | mise (`mise prune`), uv (`uv cache clean`), rustup, jetbrains, sqlite-compact (VACUUM of listed databases), codex-sessions (rollouts older than `max_age`, ignores `max_size`) |

Disabled by default (opt-in): dir-pattern providers, uv, sqlite-compact, codex-sessions, rustup, huggingface, playwright, chrome-devtools-mcp, docker-volumes.

### Auto Tiers and Budgets

- Pressure tiers (`internal/auto/tier.go`): ok (trim only providers over their limit), low (trim all enabled), critical (shorter cooldown; sweeps disabled dir-pattern providers with `sweep_tier: critical`, default sail-dirs), emergency (sweeps all stale directories).
- `sweep_tier` (`critical` or `emergency`, default `emergency`) is valid only on dir-pattern providers.
- `uvx_min_age` (uv only, default 1h): a running uvx process older than this stops blocking uv; only environments no running process uses are swept.
- Per-provider run budget (`auto.ProviderBudget`, 10m, not a config key): a provider that outlives it is reported as timed out and the pass goes on.
- project-artifacts has its own `scan_budget` (default 10s) and `pass_budget` (default 30s).

### Provider Interface

```go
type Provider interface {
    Name() string
    Paths() []string
    CurrentSize(ctx context.Context) (int64, error)
    MaxSize() int64
    MaxAge() time.Duration
    Clean(ctx context.Context, opts CleanOptions) (CleanResult, error)
    Available() bool
}
```

### Config Structure

Location: `~/.config/bilgie/config.yaml`

```yaml
version: "1"
providers:
  go-build:
    enabled: true
    paths:
      - ~/Library/Caches/go-build
    max_size: 10G
    clean_cmd: go clean -cache
```

Loading order: config file > defaults (merged)

## Output Formats

### Status Command

**Table output (default):**
```
┌──────────────┬─────────┬─────────┬────────┐
│ Provider     │ Current │ Max     │ Status │
├──────────────┼─────────┼─────────┼────────┤
│ go-build     │ 2.1 GiB │ 10 GiB  │ ok     │
│ npm          │ 4.5 GiB │ 3.0 GiB │ OVER   │
└──────────────┴─────────┴─────────┴────────┘

Total: 6.6 GiB
```

**JSON output (`--json`):**
```json
{
  "total": "6.6 GiB",
  "total_bytes": 7088545792,
  "providers": [
    {
      "name": "go-build",
      "current": "2.1 GiB",
      "current_bytes": 2254857830,
      "max": "10 GiB",
      "max_bytes": 10737418240,
      "over_limit": false
    }
  ]
}
```

### Clean Command

```bash
bilgie clean go-build npm  # specific providers
bilgie clean --all         # all enabled
bilgie clean --dry-run     # preview only
bilgie clean --force       # skip confirmation
bilgie clean --quiet       # minimal output
```

### Arm and Disarm

```bash
bilgie arm     # write the first-run marker: the next real pass only previews
bilgie disarm  # clear it: the next real pass deletes
```

`install-agent` arms it again. Both commands take the run lock first and fail while a pass runs;
`doctor` and `status` (`agent_mode` in `--json`) show the mode. Never auto-disarm.

## Quality Gates

Before committing:

- [ ] `mise run lint` passes
- [ ] `go test -race ./...` passes
- [ ] `go build ./cmd/bilgie` succeeds
- [ ] `mise run vuln` (govulncheck) reports no vulnerabilities
- [ ] Manual test of changed commands

```bash
mise run lint   # or: mise exec -- golangci-lint run
go test -race ./...
go build -o bilgie ./cmd/bilgie
mise run vuln
```

Bare `golangci-lint run` fails: the binary on PATH can be built with an older Go than the toolchain `go.mod` pins ("Go language version used to build golangci-lint is lower than the targeted Go version"). Tasks in `.mise.toml` run with the pinned tools.

## Common Commands

```bash
# Build and run
go build -o bilgie ./cmd/bilgie
./bilgie status
./bilgie status --json

# Development
go run ./cmd/bilgie status
go run ./cmd/bilgie clean --dry-run --all

# Testing
go test ./...
go test -v -run TestClean ./internal/cli/...
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Linting
mise run lint
mise exec -- golangci-lint run --fix
```

## Testing Patterns

### Command Tests

Use `WithLoader` pattern for dependency injection:
```go
func TestCleanCmd(t *testing.T) {
    tmpDir := t.TempDir()
    cfgPath := filepath.Join(tmpDir, "config.yaml")
    // write test config...

    loader := config.NewLoader()
    loader.SetConfigPath(cfgPath)

    err := runCleanWithLoader(loader, args, flags...)
    require.NoError(t, err)
}
```

### Stdout Capture

```go
func captureStdout(t *testing.T, fn func()) string {
    old := os.Stdout
    r, w, _ := os.Pipe()
    os.Stdout = w
    fn()
    w.Close()
    os.Stdout = old
    var buf bytes.Buffer
    io.Copy(&buf, r)
    return buf.String()
}
```

## Anti-Patterns

**AVOID:**

- Skipping `Available()` check before cleaning (docker may not be running)
- Hardcoding paths without `~` expansion (use `config.ExpandPaths`)
- Forgetting to test both table and JSON output
- Adding providers without integration tests
- Using `os.Exit` directly (return errors instead)
- Silent failures in provider Clean methods

**REASON:** Providers interact with external tools and filesystems. Always verify availability and handle errors gracefully for good UX.

## Size Format Reference

Parsing accepts: `5G`, `5GB`, `5GiB`, `500M`, `500MB`, `500MiB`, `100K`, `100B`

Output uses binary units: `GiB`, `MiB`, `KiB`, `B`

## Error Handling

Errors bubble up through commands and display to stderr:
```
Error: load config: open ~/.config/bilgie/config.yaml: no such file or directory
```

Exit codes:
- 0: Success
- 1: General error (config, provider, filesystem)
