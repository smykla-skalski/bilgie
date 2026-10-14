package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/smykla-skalski/bilgie/internal/osshim"
)

const gitStatusTimeout = 15 * time.Second

var kindTools = map[artifactKind][]string{
	kindRust: {"cargo", "rustc"},
	kindNode: {
		"node", "npm", "npx", "pnpm", "yarn", "bun", "deno", "tsx", "ts-node", "vite", "next", "next-server",
		"nuxt", "webpack", "nodemon", "vitest", "jest", "turbo", "esbuild",
	},
	kindPython: {"python", "python3", "pip", "pip3", "uv", "poetry"},
}

type gitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// runGit runs git without taking optional locks, so a status probe never
// rewrites the index of a repository somebody is using.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitStatusTimeout)
	defer cancel()
	full := append([]string{"--no-optional-locks", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	osshim.KillTreeOnCancel(cmd)
	cmd.WaitDelay = cleanWaitDelay
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// toolProcess is a running build tool. Cwd is empty when it could not be
// read; CwdUnknown says the lookup failed for every process.
type toolProcess struct {
	Tool        string
	pid         int
	CommandLine string
	Cwd         string
	CwdUnknown  bool
}

type processLookup func(ctx context.Context) ([]toolProcess, error)

// lookupToolProcesses lists running build tools with their working
// directories. Failing to list processes is an error, so callers fail closed.
func lookupToolProcesses(ctx context.Context) ([]toolProcess, error) {
	table, err := osshim.ProcessTable(ctx)
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var procs []toolProcess
	var pids []int
	for i := range table {
		if table[i].PID == self {
			continue
		}
		if tool := matchAnyKind(table[i].CommandLine); tool != "" {
			procs = append(procs, toolProcess{Tool: tool, pid: table[i].PID, CommandLine: table[i].CommandLine})
			pids = append(pids, table[i].PID)
		}
	}
	if len(procs) == 0 {
		return nil, nil
	}
	cwds, err := osshim.ProcessCwds(ctx, pids)
	for i := range procs {
		switch {
		case err != nil:
			procs[i].CwdUnknown = true
		default:
			procs[i].Cwd = cwds[procs[i].pid]
		}
	}
	return procs, nil
}

// measureActivity fills in when the project was last touched: the newest
// mtime outside .git and the artifact directories (a bounded sample), and the
// git HEAD and reflog time. Anything unreadable becomes the project's
// problem, which skips it.
func (p *ProjectArtifactsProvider) measureActivity(ctx context.Context, proj *project) {
	if proj.measured || ctx.Err() != nil {
		return
	}
	proj.measured = true

	newest, truncated, err := sampleNewest(ctx, proj.Dir, sampleLimit)
	if err != nil {
		if ctx.Err() == nil {
			proj.problem = "cannot read project: " + err.Error()
		}
		proj.measured = ctx.Err() == nil
		return
	}
	proj.sampled = newest
	proj.newest = newest
	if truncated {
		proj.problem = "too many files to judge idleness"
		return
	}

	repoRoot, gitDir, err := findGit(proj.Dir, proj.root, homeSpellings(p.home))
	if err != nil {
		proj.problem = "git state unreadable: " + err.Error()
		return
	}
	proj.repoRoot = repoRoot
	if gitDir == "" {
		return
	}
	gitTime, err := gitActivity(gitDir)
	if err != nil {
		proj.problem = "git state unreadable: " + err.Error()
		return
	}
	proj.gitDir, proj.gitTime = gitDir, gitTime
	if gitTime.After(proj.newest) {
		proj.newest = gitTime
	}
}

// findGit walks up from dir to the filesystem root, so a scan root inside a
// repository still finds it, and stops before any of homes: a dotfiles
// repository in home must not make every project below it part of that
// repository. repoRoot is the directory holding the .git entry; gitDir is the
// directory with HEAD, which for a linked worktree is the worktree's own
// admin directory.
func findGit(dir, root string, homes []string) (repoRoot, gitDir string, err error) {
	for cur := dir; ; cur = filepath.Dir(cur) {
		if slices.ContainsFunc(homes, func(h string) bool { return foldPathText(filepath.Clean(h)) == foldPathText(cur) }) {
			return "", "", nil
		}
		entry := gitEntryWithin(cur, root)
		if info, statErr := os.Lstat(entry); statErr == nil {
			switch {
			case info.IsDir():
				return cur, entry, nil
			case info.Mode().IsRegular():
				resolved, resolveErr := readGitFile(entry, cur)
				return cur, resolved, resolveErr
			default:
				return cur, "", errors.New(".git is neither a directory nor a file")
			}
		}
		if filepath.Dir(cur) == cur {
			return "", "", nil
		}
	}
}

// gitEntryWithin is gitEntry, but lists a directory above the scan root only
// for the exact .git name: such ancestors can hold huge numbers of entries
// and a listing ignores the pass budget.
func gitEntryWithin(dir, root string) string {
	if root != "" && !pathWithin(dir, root) {
		return filepath.Join(dir, ".git")
	}
	return gitEntry(dir)
}

// gitEntry returns the path of dir's .git entry, whatever its case: a
// case-insensitive volume accepts .GIT, and treating a lookalike as git on any
// OS only makes the checks stricter. Without one it returns dir/.git.
func gitEntry(dir string) string {
	exact := filepath.Join(dir, ".git")
	if _, err := os.Lstat(exact); err == nil {
		return exact
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return exact
	}
	for _, e := range entries {
		if isGitName(e.Name()) {
			return filepath.Join(dir, e.Name())
		}
	}
	return exact
}

func isGitName(name string) bool {
	return strings.EqualFold(name, ".git")
}

func readGitFile(path, base string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return "", errors.New(".git file has no gitdir line")
	}
	target = strings.TrimSpace(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	return filepath.Clean(target), nil
}

// gitActivity returns the newest of HEAD's mtime, the reflog's mtime and the
// timestamp of the reflog's last entry.
func gitActivity(gitDir string) (time.Time, error) {
	headInfo, err := os.Stat(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return time.Time{}, err
	}
	newest := headInfo.ModTime()
	reflog := filepath.Join(gitDir, "logs", "HEAD")
	info, err := os.Stat(reflog)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newest, nil
		}
		return time.Time{}, err
	}
	if info.ModTime().After(newest) {
		newest = info.ModTime()
	}
	if ts, ok := lastReflogTime(reflog); ok && ts.After(newest) {
		newest = ts
	}
	return newest, nil
}

