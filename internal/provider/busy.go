package provider

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kballard/go-shellquote"
	"github.com/smykla-skalski/bilgie/internal/osshim"
)

// busyProcesses maps a provider name to the tool processes whose presence
// means a clean could break a running build or install.
var busyProcesses = map[string][]string{
	"go-build": {"go"},
	"go-mod":   {"go"},
	"homebrew": {"brew"},
	"gradle":   {"gradle", "gradlew"},
	"rustup":   {"rustup", "cargo", "rustc"},
	"uv":       {"uv"},
	"yarn":     {"yarn"},
}

// busyLockFiles maps a provider name to a lock file name, relative to each
// provider path, that the tool holds with flock while it uses the cache.
var busyLockFiles = map[string]string{
	"uv": ".lock",
}

// busyParentLockFiles maps a provider name to lock file names that sit beside
// each provider path, in its parent directory. cargo takes them while it
// downloads or updates its registry and git caches, not while it compiles, so
// a long-running cargo process does not block the cargo provider.
var busyParentLockFiles = map[string][]string{
	"cargo": {".package-cache", ".package-cache-mutate"},
}

// busyAging lets a tool's long-running commands stop blocking its provider
// once they are old enough. "go run" and "go test" keep their go process alive
// for as long as the program or test binary they started runs, long after the
// compile that used the caches is done; "go build" and "go mod download" are
// never aged out.
type busyAging struct {
	tool     string
	commands []string
	after    time.Duration
}

var goBusyAging = &busyAging{tool: "go", commands: []string{"run", "test"}, after: time.Hour}

// busyAgedCommands maps a provider name to the aging rule of its tool.
var busyAgedCommands = map[string]*busyAging{
	"go-build": goBusyAging,
	"go-mod":   goBusyAging,
}

// busyGuard detects that a cache's owning tool is active.
type busyGuard struct {
	locks     []string
	processes []string
	aging     *busyAging

	listProcesses func(ctx context.Context) ([]osshim.Process, error)
	lockHeld      func(path string) (bool, error)
	now           func() time.Time
}

func newBusyGuard(name string, paths []string) *busyGuard {
	procs := busyProcesses[name]
	var locks []string
	if lockName, ok := busyLockFiles[name]; ok {
		for _, p := range paths {
			locks = append(locks, filepath.Join(p, lockName))
		}
	}
	for _, lockName := range busyParentLockFiles[name] {
		for _, p := range paths {
			lock := filepath.Join(filepath.Dir(p), lockName)
			if !slices.Contains(locks, lock) {
				locks = append(locks, lock)
			}
		}
	}
	if len(procs) == 0 && len(locks) == 0 {
		return nil
	}

	return &busyGuard{
		locks:         locks,
		processes:     procs,
		aging:         busyAgedCommands[name],
		listProcesses: processTableLister(procs),
		lockHeld:      osshim.LockHeld,
		now:           time.Now,
	}
}

// processLister returns a lister that leaves out bilgie's own process
// and its ancestors. They carry the provider name as an argument
// ("bilgie clean cargo"), so they would always read as the tool being
// busy.
func processLister(wanted []string) func(context.Context) ([]string, error) {
	list := processTableLister(wanted)
	return func(ctx context.Context) ([]string, error) {
		procs, err := list(ctx)
		if err != nil {
			return nil, err
		}
		return commandLines(procs), nil
	}
}

// processTableLister is processLister with the process rows kept, so a caller
// can read each one's pid and start time.
func processTableLister(wanted []string) func(context.Context) ([]osshim.Process, error) {
	return func(ctx context.Context) ([]osshim.Process, error) {
		procs, err := osshim.ProcessTable(ctx)
		if err != nil {
			return nil, err
		}
		return excludeSelf(procs, os.Getpid(), wanted), nil
	}
}

func commandLines(procs []osshim.Process) []string {
	lines := make([]string, len(procs))
	for i := range procs {
		lines[i] = procs[i].CommandLine
	}
	return lines
}

