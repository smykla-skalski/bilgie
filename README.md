# bilgie

A bilge pump for developer machines. Caches pile up quietly in the hull of your disk until
something floods; bilgie works like the float switch that guards a boat's bilge:

- **Idle** while free space stays above the level you set.
- **Pump** as soon as it drops below: caches are trimmed, cheapest to rebuild first, until the water level is back.
- **Sweep hard** at the critical level: stale directories go too.

Interactive TUI, 27 built-in providers for macOS, Linux and Windows, auto-discovery, smart LRU cleaning and an
unattended `auto` mode that runs from the OS scheduler.

![demo](./doc/demo.gif)
<!-- Generate with: brew install vhs && vhs doc/demo.tape -->

## Install

### Homebrew

```bash
brew install smykla-skalski/tap/bilgie
```

### Go Install

```bash
go install github.com/smykla-skalski/bilgie/cmd/bilgie@latest
```

### Binary Download

Download the archive for your platform from [releases](https://github.com/smykla-skalski/bilgie/releases):

| OS | Architectures | Archive |
|----|---------------|---------|
| macOS | amd64, arm64 | `.tar.gz` |
| Linux | amd64, arm64 | `.tar.gz` |
| Windows | amd64, arm64 | `.zip` |

Extract it and put `bilgie` (`bilgie.exe` on Windows) on your `PATH`.

## Quick Start

```bash
# Launch interactive TUI (default)
bilgie

# Check all cache sizes
bilgie status
```

## Interactive Mode

Running `bilgie` with no arguments launches a full-screen TUI built with [Bubble Tea](https://github.com/charmbracelet/bubbletea).

Providers are scanned in parallel with live size updates. Select what to clean, confirm, and watch progress — all without leaving the terminal.

### Keyboard Shortcuts

#### Selection Screen

| Key | Action |
|-----|--------|
| `j` / `k` | Move cursor up/down |
| `space` | Toggle provider |
| `a` | Select all |
| `n` | Select none |
| `o` | Select over-limit only |
| `enter` | Confirm selection |
| `q` / `esc` | Quit |

#### Confirmation Popup

| Key | Action |
|-----|--------|
| `y` | Start cleaning |
| `s` | Switch to smart mode |
| `f` | Switch to full mode |
| `n` / `esc` | Back to selection |

### TUI Flow

```
Selection → Confirmation → Cleaning (with progress bar) → Done (summary)
```

### Flags

```bash
bilgie --dry-run     # Preview without deleting
bilgie --full        # Use full clean instead of smart (default)
```

## Providers

Providers are auto-detected — only tools installed on your system appear in the TUI and status output. Unavailable providers are dimmed.

| Provider | Default Limit | Clean Method |
|----------|---------------|--------------|
| **Go** | | |
| go-build | 10G | `go clean -cache` |
| go-mod | 5G | smart: whole `module@version` directories, never `cache/download`; full: `go clean -modcache` |
| **JavaScript** | | |
| npm | 3G | smart: `_cacache` file by file (then `npm cache verify`), each `_npx/<hash>` whole; full: `npm cache clean --force` |
| yarn | 2G | smart: whole `v*/<package>` directories; full: `yarn cache clean` |
| pnpm | 5G | smart: store files (content-addressed); full: `pnpm store prune` |
| **Python** | | |
| uv | 4G | opt-in (`enabled: false`); smart: `uv cache prune`; full: `uv cache clean`; see [uv](#uv) |
| pip | 3G | `pip cache purge` |
| **Rust** | | |
| cargo | 5G | `registry/cache` `.crate` files by age, whole `registry/src/<index>/<crate>`, `git/checkouts/*/*` and `git/db/*` directories; `registry/index` untouched |
| **Java** | | |
| gradle | 10G | whole top-level `caches/*` directories |
| **Apple** | | |
| xcode-deriveddata | 20G | whole project directories |
| xcode-archives | 10G | whole `.xcarchive` bundles |
| ios-simulator | 10G | `xcrun simctl delete unavailable` |
| **Tools** | | |
| homebrew | 5G | `brew cleanup -s` |
| mise | 8G | `mise prune` for unused tool versions (listed first via `--dry-run`), `downloads` and cache files by age; see [mise](#mise) |
| docker | 50G | `docker system prune -af` |
| docker-volumes (disabled by default; ignores max_age) | 50G | `docker volume prune -f` |
| jetbrains | 3G | file-based |
| **Browsers** | | |
| edge | 3G | file-based |
| vivaldi | 3G | file-based |
| **ML and test tooling** | | |
| huggingface (disabled by default; `~/.cache/huggingface/hub` only) | 20G | whole-entry (newest kept) |
| playwright (disabled by default) | 5G | whole-entry |
| **Other caches** | | |
| lima | 10G | whole-entry |
| gh | 1G | file-based |
| chrome-devtools-mcp (disabled by default; never evicts `chrome-profile-*`) | 2G | whole-entry |
| vscode-shipit | 1G | file-based |
| app-updaters (`~/Library/Caches/*-updater`, macOS only) | 2G | file-based; skips paths with open files |
| copilot (macOS only) | 1G | file-based; skips paths with open files |
| golangci-lint (macOS only) | 1G | file-based; skips paths with open files |
| node-gyp (macOS only) | 1G | file-based; skips paths with open files |
| sail-e2e-dirs (disabled by default; `~/Library/Caches/dev.smykla.sail.e2e.*`, macOS only) | 5G | whole-directory; idle, open-file and `.git` checks |
| **Project artifacts** | | |
| project-artifacts (Rust `target`, `node_modules`, Python venvs of idle projects) | 20G | whole-directory, oldest project first |
| **Toolchains** | | |
| rustup (disabled by default; once over the limit uninstalls every toolchain except `stable`, default, active and directory-override ones) | 10G | `rustup toolchain uninstall` |
| sqlite-compact (disabled by default; lists SQLite database files, `max_size` is the free-page size that triggers `VACUUM`) | 1G | `VACUUM`; see [sqlite-compact](#sqlite-compact) |

### Project artifacts

`project-artifacts` removes the build output that dominates a developer disk: Rust `target/`
directories (4 to 12 GB each, many in git worktrees) and idle `node_modules`. It scans
project roots, finds artifact directories by marker, and removes whole directories of idle
projects, oldest project first and only as far as needed. Every artifact regenerates with a
build or install.

An artifact counts only when its marker and the project file beside it both exist, so a
directory that merely shares the name is never touched:

| Kind | Artifact | Marker inside | Project file beside it | Default |
|------|----------|---------------|------------------------|---------|
| `rust` | `target/` | `CACHEDIR.TAG` whose first line is cargo's signature | `Cargo.toml` | on |
| `node` | `node_modules/` | none | `package.json` | on |
| `python` | `.venv/`, `venv/` | `pyvenv.cfg` | `pyproject.toml` or `requirements*.txt` | off |

Roots default to whichever of `~/sideprojects`, `~/kong`, `~/work`, `~/src`, `~/code` and
`~/projects` exist. Defaults are not written to the saved config, so one file works on every
machine. The search goes `max_depth` levels below each root (default 4), never follows
symlinks, never enters `.git`, `node_modules`, `target`, `.venv` or `venv` while searching, and
stops after
`scan_budget` (default 10s) or on Ctrl-C. A nested project in a monorepo is found at its own
level. A root that is home, a parent of home or the filesystem root is ignored, and the output
says so: as a warning when other roots were scanned, as the skip reason when none was.

A project is idle when all of these are older than `min_idle` (default 7 days): the newest
mtime among files outside the artifacts and `.git` (a sample of the first 3000 entries, breadth
first; a sample cut short skips the project, with or without git), and the git `HEAD` and reflog
time. A folder that merely shares an artifact's name, such as `src/target/`, is source and is
sampled; only recognised artifacts (valid marker beside the project file) are left out. A
linked worktree is judged by its own `HEAD`. The repository is found by walking up from the
project to the filesystem root, past the configured root, but never into home, so a repository
of dotfiles in home does not own the projects below it. The git time is read again just
before each removal; a commit or checkout made meanwhile keeps the artifact. A directory-only removal never makes a project look
active: the parent keeps its mtime.

An idle project's artifact must also be unused. It is skipped while any of these is newer than
`min_idle`: the mtime of the artifact and of its top-level entries (for Rust also the entries of
`target/{debug,release}` and `target/<triple>/{debug,release}`; for Node `node_modules/.bin`;
for Python the venv's `bin` or `Scripts`), and the atime of the regular files among them, such
as `target/release/<tool>`, `node_modules/.bin/<tool>` (followed to its script) and
`node_modules/.package-lock.json`. This keeps a binary that a cron or launchd job still runs.
atime is only an extra signal: `noatime` mounts never update it and `relatime` updates it at
most daily, so a quiet atime proves nothing and the mtime checks, the running-process check and
the open-file check stay in force. Directory atimes are ignored because listing a directory
refreshes them. This use check covers the top-level entries, `.bin` and the release executables
named above, not a module read deep inside `node_modules`.

An artifact is skipped, with the reason in `--verbose` output, when:

- its project or the artifact lies in a protected location, or under a `Downloads` or
  `opencode` directory. A git worktree is not protected by being a worktree: an idle one is
  cleaned, the worktree itself is never deleted
- the project is not idle yet
- the git tree is dirty (`git status --porcelain` is non-empty, or git is missing or fails).
  Set `skip_if_dirty: false` to turn this off
- a tool of that kind runs in the project: `cargo` or `rustc` for Rust, `node`, `npm`, `npx`,
  `pnpm`, `yarn` or `bun` for Node, `python`, `pip`, `uv` or `poetry` for Python (versioned and alternative interpreters such as
  `python3.12`, `pypy3`, `ipython` and the `py` launcher count too). A process
  counts when its command line names the project, its working directory is inside it, or a
  relative path in its arguments, or the value of `--manifest-path`, `--prefix`, `--cwd` or
  `-C`, `-w` or `--workspace`, resolves to a place inside it (`cd ~/code && node app/server.js`,
  `npm --prefix app start`, `node app`). Dev servers such as `vite`, `next-server`, `tsx`
  and `nodemon` count as Node tools. A tool whose working directory is above the project, inside the scan
  root, holds it unless an argument points elsewhere (`cd ~/code && cargo build` holds every Cargo
  project below; `--manifest-path other/Cargo.toml` holds only `other`). A process
  whose working directory cannot be read counts as busy, and the skip reason names the tool and
  pid (always on Windows, where only image names are visible)
- a process has an open file in it (`lsof`, one listing for the whole pass, or `/proc` on Linux;
  `skip_if_open: false` turns this off). The listing is read again right before each removal
  when it is older than 250ms. Windows
  cannot list handles, but refuses to rename a directory that has open files, so the rename is
  the check there

Removal renames the directory aside (`.bilgie-trash-<kind>-<8 characters>` in the same folder)
and then deletes it, so a crash leaves either the intact artifact or a trash directory that no
marker check accepts, never a half-deleted `target`. The next run sweeps leftover trash, but only
names of exactly that shape inside a project folder; any other `.bilgie-trash-*` directory is
yours and stays. If the delete fails after the rename, the bytes that did go are counted and the
rest is swept next time. A file edited while artifacts are being removed blocks the project's
remaining artifacts. A trash directory that cannot be swept is reported as a warning with its
path in text, JSON and the `auto` run log.

One pass is bounded by `pass_budget` (default `30s`): candidates still waiting when it runs out
are skipped with the reason `pass time budget`, so a slow disk never turns a clean into an
endless one. Names such as `Target` are the same directory as `target` on macOS and Windows and
are matched there; on case-sensitive volumes names are exact. A `.git` entry is matched in any
case on every OS, which only makes the dirty and idleness checks stricter.

`clean project-artifacts` removes every eligible artifact (full mode). `clean --smart` and
`auto` remove only until the artifacts are under `max_size`; `auto` under low-space pressure
instead stops as soon as free space is back above the floors. `--dry-run` lists what would go;
by default the five largest, with kind, project and idle days, and `--verbose` lists all of them
and every skip. When every artifact is skipped, the provider shows as skipped with the first
reason. `status` shows the size of all artifacts and how much of it is recoverable now
(the clean checks except the slow open-file probe).

Under disk pressure the idle threshold shortens: at the critical tier the effective `min_idle`
is `critical_min_idle` (default `2d`) and at the emergency tier `emergency_min_idle` (default
`1d`). A configured `min_idle` that is already shorter is kept, and the ok and low tiers use
`min_idle` unchanged. Set `tier_scaling: false` to keep `min_idle` at every tier. The dirty-tree,
running-process, open-file, recent-artifact-use and protected-path checks apply at every tier.
Skip reasons and the `--dry-run` and `--verbose` entries show the threshold that applied.

```yaml
providers:
  project-artifacts:
    paths: [~/sideprojects, ~/work]
    max_size: 20G
    min_idle: 7d
    max_depth: 4
    scan_budget: 10s
    pass_budget: 30s
    rust: true
    node: true
    python: false
    skip_if_dirty: true
    skip_if_open: true
```

## Commands

### status

```bash
bilgie status          # Table output
bilgie status --json   # JSON output
bilgie status --unmanaged 20  # List more unmanaged directories (0 turns the scan off)
```

After the provider table, `status` lists the largest directories (100 MiB and up) that no
provider covers. It looks at the top-level directories of `~/.local/share`, `~/Library/Caches`
(`~/.cache` off macOS), the OS temp dir and `/private/tmp`. A directory counts as covered when
an enabled provider, or a `dir-pattern` sweep, points at it, into it or at a parent of it. The
scan measures top-level totals only, runs for at most 10 seconds and stops on Ctrl-C; when it
stops early the sizes are lower bounds and `status` says so. `--json` adds an `unmanaged` object.

`status` then lists protected data that exists, with sizes, under "Needs a human (protected, never
auto-deleted)": the `protected` list, `worktrees` directories and git checkouts (any directory holding a `.git` entry) up to four levels below home (hidden directories, `node_modules`, Library, Documents, Desktop, Pictures, Movies and Music are not searched; a checkout is listed once, not searched further), the Docker Desktop VM data that holds Docker volumes (`~/Library/Containers/com.docker.docker/Data/vms` on macOS, `~/.docker/desktop/vms` on Linux), and the other paths auto always skips, such as Xcode archives. Sizes are
measured with the same kind of bounded scan (10 second budget, Ctrl-C stops it, partial sizes are marked
"at least"). `--json` adds a `protected` object with `entries` and `incomplete`.

### clean

```bash
bilgie clean go-build npm  # Specific providers
bilgie clean --all         # All enabled (not xcode-archives; name it to clean it)
bilgie clean --dry-run     # Preview only
bilgie clean --force       # Skip confirmation
bilgie clean --smart       # LRU-based trimming
bilgie clean --dry-run --verbose  # List every entry
```

**Clean modes:**
- **Full** (default): Runs native tool commands (e.g., `go clean -cache`) or deletes files directly
- **Smart** (`--smart`): Removes files older than `max_age`, then LRU-trims to `max_size`

### Output

`clean`, `auto` and every dry-run print one block per provider: the action, the bytes, the
number of entries and the five largest entries, then the providers that were skipped with their
reason and a total. A provider that cleans through a tool (`go clean -cache`) has no entry list
and shows the command it would run instead.

```text
$ bilgie auto --dry-run
free 18 GiB of 460 GiB: tier low (dry-run, nothing is deleted)
npm: would free 12 GiB (56012 entries)
  largest:
       212 MiB  /Users/me/.npm/_cacache/content-v2/sha512/4e/9a/f3c1d0
       198 MiB  /Users/me/.npm/_cacache/content-v2/sha512/b1/07/22ae94
       171 MiB  /Users/me/.npm/_cacache/content-v2/sha512/0c/d8/7745be
       160 MiB  /Users/me/.npm/_cacache/content-v2/sha512/e9/31/a0c2d7
       155 MiB  /Users/me/.npm/_cacache/content-v2/sha512/73/5f/918b06
    ... and 56007 more (--verbose lists all)
go-build: would free 3.1 GiB
  would run: go clean -cache
skipped (2):
  docker: unavailable
  gradle: within limit
total: would free 15 GiB across 2 providers, 56012 entries; 2 skipped
done: free 18 GiB
```

`--verbose` prints every entry (`would delete: <path> (<size>)`) instead of the summary.
`--json` keeps its fields and adds a `summary` object to each provider (`action`, `entries`,
`bytes`, `top` with `path` and `size_bytes`, and `skipped_entries` for directory-pattern providers)
and to the document (`providers`, `skipped`, `errors`, `entries`, `bytes`).

### tick, auto and the agent

```bash
bilgie tick             # Cheap free-space check; starts a pass only when one is needed
bilgie auto             # Run one full pass now (tier from free space)
bilgie auto --dry-run   # Preview only
bilgie auto --verbose   # List every entry
bilgie install-agent    # Run tick every auto.tick_interval with the OS scheduler
bilgie uninstall-agent  # Unload and remove everything install-agent created
```

The scheduled agent runs `bilgie tick` every `auto.tick_interval` (default 2m). A tick reads free
space of the data volume with one `statfs` call and decides; it walks no directory, sizes nothing
and starts no provider unless it decides to run a pass. A healthy tick prints nothing
(`bilgie tick --verbose` explains the decision). A tick exits at once when another pass holds the run lock.

| Free space | What a tick does |
|------------|------------------|
| healthy | nothing, except a routine pass every `auto.interval` (default 30m) that trims providers over their limit |
| low | a full pass, at most every `auto.low_cooldown` (default 10m) |
| critical | the same pass at the shorter `auto.critical_cooldown` (default 2m); providers expected to free the most go first (rebuild cost breaks ties) and the pass stops as soon as space recovers |
| emergency | as critical, with the stale-directory sweeps (`dir-pattern` providers such as `sail-dirs`, even when disabled) first |
| falling | a low-tier pass when the projected free space crosses the low threshold within `auto.forecast` (default 15m) |

Thresholds:

- Low is `max(min_free, min(min_free_pct of the volume, min_free_cap))`. With the defaults
  (`30G`, `5`, `100G`) that is 46 GiB on a 926 GiB disk and never more than 100 GiB, so a large
  disk is not permanently "low". An explicit `min_free` above the cap is kept.
- Critical is below `critical_free` (default 10G) and emergency below `emergency_free` (default 5G);
  neither exceeds the level above it.
- Hysteresis: a tier worsens at once but eases only after free space is `hysteresis` (default 2G)
  above its threshold, and a pass runs until space is that far above the low threshold. Moving
  from low to a worse tier skips the cooldown (never faster than every 30 seconds).
- Forecast: the last 10 readings (kept in `~/.local/state/bilgie/tick.json`, at most 30 minutes old and
  restarted after a gap) give one rate per interval. A pass starts when there are at least 5 readings, the
  median rate falls faster than 50 MiB per minute, and the projection over `auto.forecast` is under the
  low threshold. The median means one sudden drop is never a trend. `forecast: 0` turns it off.
- Notifications: at most one per tier per `auto.notify_cooldown` (default 3h), unless free space
  dropped by more than 10 GiB since that tier's last notification. A tier not yet notified (a worse
  one) is always sent.
- Swap: every tick reads swap in use and keeps it in `tick.json`. While it stays above
  `auto.swap_warn` (default 8G, `0` disables) one desktop notification goes out per
  `auto.notify_cooldown`; it points to `bilgie doctor`. The swap read is bounded to a few seconds and
  never stops the free-space check. A notifier that is not installed is skipped once per cooldown.
  bilgie only reports: it never kills a process.
- State files: `tick.json` and `pass.json` that are corrupt, unreadable or not regular files are moved
  aside to `<name>.bad` with a warning, and the tick carries on; `bilgie doctor` reports them. An
  interrupted or failed pass and the forced first-run dry-run do not start a cooldown.

`auto` still works as before and is a full pass on demand (it also resets the routine-pass timer).
An agent installed by v0.10.0 ran `auto` every 30 minutes; run `bilgie install-agent` once to
replace it with the tick agent (`bilgie doctor` warns while no tick has been recorded).

`auto` reads free space of the data volume (`statfs` of `/System/Volumes/Data` on macOS)
and always trims in smart mode (files older than `max_age`, then LRU to `max_size`):

| Tier | Free space | What runs |
|------|------------|-----------|
| ok | at or above the low threshold | only enabled providers over their limit |
| low | below the low threshold | every enabled provider, cheapest to rebuild first; stops once free space is back above the threshold plus the hysteresis |
| critical | below `critical_free` | the low-tier trim, scheduled at the short cooldown, with the providers expected to free the most first |
| emergency | below `emergency_free` | the stale-directory sweeps first (`dir-pattern` providers such as `sail-dirs`, even when disabled), then the critical-tier order |

Each provider gets its own 10 minute budget inside the 45 minute pass deadline. A provider that exceeds it is reported as timed out with the bytes it freed so far, and the pass moves on to the next provider. The budget is fixed and not configurable.

Safety rules:

- `docker-volumes` never runs in `auto`, enabled or not.
- `xcode-archives` never runs in `auto`, by name or by path (any provider on an Xcode `Archives` folder is skipped): archives hold App Store dSYMs and signed builds.
- Protection is by exact location, never by a directory's name. A provider is skipped when its path is, lies inside, or contains one of the protected roots: `~/Downloads`, the opencode data, config, cache and home directories, Docker volumes and the `protected` list. A directory that is merely named `opencode`, `worktrees` or `Downloads` inside a cache (mise keeps `downloads/opencode`) protects nothing.
- A provider whose path lies inside a git checkout is skipped. Below the path, `auto` looks for a git checkout or worktree by its marker, an entry named `.git` of any kind (a directory, or the file of a linked worktree whose `gitdir:` points into another repository's `worktrees/` directory; the file is never opened, so any `.git` file protects), down to 3 levels below the path. The scan lists directory names only: it never opens files, never follows symlinks, stops at the first hit and gives up after 5 seconds, 50000 directories or 2000000 entries. A checkout buried deeper than 3 levels is not detected by this scan. The skip reason names the checkout that was found. For a `dir-pattern` sweep the check is per match: a protected match is skipped and counted, and the clean matches around it still go. The sweep is skipped whole only when every match is protected. For any other provider one protected entry skips the whole provider.
- Anything the scan cannot verify is skipped, never cleaned: `skipped (too large to verify: <path>)` when a limit is hit, `skipped (scan timed out, cannot verify: <path>)` when the 5 second scan runs out on a slow or network filesystem, `skipped (cannot verify: <dir>)` for an unreadable directory. Both appear in the `auto` output and the run log, and when every provider ends up skipped `auto` says `nothing to clean`.
- Names still matter in two places: the path of a `dir-pattern` sweep of user directories (such as `sail-dirs`) is skipped when it has a `Downloads`, `opencode` or `worktrees` element, and the `status` listing of protected data finds `worktrees` directories and git checkouts near home.
- Docker prune commands are cancelled after 10 minutes; command providers keep their `clean_timeout`.
- Providers whose tool is busy are skipped, as in `clean`.
- Two runs never overlap; a second one exits immediately.
- The first run after `install-agent` is a dry-run that deletes nothing, and a tick never runs a real pass while it is pending. A marker in
  `~/.local/state/bilgie/` records it, so it survives restarts and only a completed
  dry-run below the low threshold, where at least one provider ran, clears it, and only when no provider failed. Running `install-agent` again arms it again.

`install-agent` picks the scheduler of the running OS. Install from a built or installed
binary, not `go run`; the job records the binary path, so run `install-agent` again after
moving it. The first run after install is a dry-run on every OS.

| OS | Scheduler | What is written | Output |
|----|-----------|-----------------|--------|
| macOS | launchd agent, loaded with `launchctl bootstrap` (an existing agent is booted out first) | `~/Library/LaunchAgents/dev.mskalski.bilgie.plist` (runs `tick`, `StartInterval` from `auto.tick_interval`, `RunAtLoad`, low priority, a `PATH` with Homebrew, mise, Go, Cargo and Docker) | `~/Library/Logs/bilgie/auto.log` |
| Linux | systemd user timer | `$XDG_CONFIG_HOME/systemd/user` (default `~/.config/systemd/user`) `bilgie.service` and `bilgie.timer` (runs `tick`; first run a minute after enabling, then `auto.tick_interval` after each run; existing units are rewritten and the timer restarted), enabled with `systemctl --user enable` | `~/.local/state/bilgie/auto.log` |
| Linux without a systemd user manager | cron | one crontab line tagged `# bilgie` running `tick`; other entries are kept and an earlier line is replaced | `~/.local/state/bilgie/auto.log` |
| Windows | Task Scheduler task `bilgie`, created with `schtasks /Create /XML` | `~/.local/state/bilgie/bilgie-task.xml` (runs `tick` through `cmd.exe` so output is appended to the log, repeats every `auto.tick_interval`, below-normal priority, runs only while you are logged on; a console window can flash on each run) | `~/.local/state/bilgie/auto.log` |

Cron fires on fixed minute and hour marks, so the cron fallback only accepts an `auto.tick_interval`
that divides an hour or a day evenly (1m, 2m, 5m, 10m, 15m, 20m, 30m, 1h) and refuses any other. Installing the systemd timer removes an earlier cron entry and the cron fallback removes earlier systemd units.
The systemd user timer runs only while your user manager is up; on a headless box run
`loginctl enable-linger $USER` so it survives logout, otherwise install falls back to cron. Install
prints a note when the timer is installed but linger is off, and when it fell back to cron.
`uninstall-agent` also removes units installed under the other of `$XDG_CONFIG_HOME` and `~/.config`.
If `systemctl enable` fails while `XDG_CONFIG_HOME` is set only in your shell, the error says so; unset it for
the command to install into `~/.config`. `StartInterval` is a 32-bit number of seconds, so launchd rejects a
`tick_interval` above about 68 years with `interval too long for launchd`.
`uninstall-agent` removes the job, its definition
files and the first-run marker, and succeeds when nothing is installed; on Linux it clears both
the systemd units and the crontab line.

Every run appends one JSON line to `~/.local/state/bilgie/runs.jsonl`: time, tier, free space
before and after, swap in use (`swap_used_mib`, left out when it could not be read), bytes freed per provider,
and skipped providers with their reasons. The history table shows swap in its SWAP column, so the trend is
visible next to free space. The log rotates
to `runs.jsonl.1` at 8 MiB.

```bash
bilgie history           # Last 10 runs
bilgie history -n 50     # More runs (0 shows all)
bilgie history --json    # Full records, including per-provider detail
bilgie history --providers -n 20   # Bytes freed per provider over the last 20 runs
```

```text
$ bilgie history --providers -n 20
Bytes freed per provider over the last 20 run(s); dry-runs count as WOULD FREE
PROVIDER  RUNS  FREED     WOULD FREE  SKIPPED  ERRORS
npm       18    41.2 GiB  12.0 GiB    11       0
go-build  12    9.8 GiB   3.1 GiB     6        0
docker    7     0 B       0 B         2        5
```

`history` skips unreadable lines and reports how many. When a real run (not a dry-run) ends with
free space still under the low threshold, the pass shows one desktop notification (rate limited as above) (`osascript` on macOS, `notify-send` on Linux, a PowerShell toast on
Windows); a run that recovered enough space stays quiet. A notifier that is not installed is logged as a skipped
notification. A failed notification or log write is reported but does not fail the run, and a notifier is
cancelled after 10 seconds so it cannot hold the run lock.

### doctor

```bash
bilgie doctor
```

Checks, without changing anything, that the agent works: it is installed and loaded in the
scheduler of this OS (`launchctl print`, `systemctl --user is-active` or the crontab, `schtasks`),
the cadence (tick and full-pass intervals), the last tick (it must be recent for `auto.tick_interval`; a missing tick
means the installed agent is still the old 30-minute `auto` one) and the next expected full pass, the last run (time, tier, bytes freed, errors from `runs.jsonl`) is recent for `auto.interval`,
free space is above the floors, the 7-day free-space trend from the run history, config problems
(disabled providers, providers skipped on each of the last runs for a reason other than
"within limit", providers on protected paths, providers whose config does not load), swap and memory, and whether the notifier program exists.
It exits non-zero when a finding needs attention; each one carries a "what to do" line.

```text
$ bilgie doctor
[ok  ] agent: installed and loaded (launchd)
[ok  ] cadence: tick every 2m0s, full pass every 30m0s (low 10m0s, critical 2m0s)
[ok  ] last tick: 1m ago (tier low, low: next pass in 7m0s)
[ok  ] next full pass: in 21m while space is healthy, sooner if free space is low or falling
[FAIL] last run: 9h ago (tier low, freed 4.1 GiB, 1 provider error(s)); failed: docker
       what to do: see /Users/me/Library/Logs/bilgie/auto.log; run: bilgie auto --dry-run --verbose to see each error
[FAIL] schedule: last run was 9h ago but the interval is 30m0s: the agent is not running
       what to do: see /Users/me/Library/Logs/bilgie/auto.log; run: bilgie install-agent to reinstall it
[ok  ] freed: 38 GiB freed by 21 deleting run(s) in the last 7 days
[warn] free space: 24 GiB free of 460 GiB (5%), tier low
       what to do: run: bilgie auto; run: bilgie status for large unmanaged directories
[warn] trend: free space -41 GiB over 6d (65 GiB -> 24 GiB, 80 run(s)); min_free is reached in about 8 day(s) at this rate
       what to do: find what grows: bilgie status shows large unmanaged directories
[warn] memory: swap 14 GiB of 16 GiB used, memory free 3% (warning above 8.0 GiB)
       largest process families:
         112 x node /Users/me/dev/app/worker.js (40 with parent PID 1)
         9 x /usr/bin/python3 stale.py
       what to do: quit or restart the largest memory users; bilgie only reports and never kills processes. Swap grows on the same disk as free space
[note] config: 3 provider(s) disabled: docker-volumes, sail-dirs, xcode-archives
[warn] config: gradle was skipped on each of the last 5 runs: unavailable
       what to do: install the tool gradle needs, or set providers.gradle.enabled: false
[ok  ] notifier: osascript is available

6 finding(s) need attention
```

Swap and memory come from `sysctl vm.swapusage` and `memory_pressure` on macOS, `/proc/meminfo` on
Linux and the commit charge on Windows (the part beyond physical memory counts as swap). When swap is above
`auto.swap_warn`, the finding is a warning that names up to three process families: a command line repeated by at
least 4 processes, or at least 3 whose parent is PID 1. Command lines are cut to 60 characters. Windows lists processes by image
name only, so doctor does not name families there. If the
reading fails, doctor prints a note and carries on.

### config

```bash
bilgie config init   # Create default config
bilgie config show   # Display current config
bilgie config edit   # Open in $EDITOR
```

## Configuration

Location: `~/.config/bilgie/config.yaml`

Generate defaults with `bilgie config init`.

A saved config keeps the values it was created with. When a default changes
later, the saved value wins: configs from older versions keep `enabled: true`
for `huggingface` and `playwright`, which are now disabled by default.
`bilgie config show` ends with a note for every provider whose saved
`enabled` differs from the current default. Edit the file to change it.

```yaml
version: "1"
providers:
  go-build:
    enabled: true
    paths:
      - ~/Library/Caches/go-build
    max_size: 10G
    max_age: 30d
    clean_cmd: go clean -cache
```

| Field | Description |
|-------|-------------|
| `enabled` | Include in status/clean operations |
| `paths` | Directories to scan (supports `~` expansion) |
| `max_size` | Size limit (e.g., `10G`, `500M`) |
| `max_age` | File age threshold for smart clean (e.g., `30d`); units `m`, `h`, `d`, minimum `1m` |
| `clean_cmd` | Command for full clean (empty = file-based deletion) |
| `clean_timeout` | Max runtime of `clean_cmd` before it is cancelled and reported (default `2m`; units `ms`, `s`, `m`, `h`, `d`) |
| `type` | `dir-pattern` removes whole stale directories matching a glob in `paths`; `project-artifacts` removes build artifacts of idle projects found below the roots in `paths` |
| `min_idle` | `dir-pattern`: minimum idle time, from the newest mtime in the tree (default `2h`); `project-artifacts`: project idle time (default `7d`); units `m`, `h`, `d`, minimum `1m` |
| `trim_idle` | `project-artifacts`: Rust build units (`.fingerprint`, `build`, `deps`, `incremental`) unused for this long are removed from the `target/` of an active project; a profile whose `.cargo-lock` is held is skipped, and dry-run lists the bytes it would trim (default `30d`; units `m`, `h`, `d`, minimum `1m`) |
| `skip_if_open` | `dir-pattern`, `project-artifacts`: skip directories with open files via `lsof +D` (default `true`) |
| `skip_if_git_worktree` | `dir-pattern`: skip directories containing a `.git` entry (default `true`) |
| `max_depth` | `project-artifacts`: directory levels searched below each root (default `4`, from 1 to 16; a negative value is an error) |
| `pass_budget` | `project-artifacts`: time one clean pass may spend before the remaining candidates are skipped (default `30s`; units `ms`, `s`, `m`, `h`, `d`) |
| `scan_budget` | `project-artifacts`: time allowed for finding projects per pass (default `10s`; units `ms`, `s`, `m`, `h`, `d`) |
| `rust`, `node`, `python` | `project-artifacts`: per-kind switches (default `true`, `true`, `false`) |
| `skip_if_dirty` | `project-artifacts`: skip projects with uncommitted changes (default `true`) |
| `tier_scaling` | `project-artifacts`: shorten `min_idle` at the critical and emergency tiers (default `true`) |
| `critical_min_idle`, `emergency_min_idle` | `project-artifacts`: idle threshold at those tiers when `tier_scaling` is on (defaults `2d`, `1d`); never raises a shorter `min_idle`; same units and minimum as `min_idle` |
| `skip_prefixes` | Whole-entry providers never evict entries whose name starts with one of these prefixes; user values add to the built-in ones |

The optional top-level `auto` block configures `bilgie tick`, `bilgie auto` and the agent. Every key is optional; the values shown are the defaults:

```yaml
auto:
  tick_interval: 2m         # how often the agent checks free space (whole minutes, minimum 1m)
  interval: 30m             # routine full pass while space is healthy (minimum 1m)
  min_free: 30G             # low threshold: absolute floor
  min_free_pct: 5           # ... or this percentage of the volume (0 disables)
  min_free_cap: 100G        # the percentage part never exceeds this
  critical_free: 10G        # critical tier below this
  emergency_free: 5G        # emergency tier below this (stale-directory sweeps)
  hysteresis: 2G            # a tier eases only this far above its threshold
  low_cooldown: 10m         # minimum gap between passes at the low tier
  critical_cooldown: 2m     # ... at the critical and emergency tiers
  forecast: 15m             # pass when the falling trend crosses the low threshold within this (0 disables)
  notify_cooldown: 3h       # minimum gap between notifications of one tier (also between swap warnings)
  swap_warn: 8G             # warn when swap in use exceeds this (0 disables)
```

A config that sets `min_free_pct: 15` keeps that value, but the percentage part is still limited by `min_free_cap` (100G by default): on a 2 TB volume the low threshold is 100G, not 300G. Raise `min_free_cap` to restore the old threshold.

The optional top-level `protected` list names paths that `auto` never deletes from and that `status`
reports under "needs a human":

```yaml
protected:
  - ~/Documents/important
```

Surrounding whitespace on an entry is trimmed and a blank entry is dropped; an entry that differs from an earlier
one only by letter case or by the `/System/Volumes/Data` firmlink prefix is dropped as a duplicate (protection
compares both ways already). Entries must be literal paths, absolute or starting with `~/`: globs, `.`/`..` elements, home, its parents and top-level directories are rejected. They are added to the built-in list (`~/Downloads`,
`~/.local/share/opencode`, `/var/lib/docker/volumes`); removing a built-in entry from the file has no
effect. `auto` also skips any path inside a git checkout or worktree, any path holding one within 3 levels (see
the safety rules), and never prunes Docker volumes (Docker Desktop keeps them inside its VM image).
`project-artifacts` is the one provider that works inside git checkouts and `worktrees` directories by design; it enforces the protected list, `Downloads` and `opencode` itself and cleans only the artifact directories. `mise` also enforces the protected list itself (its `plugins/` hold git clones that `mise prune` never touches), so `auto` skips the checkout scan for it.

### Whole-unit trees

Some caches hold trees that are only valid complete: an installed package under
`~/.npm/_npx`, an extracted crate under `~/.cargo/registry/src` (cargo trusts its
`.cargo-ok` marker), a Go module under `pkg/mod`. `npm`, `cargo`, `go-mod`, `yarn`,
`gradle`, `xcode-deriveddata` and `xcode-archives` never delete inside such a
tree. Independent files (`_cacache`, `.crate` archives) are trimmed
by age, then oldest first. Trees go whole, oldest first by newest file mtime, and only while
over `max_size`; `max_age` does not apply to them because mtime records install time, not use.
`max_size` limits only what the provider may delete; untouched parts such as
`registry/index` or npm logs count neither toward the size `status` and `auto` compare to the
limit nor toward eviction. When the cache is over `max_size` but every candidate is kept, the
output says nothing is evictable. A tree with an unreadable part is kept: its size and age
are known only in part.

A tree is kept when it is the newest of its pattern, was modified in the last 2 hours or is named
on the command line of a running process. A tree is first renamed aside and then deleted, so it is
whole or gone; a leftover `.bilgie-trash-*` directory from a failed delete is removed on the next run.
Dot entries are ignored. In full mode providers with a `clean_cmd` run it.

### uv

`uv` is a command-managed provider: uv decides what goes, bilgie never deletes a file in the uv
cache. The `wheels-v*` pointer files reference `archive-v0/<id>`, so removing either side makes
the next install fail with `failed to read directory .../archive-v0/<id>`.

`uv` is off by default until a few safety follow-ups land. Enable it with `providers.uv.enabled: true`.
The old behavior of trimming uv cache files by age is gone for good because it could corrupt the cache.
A config that already sets `enabled: true` for `uv` (for example one written by `config init`) keeps it
on; a config that omits `enabled` follows the new default (off). bilgie cannot tell a saved default
from a deliberate choice, so set `enabled: false` to turn it off in an existing config.

- Smart mode (and `auto`) runs `uv cache prune`, which drops entries nothing refers to.
- Full mode runs `uv cache clean`.
- `--dry-run` prints the command and the current cache size and runs nothing, because uv has no
  dry-run for prune. Freed bytes are the cache size before minus after.
- uv is found on `PATH` at run time. If it is missing, fails or exceeds `clean_timeout`
  (default `2m`), the provider is skipped with the reason and nothing is deleted.
- The cache directory follows uv's own order when `paths` is left at the default: `UV_CACHE_DIR`,
  then `$XDG_CACHE_HOME/uv` (macOS and Linux; a relative value is ignored; Windows ignores
  `XDG_CACHE_HOME`), then `~/.cache/uv`
  (macOS/Linux) or `%LOCALAPPDATA%\uv\cache` (Windows). A `paths` entry that is not the default
  always wins over the environment; an entry spelled exactly like the default counts as the default. bilgie hands the chosen directory to uv as `UV_CACHE_DIR` and
  clears `UV_NO_CACHE`; it also runs uv with `--no-config`, so a `no-cache` or `cache-dir` setting in a uv config file cannot redirect uv. uv cleans exactly the directory bilgie measured.
- `paths` must name exactly one directory. A path that is, lies inside or contains a protected
  root is skipped.
- `max_size` and `max_age` do not bound uv: prune and clean decide. Existing configs without
  `clean_cmd` keep working. `clean_cmd` may only be `uv cache clean`, `uv cache prune` or `uv cache prune --ci`
  (a prune command also serves smart mode, so `--ci` applies there too; a clean command only replaces the full-mode default); anything else fails to load with
  `provider uv: clean_cmd must be ...`, so remove it.
- Busy guard: skipped while `<path>/.lock` is flock-held or a `uv` process runs.

### sqlite-compact

`sqlite-compact` shrinks SQLite databases that hold a lot of free pages, such as the opencode
state database (`~/.local/share/opencode/opencode.db`). SQLite never returns free pages to the
filesystem on its own. The provider deletes no file; it runs `VACUUM` through the `sqlite3`
binary on `PATH`. It is off by default. Enable it with `providers.sqlite-compact.enabled: true`
and list the database files in `paths` (no globs).

A database is compacted only when all of these hold; otherwise it is skipped with the reason:

- No process has the database, its `-wal` or `-shm` file open (checked with `lsof`; if the
  check fails, the database counts as in use).
- Free pages exceed both `max_size` (default `1G`) and half of the file.
- Free disk space is at least the size of the live data, because `VACUUM` writes a full copy.
- `sqlite3` is found on `PATH`.

`VACUUM` builds a copy and swaps it in at commit, so a failed or interrupted run leaves the
original database intact. After each run bilgie checks `PRAGMA integrity_check` and warns if it
does not return `ok`. `--dry-run` lists what would be compacted and runs nothing.

`status` reports the free-page bytes of every listed database, including one that is in use,
read from the database header. The provider's current size is that total. `--json` adds a
`databases` list with `path`, `bytes`, `reclaimable_bytes` and `in_use`. The header can lag
while a WAL is not yet checkpointed.

To compact by hand: quit every process that uses the database, check that free disk space
exceeds the live data (file size minus free pages), then run:

```bash
sqlite3 ~/.local/share/opencode/opencode.db 'VACUUM;'
sqlite3 ~/.local/share/opencode/opencode.db 'PRAGMA integrity_check;'
```

Switching `auto_vacuum` to incremental needs one full `VACUUM` as well; bilgie does not change it.

### mise

`mise` is a command-managed provider: mise decides what goes, bilgie never deletes inside
`installs/`. A run (`clean`, `auto`, smart or full mode alike) does this:

1. Refuses a path that is, lies inside or contains a protected root (`~/Downloads`, opencode data,
   Docker volumes, the `protected` list). `auto` does not run the git checkout scan on mise: the
   plugin clones under `plugins/` (`lua`, `make`, `teleport-ent`, ...) are real git checkouts, and
   they are never touched.
2. Skips with a reason when `mise` is running or any process command line names a file below
   `<mise dir>/installs/`, or the process list cannot be read. The check sees only what the process
   list shows: a binary started by bare name from `PATH` (`node server.js`), or any binary on Windows
   (image names only), is not visible to it. `mise prune` itself keeps the version a running process
   started from, so mise is the backstop there.
3. Runs `mise prune --dry-run` and reads its report: `mise <tool>@<version> is prunable: ...` names the
   version, and `mise <tool>@<version> [dryrun] remove <installs>/<dir>/<version>, <cache>/<tool>/<version>`
   gives its directories (the installs directory name is a slug, `npm:@redocly/cli` is `npm-redocly-cli`).
   Other known lines (`uninstall`, `done`, `pruned configuration links`) are ignored. A failing, timed
   out or unparseable listing, or a path outside `installs/<dir>/<version>`, skips the provider and
   deletes nothing. Sizes are measured on the listed directories.
4. Lists each version with its size (`would prune: node@20.0.0 (1.2 GB)`); a dry-run stops here.
   A real run calls `mise prune --yes` once, then reports `pruned:` or, when mise kept a version,
   `kept:`.
5. Trims `<first path>/downloads` and every further path (the cache directory, `~/Library/Caches/mise`
   or `~/.cache/mise`) by `max_age`, then oldest first while over `max_size`.

`mise prune` removes only versions that no tracked config (`~/.local/state/mise/tracked-configs`)
references. A version used only by an untracked project directory, by `MISE_<TOOL>_VERSION` or by
`mise exec` may be removed; mise reinstalls it on demand. The real `mise prune --yes` removes what is
unused at that moment, which can differ from the earlier listing if a config changed in between. `clean_cmd` defaults to `mise prune` and
must be `<mise executable> prune` optionally followed by tool names, with no flags (bilgie adds `--dry-run` and `--yes` itself); `clean_timeout` bounds each call.

### Busy tools

`clean` skips a provider while its tool is active, so a clean never breaks a
running build or waits on a held lock. The skip reason shows in `clean` output
and in `clean --json` (`"status": "skipped"`, `"reason"`). A skip is not a
failure. If the check itself fails, the provider is skipped too.

| Provider | Skipped while |
|----------|---------------|
| `go-build`, `go-mod` | a `go` process runs |
| `cargo` | a `cargo` or `rustc` process runs |
| `gradle` | a `gradle` or `gradlew` process, or a Gradle daemon, runs |
| `homebrew` | a `brew` process runs |
| `yarn` | a `yarn` process runs (`yarn`, `yarn.js`, `yarnpkg`; any OS) |
| `uv` | `<path>/.lock` is flock-held, or a `uv` process runs |
| `mise` | a `mise` process runs, or a process command line names a file below `<path>/installs/` |

Busy detection errs toward skipping: any process whose command line contains
the tool name counts, including wrappers such as `sudo` or `sh -c`. The
bilgie process and its wrapper ancestors are not counted: a wrapper is
recognized by parsing its command string with shell quoting rules (quotes,
backslash escapes, repeated spaces, nested `sh -c`, `env VAR=x`, `sudo`) and
finding bilgie's own executable and arguments in it. A string that does not
parse falls back to its whitespace-separated words and must still show bilgie's
executable followed by its arguments, or it stays busy. Shell separators glued to a word
(`echo hi;bilgie clean cargo`) are split. An ancestor that is the tool itself, such as
`cargo run -- clean cargo`, is counted. A hung
`clean_cmd` is killed with its whole process group, so it must not need a
terminal.

Age thresholds (`max_age`, `min_idle`) must be at least `1m`. A value such as
`30s` or `30ms` is almost always a unit typo (`30d`, `30m`) and would make smart
clean delete nearly everything, so the config is rejected when it loads, for every
command, with a message like `provider "go-build": max_age 30s is under the 1m
minimum; did you mean 30d?`. `ms` is accepted only for time bounds
(`scan_budget`, `pass_budget`, `clean_timeout`). Bare numbers are seconds.

`clean --json` needs `--force` or `--dry-run` because it cannot prompt.

`dir-pattern` paths must contain a glob character and be absolute or start with `~/`,
and `min_idle` must parse. A provider whose config does not load is not hidden:
`clean`, `status`, `auto` and `doctor` print `provider <name>: <reason>` (the same text goes into the
`--json` output and the run log) and every other provider still runs. `auto` refuses a whole config that
fails validation (a relative or glob-less path) and names the offending provider in that error.

Sizes and freed bytes count each hard-linked file once, across all matched directories and in dry-run
totals; a `dir-pattern` sweep reports a shared file as freed only when every link to it lies inside the removed directories, and duplicate patterns list a directory once. On Linux and macOS a file is identified by device and inode. On Windows by volume serial and file
index (one extra open per file); if the identity cannot be read, every link counts in full, so the
figure can only be too high. The age-based file trim lists each link on its own and still counts it
separately.

The provider ignores
`max_age` and removes every stale match whole regardless of `max_size`, which is
only used for `status`. `lsof` run as a non-root user cannot see other users'
processes.

The built-in `sail-dirs` provider (`sail*` in the OS temp dir and in the fixed system
temp dir, `/tmp` or `/private/tmp` on macOS, listed once when both resolve to the same directory)
uses `dir-pattern` and is disabled by default. The paths follow the machine and are not saved to
the config file. Enable it with `enabled: true` after reviewing
`bilgie clean sail-dirs --dry-run`, which lists each directory with its size,
idle time and the reason it would be skipped.

## Building from Source

```bash
git clone https://github.com/smykla-skalski/bilgie
cd bilgie
mise run build
```

### Mise Tasks

| Task | Description |
|------|-------------|
| `mise run build` | Build binary |
| `mise run install` | Install to $GOPATH/bin |
| `mise run test` | Run tests with race detection |
| `mise run lint` | Run golangci-lint |
| `mise run cover` | Generate coverage report |
| `mise run clean` | Remove build artifacts |
| `mise run all` | lint + test + build |

## License

MIT