// lastReflogTime parses the commit time of the final reflog line, which reads
// "<old> <new> Name <email> <unix-time> <tz>\t<message>".
func lastReflogTime(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}
	const tail = 8192
	offset := max(info.Size()-tail, 0)
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}, false
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\r\n"), "\n")
	head, _, _ := strings.Cut(lines[len(lines)-1], "\t")
	end := strings.LastIndex(head, ">")
	if end < 0 {
		return time.Time{}, false
	}
	fields := strings.Fields(head[end+1:])
	if len(fields) == 0 {
		return time.Time{}, false
	}
	secs, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// busyReason skips an artifact whose project has a running build tool of the
// artifact's kind: one whose command line names the project, or whose
// working directory is inside it (or in the enclosing repository, above it),
// or above it inside the scan root with no path argument pointing elsewhere.
// A matched process whose directory cannot be read counts as busy.
func (ps *pass) busyReason(c *candidate) string {
	if ps.procsErr != nil {
		return "cannot list processes: " + ps.procsErr.Error()
	}
	tools := kindTools[c.art.Kind]
	proj := c.proj
	for _, proc := range ps.procs {
		tool := proc.Tool
		if !slices.Contains(tools, tool) {
			tool = matchKind(proc.CommandLine, c.art.Kind)
		}
		if tool == "" {
			continue
		}
		switch {
		case foldContains(proc.CommandLine, proj.Dir), foldContains(proc.CommandLine, proj.alias):
			return tool + " is running in the project"
		case proc.CwdUnknown, proc.Cwd == "":
			return tool + procLabel(proc) + " is running and its directory cannot be read"
		case pathWithin(proc.Cwd, proj.Dir):
			return tool + " is running in the project"
		case argsReach(proc, proj.Dir, proj.alias), namesRelative(proc, proj.Dir):
			return tool + " is running on the project"
		case proj.repoRoot != "" && pathWithin(proc.Cwd, proj.repoRoot) && pathWithin(proj.Dir, proc.Cwd):
			return tool + " is running in the repository"
		case holdsFromAbove(proc, proj):
			return tool + " is running above the project"
		}
	}
	return ""
}

func procLabel(proc toolProcess) string {
	if proc.pid > 0 {
		return fmt.Sprintf(" (pid %d)", proc.pid)
	}
	return ""
}

// pathFlags are the options whose value is a project path.
var pathFlags = []string{"--manifest-path", "--prefix", "--cwd", "-C", "--workspace", "-w"}