// excludeSelf drops the process with pid self and its ancestors, except an
// ancestor that is a wanted tool: its executable is the tool, or its command
// line names the tool without wrapping bilgie's own invocation
// ("cargo run -- clean", "ruby brew.rb bundle"). A wrapper such as
// "sh -c 'bilgie clean cargo'" holds that invocation and is dropped. A zero
// pid is unknown and never matches.
func excludeSelf(procs []osshim.Process, self int, wanted []string) []osshim.Process {
	return excludeSelfFor(procs, self, wanted, runtime.GOOS == "windows")
}

func excludeSelfFor(procs []osshim.Process, self int, wanted []string, windows bool) []osshim.Process {
	byPID := make(map[int]*osshim.Process, len(procs))
	for i := range procs {
		if procs[i].PID != 0 {
			byPID[procs[i].PID] = &procs[i]
		}
	}

	var inv invocation
	if p, ok := byPID[self]; ok {
		inv = newInvocation(commandText(p), windows)
	}

	skip := map[int]bool{}
	seen := map[int]bool{}
	for pid := self; pid != 0 && !seen[pid]; {
		seen[pid] = true
		p, ok := byPID[pid]
		if !ok {
			break
		}
		if pid == self || !keepAncestor(p, inv, wanted, windows) {
			skip[pid] = true
		}
		pid = p.PPID
	}

	kept := make([]osshim.Process, 0, len(procs))
	for i := range procs {
		if procs[i].PID != 0 && skip[procs[i].PID] {
			continue
		}
		kept = append(kept, procs[i])
	}
	return kept
}

// commandText is the command line without any appended kernel process name.
func commandText(p *osshim.Process) string {
	if p.Args != "" {
		return p.Args
	}
	return p.CommandLine
}

// invocation is how bilgie itself was started: the executable base name and
// the whitespace-separated arguments after it.
type invocation struct {
	exe  string
	args []string
}

func newInvocation(text string, windows bool) invocation {
	norm := normalizeSeparators(text, windows)
	fields := plainFields(norm)
	if windows {
		if flat, ok := flattenCommand(norm, 0); ok {
			fields = flat
		}
	}
	if len(fields) < 2 {
		return invocation{}
	}
	return invocation{exe: exeName(fields[0], windows), args: fields[1:]}
}

func keepAncestor(p *osshim.Process, inv invocation, wanted []string, windows bool) bool {
	if matchProcessFor(firstToken(p.CommandLine), wanted, windows) != "" {
		return true
	}
	return !wrapsInvocation(commandText(p), inv, wanted, windows) && matchProcessFor(p.CommandLine, wanted, windows) != ""
}

// wrapsInvocation reports whether a command line launches bilgie with its own
// arguments, directly or through any depth of shell -c strings, with no wanted
// tool before it ("sudo cargo run -- bilgie clean"). A string that
// does not parse (ps drops the quotes around a path with an apostrophe) falls
// back to plain whitespace-separated fields; the run of executable and
// arguments must still be found, so a truly broken string errs busy.
func wrapsInvocation(text string, inv invocation, wanted []string, windows bool) bool {
	if inv.exe == "" {
		return false
	}
	norm := normalizeSeparators(text, windows)
	tokens, ok := flattenCommand(norm, 0)
	if !ok {
		tokens = plainFields(norm)
	}
	for i := 1; i+len(inv.args) <= len(tokens); i++ {
		if slices.Equal(tokens[i:i+len(inv.args)], inv.args) && exeName(tokens[i-1], windows) == inv.exe {
			return matchProcessFor(strings.Join(tokens[:i-1], " "), wanted, windows) == ""
		}
	}
	return false
}

const maxWrapperDepth = 8