// argsReach reports whether a path in the process's arguments, resolved
// against its working directory, lies inside one of dirs. This catches
// `cd ~/code && node app/server.js` and `npm --prefix app start`, whose
// working directory is above the project. Only path-like tokens and the
// values of pathFlags count, and an ancestor working directory alone never
// does.
func argsReach(proc toolProcess, dirs ...string) bool {
	if proc.Cwd == "" {
		return false
	}
	for _, fields := range [][]string{splitQuoted(proc.CommandLine), strings.Fields(proc.CommandLine)} {
		if fieldsReach(fields, proc.Cwd, dirs) {
			return true
		}
	}
	return false
}

func fieldsReach(fields []string, cwd string, dirs []string) bool {
	for _, spellings := range argPaths(fields, cwd) {
		for _, spelling := range spellings {
			for _, dir := range dirs {
				if dir != "" && pathWithin(spelling, dir) {
					return true
				}
			}
		}
	}
	return false
}

// argPaths returns, for each path-like argument, its spellings resolved
// against cwd: lexical and, when it exists, symlink-resolved.
func argPaths(fields []string, cwd string) [][]string {
	var out [][]string
	for i, field := range fields {
		tok := strings.Trim(field, `"';&|()`)
		if strings.HasPrefix(tok, "-") {
			flag, value, hasValue := strings.Cut(tok, "=")
			if !slices.Contains(pathFlags, flag) {
				continue
			}
			if !hasValue {
				if i+1 == len(fields) {
					continue
				}
				value = strings.Trim(fields[i+1], `"';&|()`)
			}
			tok = value
		} else if !strings.ContainsAny(tok, `/\`) && !strings.HasPrefix(tok, ".") && !existsBelow(cwd, tok) {
			continue
		}
		if tok == "" {
			continue
		}
		abs := tok
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, tok)
		}
		spellings := []string{abs}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
			spellings = append(spellings, resolved)
		}
		out = append(out, spellings)
	}
	return out
}

// holdsFromAbove reports whether a build tool run from a directory above the
// project, inside the scan root, holds it: `cd ~/code && cargo build` builds
// whatever manifest sits below. Any path argument that resolves outside the
// project means the tool works elsewhere, so it holds only that target.
func holdsFromAbove(proc toolProcess, proj *project) bool {
	if proc.Cwd == "" || proj.root == "" || proc.Cwd == proj.Dir ||
		!pathWithin(proj.Dir, proc.Cwd) || !pathWithin(proc.Cwd, proj.root) {
		return false
	}
	for _, fields := range [][]string{splitQuoted(proc.CommandLine), strings.Fields(proc.CommandLine)} {
		for _, spellings := range argPaths(fields, proc.Cwd) {
			if isToolPath(spellings[0]) {
				continue
			}
			if !slices.ContainsFunc(spellings, func(sp string) bool { return pathWithin(sp, proj.Dir) }) {
				return false
			}
		}
	}
	return true
}

// isToolPath reports whether path is a build tool executable, such as the
// /usr/bin/node that starts the command line.
func isToolPath(path string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(path)), ".exe")
	return matchAnyKind(base) != ""
}

// namesRelative reports whether the command line spells the project's path
// relative to the process's working directory, which sits above it. This
// catches names ps shows without quoting, such as `node my app/server.js`.
func namesRelative(proc toolProcess, projDir string) bool {
	rel, err := filepath.Rel(proc.Cwd, projDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	line, want := foldPathText(proc.CommandLine), foldPathText(filepath.ToSlash(rel))
	for from := 0; ; {
		i := strings.Index(line[from:], want)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(want)
		leftOK := start == 0 || strings.ContainsRune(" \t\"'=/", rune(line[start-1]))
		rightOK := end == len(line) || strings.ContainsRune(" \t\"'/", rune(line[end]))
		if leftOK && rightOK {
			return true
		}
		from = start + 1
	}
}

// existsBelow reports whether tok, a bare word such as the app in `node app`,
// names something that exists in cwd.
func existsBelow(cwd, tok string) bool {
	if tok == "" || strings.HasPrefix(tok, "-") {
		return false
	}
	_, err := os.Lstat(filepath.Join(cwd, tok))
	return err == nil
}

// splitQuoted splits a command line on spaces, keeping a quoted run, which may
// hold spaces, in one field with the quotes removed.
func splitQuoted(s string) []string {
	var (
		fields []string
		cur    strings.Builder
		quote  rune
		active bool
	)
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, active = r, true
		case r == ' ' || r == '\t':
			if active || cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
				active = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if active || cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	return fields
}

// dirtyReason skips projects whose git tree has uncommitted changes. Git
// missing or failing skips the project too. One status per repository.
func (ps *pass) dirtyReason(ctx context.Context, proj *project) string {
	p := ps.p
	if !p.skipDirty || proj.repoRoot == "" {
		return ""
	}
	if reason, ok := ps.dirty[proj.repoRoot]; ok {
		return reason
	}
	out, err := p.git(ctx, proj.repoRoot, "status", "--porcelain")
	reason := ""
	switch {
	case ctx.Err() != nil:
		return "cancelled"
	case err != nil:
		reason = "git status failed: " + err.Error()
	case strings.TrimSpace(out) != "":
		reason = "uncommitted changes"
	}
	ps.dirty[proj.repoRoot] = reason
	return reason
}

// protectedReason reports why path may not be touched: it is, lies inside or
// contains a protected location, or has a Downloads or opencode element.
func (p *ProjectArtifactsProvider) protectedReason(path string) string {
	p.mu.Lock()
	protected := append([]string(nil), p.protected...)
	p.mu.Unlock()

	candidates := []string{filepath.Clean(path)}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != candidates[0] {
		candidates = append(candidates, resolved)
	}
	for _, cand := range candidates {
		if lexicallyProtected(cand) {
			return "protected path"
		}
		for _, root := range protected {
			for _, spelling := range rootSpellings(root) {
				if pathWithin(cand, spelling) || pathWithin(spelling, cand) {
					return "protected path " + root
				}
			}
		}
	}
	return ""
}

func rootSpellings(root string) []string {
	out := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		out = append(out, resolved)
	}
	return out
}

// lexicallyProtected checks only the path text: Downloads and opencode
// elements and Xcode's Archives folder. It is cheap enough for every
// directory the search visits.
func lexicallyProtected(path string) bool {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i, part := range parts {
		if strings.EqualFold(part, "Downloads") || strings.EqualFold(part, "opencode") {
			return true
		}
		if strings.EqualFold(part, "Xcode") && i+1 < len(parts) && strings.EqualFold(parts[i+1], "Archives") {
			return true
		}
	}
	return false
}

// expandProtected turns ~/ entries into absolute paths below home.
func expandProtected(entries []string, home string) []string {
	var out []string
	for _, entry := range entries {
		if runtime.GOOS == "windows" || strings.HasPrefix(entry, `~\`) {
			entry = strings.ReplaceAll(entry, `\`, "/")
		}
		if rest, ok := strings.CutPrefix(entry, "~/"); ok {
			if home == "" {
				continue
			}
			entry = filepath.Join(home, filepath.FromSlash(rest))
		}
		if filepath.IsAbs(entry) {
			out = append(out, filepath.Clean(entry))
		}
	}
	return out
}

func foldPathText(s string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(s)
	}
	return s
}

func foldContains(text, sub string) bool {
	return sub != "" && strings.Contains(foldPathText(text), foldPathText(sub))
}

// pathWithin reports whether path equals root or lies below it, comparing
// case-insensitively where the default filesystem does.
func pathWithin(path, root string) bool {
	path, root = foldPathText(filepath.Clean(path)), foldPathText(filepath.Clean(root))
	if path == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(path, root)
}

var versionedTool = regexp.MustCompile(`^(python|pythonw|pip|pypy|ipython|py|pyw)[0-9.]*$`)

// matchKind names the build tool of kind that a command line involves.
// Versioned and alternative interpreters (python3.12, pip3.12, pypy3,
// ipython, the py launcher) count for Python.
func matchKind(commandLine string, kind artifactKind) string {
	if tool := matchProcess(commandLine, kindTools[kind]); tool != "" {
		return tool
	}
	if kind != kindPython {
		return ""
	}
	for field := range strings.FieldsSeq(commandLine) {
		base := strings.ToLower(filepath.Base(strings.Trim(field, `"';&|()`)))
		base = strings.TrimSuffix(base, ".exe")
		if versionedTool.MatchString(base) {
			return base
		}
	}
	return ""
}

func matchAnyKind(commandLine string) string {
	for _, kind := range []artifactKind{kindRust, kindNode, kindPython} {
		if tool := matchKind(commandLine, kind); tool != "" {
			return tool
		}
	}
	return ""
}