// flattenCommand splits text with shell quoting rules and splits every token
// that still holds whitespace again, so nested "sh -c" strings and quoted
// paths with spaces reduce to the whitespace-separated words a process listing
// shows, with shell operators glued to a word trimmed off. It reports false when any level does not parse.
func flattenCommand(text string, depth int) ([]string, bool) {
	if depth > maxWrapperDepth {
		return nil, false
	}
	words, err := shellquote.Split(text)
	if err != nil {
		return nil, false
	}
	out := make([]string, 0, len(words))
	for _, w := range words {
		if len(strings.Fields(w)) <= 1 {
			out = append(out, splitOperators(w)...)
			continue
		}
		inner, ok := flattenCommand(w, depth+1)
		if !ok {
			return nil, false
		}
		out = append(out, inner...)
	}
	return out, true
}

// plainFields splits text on whitespace only, with shell operators trimmed
// off each word, for command lines that shell quoting cannot parse.
func plainFields(text string) []string {
	out := make([]string, 0, 8)
	for w := range strings.FieldsSeq(text) {
		out = append(out, splitOperators(w)...)
	}
	return out
}

// isShellOperator reports a character that separates commands in a shell.
func isShellOperator(r rune) bool {
	return r == ';' || r == '&' || r == '|'
}

// splitOperators cuts a word at command separators glued to it
// ("hi;bilgie") and trims grouping characters off the pieces.
func splitOperators(w string) []string {
	out := make([]string, 0, 2)
	for part := range strings.FieldsFuncSeq(w, isShellOperator) {
		if t := strings.Trim(part, " \t\n()"); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// normalizeSeparators turns Windows backslashes into slashes, which shell
// quoting would read as escapes.
func normalizeSeparators(s string, windows bool) string {
	if windows {
		return strings.ReplaceAll(s, `\`, "/")
	}
	return s
}

func exeName(token string, windows bool) string {
	base := filepath.Base(token)
	if windows {
		base = strings.TrimSuffix(strings.ToLower(base), ".exe")
	}
	return base
}

func firstToken(commandLine string) string {
	fields := strings.Fields(commandLine)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// busyReason returns a non-empty reason when the tool is busy. A failed check
// counts as busy: cleaning blind is what the guard exists to prevent.
func (g *busyGuard) busyReason(ctx context.Context) string {
	if g == nil {
		return ""
	}

	for _, path := range g.locks {
		held, err := g.lockHeld(path)
		if err != nil {
			return fmt.Sprintf("cannot check lock %s: %v", path, err)
		}
		if held {
			return "lock held: " + path
		}
	}

	if len(g.processes) == 0 {
		return ""
	}

	procs, err := g.listProcesses(ctx)
	if err != nil {
		return "cannot list processes: " + err.Error()
	}
	now := time.Now()
	if g.now != nil {
		now = g.now()
	}
	for i := range procs {
		proc := matchProcess(procs[i].CommandLine, g.processes)
		if proc == "" || g.aging.agedOut(&procs[i], now) {
			continue
		}
		return describeBusy(proc, &procs[i], now)
	}

	return ""
}

// describeBusy names the blocking process, with its pid and age when the
// process listing reports them.
func describeBusy(tool string, p *osshim.Process, now time.Time) string {
	var details []string
	if p.PID > 0 {
		details = append(details, "pid "+strconv.Itoa(p.PID))
	}
	if !p.Started.IsZero() {
		details = append(details, "up "+now.Sub(p.Started).Round(time.Second).String())
	}
	if len(details) == 0 {
		return tool + " is running"
	}
	return tool + " is running (" + strings.Join(details, ", ") + ")"
}

// agedOut reports whether p has run for the aging threshold and every
// invocation of the tool on its command line is an aging command. An unknown
// start time, an unparsed subcommand or any other invocation keeps it busy.
func (a *busyAging) agedOut(p *osshim.Process, now time.Time) bool {
	if a == nil || p.Started.IsZero() || now.Sub(p.Started) < a.after {
		return false
	}
	subs := toolSubcommands(commandText(p), a.tool, runtime.GOOS == "windows")
	if len(subs) == 0 {
		return false
	}
	for _, sub := range subs {
		if !slices.Contains(a.commands, sub) {
			return false
		}
	}
	return true
}

// chdirFlag stands in for the subcommand of a "go -C dir ..." invocation. The
// directory may hold spaces, which a process listing does not quote, so the
// subcommand cannot be told from the path and the invocation stays busy.
const chdirFlag = "-C"

// toolSubcommands returns the subcommand that follows each occurrence of the
// tool on a command line; an occurrence with no subcommand yields "". The
// words after a subcommand are its arguments, so a later word that merely
// names the tool ("go run ./cmd/go") is not another invocation until a shell
// separator ends the command.
func toolSubcommands(commandLine, tool string, windows bool) []string {
	fields := strings.Fields(separatorWords.Replace(commandLine))
	var subs []string
	inArgs := false
	for i, field := range fields {
		if inArgs {
			inArgs = !endsCommand(field)
			continue
		}
		if fieldTool(field, windows) != tool {
			continue
		}
		sub := ""
		if i+1 < len(fields) {
			sub = trimField(fields[i+1])
			if sub == "-C" || strings.HasPrefix(sub, "-C=") {
				sub = chdirFlag
			}
			inArgs = !endsCommand(fields[i+1])
		}
		subs = append(subs, sub)
	}
	return subs
}

// separatorWords sets every shell separator apart as a word of its own, so a
// glued "a.go|go build" shows both commands.
var separatorWords = strings.NewReplacer(";", " ; ", "&", " ; ", "|", " ; ")

// endsCommand reports whether a word is a shell command separator.
func endsCommand(field string) bool {
	return field == ";"
}

func trimField(field string) string {
	return strings.Trim(field, `"';&|()`)
}

// toolAliases maps a sibling binary to the tool it belongs to.
var toolAliases = map[string]string{
	"uvx":                             "uv",
	"yarn.js":                         "yarn",
	"yarn.cjs":                        "yarn",
	"yarn.cmd":                        "yarn",
	"yarnpkg":                         "yarn",
	"yarnpkg.cmd":                     "yarn",
	"gradle.bat":                      "gradle",
	"gradle.cmd":                      "gradle",
	"gradlew.bat":                     "gradlew",
	"gradlew.cmd":                     "gradlew",
	gradleDaemonMain:                  "gradle",
	strings.ToLower(gradleDaemonMain): "gradle",
}

const gradleDaemonMain = "org.gradle.launcher.daemon.bootstrap.GradleDaemon"

// matchProcess reports which wanted tool a process command line involves,
// by the rules of the running OS.
func matchProcess(commandLine string, wanted []string) string {
	return matchProcessFor(commandLine, wanted, runtime.GOOS == "windows")
}

// matchProcessFor is matchProcess with the OS rules chosen by the caller, so
// Windows-style lines can be tested on any host.
// Any token whose basename is the tool counts, wherever it sits (wrappers,
// "sh -c" strings, "xargs go vet"). It errs toward busy: a spurious skip only
// delays a clean, while a missed process could break a live build.
func matchProcessFor(commandLine string, wanted []string, windows bool) string {
	for field := range strings.FieldsSeq(commandLine) {
		base := fieldTool(field, windows)
		for _, want := range wanted {
			if base == want {
				return want
			}
		}
	}
	return ""
}

// fieldTool is the tool name a command-line word stands for: its base name
// with sibling binaries mapped to their tool.
func fieldTool(field string, windows bool) string {
	field = trimField(field)
	if windows {
		field = strings.ReplaceAll(field, `\`, "/")
	}
	base := filepath.Base(field)
	if windows {
		base = strings.TrimSuffix(strings.ToLower(path.Base(field)), ".exe")
	}
	if strings.HasPrefix(base, "yarn-") && strings.HasSuffix(base, ".cjs") {
		base = "yarn"
	}
	if alias, ok := toolAliases[base]; ok {
		base = alias
	}
	if base == "brew.rb" {
		base = "brew"
	}
	return base
}
