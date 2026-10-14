package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const day = 24 * time.Hour

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
}

func artifactTag() string {
	return cargoTagSignature + "\n# This file is a cache directory tag created by cargo.\n"
}

func rustProject(t *testing.T, dir string, payload int, idle time.Duration) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "Cargo.toml"), "[package]\n")
	writeFile(t, filepath.Join(dir, "src", "main.rs"), "fn main() {}\n")
	writeFile(t, filepath.Join(dir, "target", "CACHEDIR.TAG"), artifactTag())
	writeFile(t, filepath.Join(dir, "target", "debug", "deps", "big.rlib"), strings.Repeat("x", payload))
	ageTree(t, dir, idle)
}

func nodeProject(t *testing.T, dir string, payload int, idle time.Duration) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "package.json"), "{}\n")
	writeFile(t, filepath.Join(dir, "index.js"), "1\n")
	writeFile(t, filepath.Join(dir, "node_modules", "left-pad", "index.js"), strings.Repeat("y", payload))
	ageTree(t, dir, idle)
}

func pythonProject(t *testing.T, dir string, payload int, idle time.Duration) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "pyproject.toml"), "[project]\n")
	writeFile(t, filepath.Join(dir, ".venv", "pyvenv.cfg"), "home = /usr/bin\n")
	writeFile(t, filepath.Join(dir, ".venv", "lib", "site.py"), strings.Repeat("z", payload))
	ageTree(t, dir, idle)
}

func gitRepo(t *testing.T, dir string, idle time.Duration) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	when := time.Now().Add(-idle)
	writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(gitDir, "logs", "HEAD"),
		fmt.Sprintf("0 1 A <a@b.c> %d +0000\tcommit: x\n", when.Unix()))
	ageTree(t, gitDir, idle)
	require.NoError(t, os.Chtimes(dir, when, when))
}

type artifactHarness struct {
	t     *testing.T
	root  string
	p     *ProjectArtifactsProvider
	git   []string
	dirty map[string]bool
	// gitOut answers non-status git calls as (output, error) keyed by the
	// subcommand; a missing key succeeds with no output.
	gitOut map[string]gitAnswer
}

type gitAnswer struct {
	out string
	err error
}

func newArtifactHarness(t *testing.T, mutate func(*config.Provider)) *artifactHarness {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cfg := config.Provider{
		Type:    config.TypeProjectArtifacts,
		Paths:   []string{root},
		MaxSize: "1K",
		MinIdle: "30d",
		Enabled: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := NewProjectArtifactsProvider("project-artifacts", cfg)
	require.NoError(t, err)

	h := &artifactHarness{t: t, root: root, p: p, dirty: map[string]bool{}, gitOut: map[string]gitAnswer{}}
	p.home = filepath.Join(t.TempDir(), "home")
	p.openCheck = func(context.Context, string) (bool, error) { return false, nil }
	p.openMany = nil
	p.processes = func(context.Context) ([]toolProcess, error) { return nil, nil }
	p.git = func(_ context.Context, dir string, args ...string) (string, error) {
		h.git = append(h.git, dir+" "+strings.Join(args, " "))
		if len(args) > 0 && args[0] != "status" {
			a := h.gitOut[args[0]]
			return a.out, a.err
		}
		if h.dirty[dir] {
			return " M file.go\n", nil
		}
		return "", nil
	}
	return h
}

func (h *artifactHarness) path(parts ...string) string {
	return filepath.Join(append([]string{h.root}, parts...)...)
}

func (h *artifactHarness) clean(opts CleanOptions) CleanResult {
	h.t.Helper()
	h.p.invalidate()
	res, err := h.p.Clean(context.Background(), opts)
	require.NoError(h.t, err)
	return res
}

func (h *artifactHarness) entryPaths(res CleanResult) []string {
	var paths []string
	for _, e := range res.Entries {
		paths = append(paths, e.Path)
	}
	return paths
}

func TestProjectArtifacts_RemovesWholeIdleArtifacts(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 4000, 60*day)
	nodeProject(t, h.path("web"), 1000, 60*day)
	pythonProject(t, h.path("ml"), 500, 60*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, h.path("api", "target"))
	assert.NoDirExists(t, h.path("web", "node_modules"))
	assert.DirExists(t, h.path("ml", ".venv"), "python is off by default")
	assert.FileExists(t, h.path("api", "src", "main.rs"))
	assert.FileExists(t, h.path("web", "package.json"))
	assert.Equal(t, int64(4000+len(artifactTag())+1000), res.BytesCleaned)
	assert.ElementsMatch(t, []string{h.path("api", "target"), h.path("web", "node_modules")}, h.entryPaths(res))
	assert.Contains(t, res.Entries[0].Detail, "idle 60d")
	assert.Contains(t, res.Output, "removed: ")
}

func TestProjectArtifacts_PythonWhenEnabled(t *testing.T) {
	on := true
	h := newArtifactHarness(t, func(c *config.Provider) { c.Python = &on })
	pythonProject(t, h.path("ml"), 500, 60*day)
	writeFile(t, h.path("svc", "requirements-dev.txt"), "flask\n")
	writeFile(t, h.path("svc", "venv", "pyvenv.cfg"), "x\n")
	ageTree(t, h.path("svc"), 60*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, h.path("ml", ".venv"))
	assert.NoDirExists(t, h.path("svc", "venv"))
	assert.Len(t, res.Entries, 2)
}

func TestProjectArtifacts_KindFlags(t *testing.T) {
	off := false
	h := newArtifactHarness(t, func(c *config.Provider) { c.Rust, c.Node = &off, &off })
	rustProject(t, h.path("api"), 10, 60*day)
	nodeProject(t, h.path("web"), 10, 60*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Empty(t, res.Entries)
	assert.DirExists(t, h.path("api", "target"))
	assert.DirExists(t, h.path("web", "node_modules"))
}

func TestProjectArtifacts_RecentProjectUntouched(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("fresh"), 100, 2*day)
	nodeProject(t, h.path("fresh-node"), 100, 29*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Empty(t, res.Entries)
	assert.Equal(t, 2, res.SkippedEntries)
	assert.DirExists(t, h.path("fresh", "target"))
	assert.DirExists(t, h.path("fresh-node", "node_modules"))
	assert.Contains(t, res.Output, "min_idle")
}

func TestProjectArtifacts_RecentSourceFileKeepsProject(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	touch := time.Now().Add(-day)
	require.NoError(t, os.Chtimes(h.path("api", "src", "main.rs"), touch, touch))

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Empty(t, res.Entries)
	assert.DirExists(t, h.path("api", "target"))
}

func TestProjectArtifacts_MarkersAreRequired(t *testing.T) {
	on := true
	h := newArtifactHarness(t, func(c *config.Provider) { c.Python = &on })

	writeFile(t, h.path("notag", "Cargo.toml"), "x")
	writeFile(t, h.path("notag", "target", "debug", "a.bin"), "data")
	writeFile(t, h.path("badtag", "Cargo.toml"), "x")
	writeFile(t, h.path("badtag", "target", "CACHEDIR.TAG"), "Signature: deadbeef\n")
	writeFile(t, h.path("nocargo", "target", "CACHEDIR.TAG"), artifactTag())
	writeFile(t, h.path("javaish", "target", "classes", "A.class"), "data")
	writeFile(t, h.path("nopkg", "node_modules", "x", "index.js"), "data")
	writeFile(t, h.path("novenvcfg", "pyproject.toml"), "x")
	writeFile(t, h.path("novenvcfg", ".venv", "lib", "x.py"), "data")
	writeFile(t, h.path("noproj", ".venv", "pyvenv.cfg"), "x")
	for _, name := range []string{"notag", "badtag", "nocargo", "javaish", "nopkg", "novenvcfg", "noproj"} {
		ageTree(t, h.path(name), 90*day)
	}

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Empty(t, res.Entries)
	for _, dir := range [][]string{
		{"notag", "target"}, {"badtag", "target"}, {"nocargo", "target"}, {"javaish", "target"},
		{"nopkg", "node_modules"}, {"novenvcfg", ".venv"}, {"noproj", ".venv"},
	} {
		assert.DirExists(t, h.path(dir...))
	}
	size, err := h.p.CurrentSize(context.Background())
	require.NoError(t, err)
	assert.Zero(t, size)
}

func TestProjectArtifacts_CargoTagMustLeadWithSignature(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, content string
		want          bool
	}{
		{"cargo tag", artifactTag(), true},
		{"crlf", cargoTagSignature + "\r\n# x\r\n", true},
		{"signature only", cargoTagSignature, true},
		{"wrong signature", "Signature: 0000\n", false},
		{"signature on second line", "# c\n" + cargoTagSignature + "\n", false},
		{"suffix garbage", cargoTagSignature + "ff\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "_"))
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			assert.Equal(t, tt.want, validCargoTag(path))
		})
	}
	assert.False(t, validCargoTag(filepath.Join(dir, "missing")))
}

func TestProjectArtifacts_DirtyTreeSkipped(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("dirty"), 100, 90*day)
	gitRepo(t, h.path("dirty"), 90*day)
	rustProject(t, h.path("clean"), 100, 90*day)
	gitRepo(t, h.path("clean"), 90*day)
	h.dirty[evalDir(t, h.path("dirty"))] = true

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("dirty", "target"))
	assert.NoDirExists(t, h.path("clean", "target"))
	assert.Contains(t, res.Output, "uncommitted changes")
	assert.Equal(t, 1, res.SkippedEntries)
	for _, call := range h.git {
		assert.Contains(t, call, "status --porcelain")
	}
}

func evalDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return resolved
}

func TestProjectArtifacts_GitFailureFailsClosed(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	gitRepo(t, h.path("api"), 90*day)
	h.p.git = func(context.Context, string, ...string) (string, error) { return "", errors.New("git: not found") }

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("api", "target"))
	assert.Contains(t, res.Output, "git status failed")
}

func TestProjectArtifacts_DirtyCheckCanBeDisabled(t *testing.T) {
	off := false
	h := newArtifactHarness(t, func(c *config.Provider) { c.SkipIfDirty = &off })
	rustProject(t, h.path("api"), 100, 90*day)
	gitRepo(t, h.path("api"), 90*day)
	h.dirty[evalDir(t, h.path("api"))] = true

	h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, h.path("api", "target"))
	assert.Empty(t, h.git)
}

func TestProjectArtifacts_NonGitProjectNeverAsksGit(t *testing.T) {
	h := newArtifactHarness(t, nil)
	nodeProject(t, h.path("web"), 100, 90*day)

	h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, h.path("web", "node_modules"))
	assert.Empty(t, h.git)
}

func TestProjectArtifacts_RecentGitActivityKeepsProject(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	gitRepo(t, h.path("api"), 2*day)
	require.NoError(t, os.Chtimes(h.path("api"), time.Now().Add(-90*day), time.Now().Add(-90*day)))

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Empty(t, res.Entries)
	assert.DirExists(t, h.path("api", "target"))
}

func TestProjectArtifacts_ReflogTimeCountsEvenWithOldFiles(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	gitRepo(t, h.path("api"), 90*day)
	recent := time.Now().Add(-3 * day)
	writeFile(t, h.path("api", ".git", "logs", "HEAD"),
		fmt.Sprintf("0 1 A <a@b.c> %d +0000\tcheckout: moving\n", recent.Unix()))
	old := time.Now().Add(-90 * day)
	for _, p := range []string{h.path("api", ".git", "logs", "HEAD"), h.path("api", ".git", "logs"), h.path("api", ".git")} {
		require.NoError(t, os.Chtimes(p, old, old))
	}

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Empty(t, res.Entries)
}

func linkedWorktree(t *testing.T, main, wt string, headIdle time.Duration) {
	t.Helper()
	admin := filepath.Join(main, ".git", "worktrees", filepath.Base(wt))
	when := time.Now().Add(-headIdle)
	writeFile(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/feature\n")
	writeFile(t, filepath.Join(admin, "logs", "HEAD"), fmt.Sprintf("0 1 A <a@b.c> %d +0000\tcommit: x\n", when.Unix()))
	ageTree(t, admin, headIdle)
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+admin+"\n")
	require.NoError(t, os.Chtimes(filepath.Join(wt, ".git"), when, when))
}

func TestProjectArtifacts_WorktreeUsesItsOwnHead(t *testing.T) {
	h := newArtifactHarness(t, nil)
	main := h.path("main")
	rustProject(t, main, 100, 90*day)
	gitRepo(t, main, 1*day)

	idleWT := h.path("wt-idle")
	rustProject(t, idleWT, 100, 90*day)
	linkedWorktree(t, main, idleWT, 90*day)
	ageTree(t, idleWT, 90*day)

	activeWT := h.path("wt-active")
	rustProject(t, activeWT, 100, 90*day)
	linkedWorktree(t, main, activeWT, 1*day)
	require.NoError(t, os.Chtimes(activeWT, time.Now().Add(-90*day), time.Now().Add(-90*day)))

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, filepath.Join(idleWT, "target"))
	assert.FileExists(t, filepath.Join(idleWT, ".git"), "the worktree itself stays")
	assert.DirExists(t, filepath.Join(activeWT, "target"))
	assert.DirExists(t, filepath.Join(main, "target"))
	assert.Len(t, res.Entries, 1)
}

func TestProjectArtifacts_UnreadableGitFileFailsClosed(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	writeFile(t, h.path("api", ".git"), "gitdir: /does/not/exist\n")
	ageTree(t, h.path("api"), 90*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("api", "target"))
	assert.Contains(t, res.Output, "git state unreadable")
}

func TestProjectArtifacts_OpenFilesSkipped(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("busy"), 100, 90*day)
	rustProject(t, h.path("free"), 100, 90*day)
	var probed []string
	h.p.openCheck = func(_ context.Context, dir string) (bool, error) {
		probed = append(probed, dir)
		return strings.Contains(dir, "busy"), nil
	}

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("busy", "target"))
	assert.NoDirExists(t, h.path("free", "target"))
	assert.Contains(t, res.Output, "has open files")
	assert.Len(t, probed, 3, "both in the pass, and again right before the one removal")
}

func TestProjectArtifacts_OpenCheckErrorFailsClosed(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	h.p.openCheck = func(context.Context, string) (bool, error) { return false, errors.New("lsof not found") }

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("api", "target"))
	assert.Contains(t, res.Output, "open-file check failed")
}

func TestProjectArtifacts_OpenFilesUnsupportedRelyOnRename(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	h.p.openCheck = func(context.Context, string) (bool, error) { return false, osshim.ErrOpenFilesUnsupported }

	h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, h.path("api", "target"))
}

func TestProjectArtifacts_OpenCheckCanBeDisabled(t *testing.T) {
	off := false
	h := newArtifactHarness(t, func(c *config.Provider) { c.SkipIfOpen = &off })
	rustProject(t, h.path("api"), 100, 90*day)
	h.p.openCheck = func(context.Context, string) (bool, error) { return true, nil }

	h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, h.path("api", "target"))
}

func TestProjectArtifacts_RunningToolSkipsMatchingKindOnly(t *testing.T) {
	proj := func(h *artifactHarness) string { return evalDir(t, h.path("both")) }
	tests := []struct {
		name      string
		procs     func(project string) []toolProcess
		procsErr  error
		wantRust  bool
		wantNode  bool
		wantReasn string
	}{
		{"no tools", func(string) []toolProcess { return nil }, nil, false, false, ""},
		{"cargo in project", func(p string) []toolProcess {
			return []toolProcess{{Tool: "cargo", CommandLine: "cargo build", Cwd: p}}
		}, nil, true, false, "cargo is running"},
		{"cargo in subdir", func(p string) []toolProcess {
			return []toolProcess{{Tool: "cargo", CommandLine: "cargo test", Cwd: filepath.Join(p, "src")}}
		}, nil, true, false, "cargo is running"},
		{"rustc names project", func(p string) []toolProcess {
			return []toolProcess{{Tool: "rustc", CommandLine: "rustc --out-dir " + p + "/target/debug"}}
		}, nil, true, false, "rustc is running"},
		{"node in project", func(p string) []toolProcess {
			return []toolProcess{{Tool: "node", CommandLine: "node server.js", Cwd: p}}
		}, nil, false, true, "node is running"},
		{"pnpm in project", func(p string) []toolProcess {
			return []toolProcess{{Tool: "pnpm", CommandLine: "pnpm dev", Cwd: p}}
		}, nil, false, true, "pnpm is running"},
		{"cargo elsewhere", func(string) []toolProcess {
			return []toolProcess{{Tool: "cargo", CommandLine: "cargo build", Cwd: "/somewhere/else"}}
		}, nil, false, false, ""},
		{"cargo with unreadable cwd", func(string) []toolProcess {
			return []toolProcess{{Tool: "cargo", CommandLine: "cargo build"}}
		}, nil, true, false, "cannot be read"},
		{"cwd lookup failed", func(string) []toolProcess {
			return []toolProcess{{Tool: "cargo", CommandLine: "cargo build", CwdUnknown: true}}
		}, nil, true, false, "cannot be read"},
		{"process listing failed", func(string) []toolProcess { return nil }, errors.New("ps failed"), true, true, "cannot list processes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newArtifactHarness(t, nil)
			rustProject(t, h.path("both"), 100, 90*day)
			nodeProject(t, h.path("both"), 100, 90*day)
			project := proj(h)
			h.p.processes = func(context.Context) ([]toolProcess, error) { return tt.procs(project), tt.procsErr }

			res := h.clean(CleanOptions{Mode: CleanModeFull})

			assert.Equal(t, tt.wantRust, dirExists(h.path("both", "target")), "rust target kept")
			assert.Equal(t, tt.wantNode, dirExists(h.path("both", "node_modules")), "node_modules kept")
			assert.Contains(t, res.Output, tt.wantReasn)
		})
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func TestProjectArtifacts_ToolInEnclosingRepoDirectoryBlocksNestedProject(t *testing.T) {
	h := newArtifactHarness(t, nil)
	repo := h.path("mono")
	nodeProject(t, filepath.Join(repo, "packages", "a"), 100, 90*day)
	gitRepo(t, repo, 90*day)
	ageTree(t, filepath.Join(repo, "packages"), 90*day)
	mono := evalDir(t, repo)
	h.p.processes = func(context.Context) ([]toolProcess, error) {
		return []toolProcess{{Tool: "pnpm", CommandLine: "pnpm -r build", Cwd: mono}}, nil
	}

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, filepath.Join(repo, "packages", "a", "node_modules"))
	assert.Contains(t, res.Output, "pnpm is running in the repository")
}

func TestProjectArtifacts_MonorepoNestedProjectsFoundAtOwnLevel(t *testing.T) {
	h := newArtifactHarness(t, nil)
	repo := h.path("mono")
	nodeProject(t, repo, 100, 90*day)
	nodeProject(t, filepath.Join(repo, "packages", "a"), 100, 90*day)
	nodeProject(t, filepath.Join(repo, "packages", "b"), 100, 90*day)
	writeFile(t, filepath.Join(repo, "node_modules", "pkg", "package.json"), "{}")
	writeFile(t, filepath.Join(repo, "node_modules", "pkg", "node_modules", "inner", "index.js"), "x")
	ageTree(t, repo, 90*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Len(t, res.Entries, 3)
	assert.NoDirExists(t, filepath.Join(repo, "packages", "a", "node_modules"))
	assert.NoDirExists(t, filepath.Join(repo, "node_modules"))
}

func TestProjectArtifacts_DepthIsBounded(t *testing.T) {
	deep := func(h *artifactHarness) string { return h.path("a", "b", "c", "d") }
	tests := []struct {
		name  string
		depth int
		want  bool
	}{
		{"default depth reaches 4", 0, true},
		{"depth 4 reaches", 4, true},
		{"depth 3 stops short", 3, false},
		{"depth 1 stops short", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newArtifactHarness(t, func(c *config.Provider) { c.MaxDepth = tt.depth })
			tooDeep := h.path("a", "b", "c", "d", "e")
			nodeProject(t, tooDeep, 10, 90*day)
			nodeProject(t, deep(h), 10, 90*day)
			ageTree(t, h.path("a"), 90*day)

			res := h.clean(CleanOptions{Mode: CleanModeFull})

			assert.Equal(t, tt.want, !dirExists(filepath.Join(deep(h), "node_modules")))
			assert.DirExists(t, filepath.Join(tooDeep, "node_modules"), "depth 5 is never reached")
			if !tt.want {
				assert.Empty(t, res.Entries)
			}
		})
	}
}

func TestProjectArtifacts_TimeBudgetStopsDiscovery(t *testing.T) {
	h := newArtifactHarness(t, nil)
	nodeProject(t, h.path("web"), 10, 90*day)
	h.p.budget = time.Second
	h.p.now = func() time.Time { return time.Now().Add(-time.Hour) }

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Empty(t, res.Entries)
	assert.DirExists(t, h.path("web", "node_modules"))
	assert.Contains(t, res.Output, "budget")
}

func TestProjectArtifacts_BudgetAppliesPerPass(t *testing.T) {
	h := newArtifactHarness(t, nil)
	nodeProject(t, h.path("web"), 10, 90*day)

	h.p.budget = time.Second
	h.p.now = func() time.Time { return time.Now().Add(-time.Hour) }
	h.clean(CleanOptions{DryRun: true, Mode: CleanModeFull})
	h.p.now = time.Now
	h.p.budget = 10 * time.Second
	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Len(t, res.Entries, 1)
	assert.NotContains(t, res.Output, "budget")
}

func TestProjectArtifacts_CancelledContextDeletesNothing(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.p.Clean(ctx, CleanOptions{Mode: CleanModeFull})

	require.ErrorIs(t, err, context.Canceled)
	assert.DirExists(t, h.path("api", "target"))
	_, err = h.p.CurrentSize(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestProjectArtifacts_CancelBetweenArtifactsStops(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("a-old"), 100, 90*day)
	rustProject(t, h.path("b-older"), 100, 120*day)
	ctx, cancel := context.WithCancel(context.Background())
	h.p.openCheck = func(context.Context, string) (bool, error) {
		cancel()
		return false, nil
	}

	res, err := h.p.Clean(ctx, CleanOptions{Mode: CleanModeFull})

	require.ErrorIs(t, err, context.Canceled)
	assert.DirExists(t, h.path("a-old", "target"))
	assert.DirExists(t, h.path("b-older", "target"))
	assert.Empty(t, res.Entries)
}

func TestProjectArtifacts_SymlinksAreNeverFollowed(t *testing.T) {
	h := newArtifactHarness(t, nil)
	outside := t.TempDir()
	rustProject(t, filepath.Join(outside, "elsewhere"), 100, 90*day)
	if err := os.Symlink(filepath.Join(outside, "elsewhere"), h.path("link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	realTarget := filepath.Join(outside, "realtarget")
	writeFile(t, filepath.Join(realTarget, "CACHEDIR.TAG"), artifactTag())
	writeFile(t, h.path("api", "Cargo.toml"), "x")
	require.NoError(t, os.Symlink(realTarget, h.path("api", "target")))
	ageTree(t, h.path("api"), 90*day)

	nodeProject(t, h.path("web"), 10, 90*day)
	writeFile(t, filepath.Join(outside, "pkgs", "x.js"), "x")
	require.NoError(t, os.MkdirAll(h.path("web", "node_modules", "scope"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(outside, "pkgs"), h.path("web", "node_modules", "scope", "linked")))
	ageTree(t, h.path("web"), 90*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, filepath.Join(outside, "elsewhere", "target"))
	assert.DirExists(t, realTarget)
	assert.FileExists(t, filepath.Join(outside, "pkgs", "x.js"))
	assert.NoDirExists(t, h.path("web", "node_modules"))
	assert.Len(t, res.Entries, 1)
}

func TestProjectArtifacts_DeterministicOldestFirst(t *testing.T) {
	build := func() (*artifactHarness, CleanResult) {
		h := newArtifactHarness(t, nil)
		rustProject(t, h.path("mid"), 100, 60*day)
		rustProject(t, h.path("oldest"), 100, 200*day)
		nodeProject(t, h.path("newest"), 100, 40*day)
		rustProject(t, h.path("tie-b"), 100, 100*day)
		rustProject(t, h.path("tie-a"), 100, 100*day)
		writeFile(t, h.path("tie-a", "package.json"), "{}")
		writeFile(t, h.path("tie-a", "node_modules", "x.js"), "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
		ageTree(t, h.path("tie-a"), 100*day)
		ageTree(t, h.path("tie-b"), 100*day)
		return h, h.clean(CleanOptions{DryRun: true, Mode: CleanModeFull})
	}

	_, first := build()
	h, second := build()
	want := func(h *artifactHarness) []string {
		return []string{
			h.path("oldest", "target"),
			h.path("tie-a", "target"),
			h.path("tie-a", "node_modules"),
			h.path("tie-b", "target"),
			h.path("mid", "target"),
			h.path("newest", "node_modules"),
		}
	}
	got := h.entryPaths(second)
	rel := func(paths []string) []string {
		out := make([]string, len(paths))
		for i, p := range paths {
			out[i] = filepath.Base(filepath.Dir(p)) + "/" + filepath.Base(p)
		}
		return out
	}
	assert.Equal(t, rel(want(h)), rel(got))
	assert.Equal(t, rel(want(h)), rel(h.entryPaths(first)))
}

func TestProjectArtifacts_DryRunDeletesNothing(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 4000, 90*day)
	nodeProject(t, h.path("web"), 1000, 90*day)

	res := h.clean(CleanOptions{DryRun: true, Mode: CleanModeFull})

	assert.Len(t, res.Entries, 2)
	assert.Positive(t, res.BytesCleaned)
	assert.Contains(t, res.Output, "would remove: ")
	assert.DirExists(t, h.path("api", "target"))
	assert.DirExists(t, h.path("web", "node_modules"))
	assert.FileExists(t, h.path("api", "target", "debug", "deps", "big.rlib"))
}

func TestProjectArtifacts_SmartModeStopsAtMaxSize(t *testing.T) {
	h := newArtifactHarness(t, func(c *config.Provider) { c.MaxSize = "6000" })
	rustProject(t, h.path("a-oldest"), 4000, 200*day)
	rustProject(t, h.path("b-middle"), 4000, 100*day)
	rustProject(t, h.path("c-newest"), 4000, 50*day)

	res := h.clean(CleanOptions{Mode: CleanModeSmart})

	assert.NoDirExists(t, h.path("a-oldest", "target"))
	assert.NoDirExists(t, h.path("b-middle", "target"))
	assert.DirExists(t, h.path("c-newest", "target"))
	assert.Len(t, res.Entries, 2)
}

func TestProjectArtifacts_SmartModeUnderLimitDoesNothing(t *testing.T) {
	h := newArtifactHarness(t, func(c *config.Provider) { c.MaxSize = "1G" })
	rustProject(t, h.path("api"), 4000, 200*day)

	res := h.clean(CleanOptions{Mode: CleanModeSmart})

	assert.Empty(t, res.Entries)
	assert.DirExists(t, h.path("api", "target"))
}

func TestProjectArtifacts_SmartModeCountsRecentArtifactsTowardTotal(t *testing.T) {
	h := newArtifactHarness(t, func(c *config.Provider) { c.MaxSize = "5000" })
	rustProject(t, h.path("old"), 3000, 100*day)
	rustProject(t, h.path("recent"), 3000, 1*day)

	res := h.clean(CleanOptions{Mode: CleanModeSmart})

	assert.NoDirExists(t, h.path("old", "target"))
	assert.DirExists(t, h.path("recent", "target"))
	assert.Len(t, res.Entries, 1)
}

func TestProjectArtifacts_PressureStopsWhenRecovered(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("a-oldest"), 4000, 200*day)
	rustProject(t, h.path("b-middle"), 4000, 100*day)
	rustProject(t, h.path("c-newest"), 4000, 50*day)
	var seen []int64

	res := h.clean(CleanOptions{Mode: CleanModeSmart, Recovered: func(freed int64) bool {
		seen = append(seen, freed)
		return freed >= 4000
	}})

	assert.NoDirExists(t, h.path("a-oldest", "target"))
	assert.DirExists(t, h.path("b-middle", "target"))
	assert.DirExists(t, h.path("c-newest", "target"))
	assert.Len(t, res.Entries, 1)
	require.NotEmpty(t, seen)
	assert.Equal(t, int64(0), seen[0])
}

func TestProjectArtifacts_PressureIgnoresMaxSize(t *testing.T) {
	h := newArtifactHarness(t, func(c *config.Provider) { c.MaxSize = "1G" })
	rustProject(t, h.path("api"), 4000, 100*day)

	res := h.clean(CleanOptions{Mode: CleanModeSmart, Recovered: func(int64) bool { return false }})

	assert.Len(t, res.Entries, 1)
}

func TestProjectArtifacts_ProtectedProjectsSkipped(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("keep", "api"), 100, 90*day)
	rustProject(t, h.path("go", "api"), 100, 90*day)
	h.p.SetProtected([]string{h.path("keep")})
	h.p.SetProtected([]string{h.path("keep")})
	assert.Equal(t, 1, strings.Count(strings.Join(h.p.protected, "\n"), h.path("keep")))

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("keep", "api", "target"))
	assert.NoDirExists(t, h.path("go", "api", "target"))
	assert.Contains(t, res.Output, "protected path")
}

func TestProjectArtifacts_DownloadsAndOpencodeElementsAreNeverSearched(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("Downloads", "api"), 100, 90*day)
	nodeProject(t, h.path("opencode", "web"), 100, 90*day)
	nodeProject(t, h.path("fine"), 100, 90*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("Downloads", "api", "target"))
	assert.DirExists(t, h.path("opencode", "web", "node_modules"))
	assert.Len(t, res.Entries, 1)
}

func TestProjectArtifacts_ProtectedRootInsideProjectBlocksIt(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	h.p.SetProtected([]string{h.path("api", "data")})
	require.NoError(t, os.MkdirAll(h.path("api", "data"), 0o750))

	h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("api", "target"))
}

func TestProjectArtifacts_GitWorktreesDirectoryIsNotProtected(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("worktrees", "feature"), 100, 90*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.Len(t, res.Entries, 1)
	assert.NoDirExists(t, h.path("worktrees", "feature", "target"))
}

func TestProjectArtifacts_BroadRootsAreRejected(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	nodeProject(t, filepath.Join(home, "proj"), 10, 90*day)
	p, err := NewProjectArtifactsProvider("project-artifacts", config.Provider{
		Type: config.TypeProjectArtifacts, Paths: []string{home, root},
		MaxSize: "1K",
	})
	require.NoError(t, err)
	p.home = home
	p.openCheck = func(context.Context, string) (bool, error) { return false, nil }
	p.openMany = nil
	p.processes = func(context.Context) ([]toolProcess, error) { return nil, nil }

	size, err := p.CurrentSize(context.Background())

	require.NoError(t, err)
	assert.Zero(t, size, "home and its parent are never scanned")
}

func TestProjectArtifacts_SubRootOfHomeIsScanned(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	nodeProject(t, filepath.Join(home, "code", "proj"), 10, 90*day)
	p, err := NewProjectArtifactsProvider("project-artifacts", config.Provider{
		Type: config.TypeProjectArtifacts, Paths: []string{filepath.Join(home, "code")}, MaxSize: "1K",
	})
	require.NoError(t, err)
	p.home = home

	size, err := p.CurrentSize(context.Background())

	require.NoError(t, err)
	assert.Positive(t, size)
}

func TestProjectArtifacts_MissingRootsAreIgnored(t *testing.T) {
	h := newArtifactHarness(t, func(c *config.Provider) {
		c.Paths = append(c.Paths, filepath.Join(os.TempDir(), "bilgie-does-not-exist-root"))
	})
	nodeProject(t, h.path("web"), 10, 90*day)

	size, err := h.p.CurrentSize(context.Background())

	require.NoError(t, err)
	assert.Positive(t, size)
}

func TestProjectArtifacts_OverlappingRootsCountOnce(t *testing.T) {
	h := newArtifactHarness(t, func(c *config.Provider) { c.Paths = append(c.Paths, filepath.Join(c.Paths[0], "web")) })
	nodeProject(t, h.path("web"), 100, 90*day)

	res := h.clean(CleanOptions{DryRun: true, Mode: CleanModeFull})

	assert.Len(t, res.Entries, 1)
}

func TestProjectArtifacts_TrashIsSweptAndNeverMistakenForAnArtifact(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	leftover := h.path("api", trashPrefix+"target-abc12345")
	writeFile(t, filepath.Join(leftover, "CACHEDIR.TAG"), artifactTag())
	writeFile(t, filepath.Join(leftover, "debug", "partial.rlib"), "half")
	nodeProject(t, h.path("web"), 100, 90*day)
	nested := h.path("web", "node_modules")
	writeFile(t, filepath.Join(h.path("web"), trashPrefix+"node_modules-zzzzzzzz", "a.js"), "x")
	ageTree(t, h.path("api"), 90*day)
	ageTree(t, h.path("web"), 90*day)

	size, err := h.p.CurrentSize(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(100+len(artifactTag())+100), size)

	dry := h.clean(CleanOptions{DryRun: true, Mode: CleanModeFull})
	assert.DirExists(t, leftover, "dry-run sweeps nothing")
	assert.NotContains(t, dry.Output, "swept")

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.NoDirExists(t, leftover)
	assert.NoDirExists(t, h.path("web", trashPrefix+"node_modules-zzzzzzzz"))
	assert.NoDirExists(t, nested)
	assert.Contains(t, res.Output, "swept: ")
}

func TestProjectArtifacts_RemoveAsideLeavesNoPartialTree(t *testing.T) {
	dir := t.TempDir()
	art := filepath.Join(dir, "target")
	writeFile(t, filepath.Join(art, "CACHEDIR.TAG"), artifactTag())
	writeFile(t, filepath.Join(art, "debug", "a.bin"), "data")

	_, gone, err := removeAside(art)

	require.NoError(t, err)
	assert.True(t, gone)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestProjectArtifacts_RenameFailureLeavesArtifactIntact(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	require.NoError(t, os.Chmod(h.path("api"), 0o500))
	t.Cleanup(func() { _ = os.Chmod(h.path("api"), 0o700) })

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.FileExists(t, h.path("api", "target", "CACHEDIR.TAG"))
	assert.FileExists(t, h.path("api", "target", "debug", "deps", "big.rlib"))
	assert.Empty(t, res.Entries)
	assert.Contains(t, res.Output, "cannot rename aside")
}

func TestProjectArtifacts_MarkerRemovedDuringChecksBlocksRemoval(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	h.p.openCheck = func(context.Context, string) (bool, error) {
		return false, os.Remove(h.path("api", "Cargo.toml"))
	}

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("api", "target"))
	assert.Contains(t, res.Output, "markers changed")
}

func TestProjectArtifacts_ActivityDuringChecksBlocksRemoval(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	h.p.openCheck = func(context.Context, string) (bool, error) {
		now := time.Now()
		return false, os.Chtimes(h.path("api", "src", "main.rs"), now, now)
	}

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("api", "target"))
	assert.Contains(t, res.Output, "modified during checks")
}

func TestProjectArtifacts_RecoverableCountsOnlyEligible(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("idle"), 1000, 90*day)
	rustProject(t, h.path("recent"), 1000, 1*day)
	rustProject(t, h.path("dirty"), 1000, 90*day)
	gitRepo(t, h.path("dirty"), 90*day)
	h.dirty[evalDir(t, h.path("dirty"))] = true
	h.p.openCheck = func(context.Context, string) (bool, error) {
		t.Error("recoverable must not run the open-file probe")
		return true, nil
	}

	total, err := h.p.CurrentSize(context.Background())
	require.NoError(t, err)
	recoverable, err := h.p.Recoverable(context.Background())

	require.NoError(t, err)
	oneTarget := int64(1000 + len(artifactTag()))
	assert.Equal(t, 3*oneTarget, total)
	assert.Equal(t, oneTarget, recoverable)
}

func TestProjectArtifacts_ScanIsCachedBetweenSizeAndClean(t *testing.T) {
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)

	first, err := h.p.scan(context.Background())
	require.NoError(t, err)
	second, err := h.p.scan(context.Background())
	require.NoError(t, err)
	assert.Same(t, first, second)

	h.clean(CleanOptions{Mode: CleanModeFull})
	third, err := h.p.scan(context.Background())
	require.NoError(t, err)
	assert.NotSame(t, first, third)
	assert.Zero(t, third.total())
}

func TestProjectArtifacts_ConstructorValidation(t *testing.T) {
	base := config.Provider{Type: config.TypeProjectArtifacts, Paths: []string{t.TempDir()}, MaxSize: "1G"}
	tests := []struct {
		name   string
		mutate func(*config.Provider)
		errHas string
	}{
		{"valid", func(*config.Provider) {}, ""},
		{"bad min_idle", func(c *config.Provider) { c.MinIdle = "soon" }, "min_idle"},
		{"bad scan_budget", func(c *config.Provider) { c.ScanBudget = "x" }, "scan_budget"},
		{"zero scan_budget", func(c *config.Provider) { c.ScanBudget = "0s" }, "positive"},
		{"bad max_size", func(c *config.Provider) { c.MaxSize = "lots" }, "max_size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base
			tt.mutate(&cfg)
			_, err := NewProjectArtifactsProvider("project-artifacts", cfg)
			if tt.errHas == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.errHas)
		})
	}
}

func TestProjectArtifacts_Defaults(t *testing.T) {
	p, err := NewProjectArtifactsProvider("project-artifacts", config.Provider{
		Type: config.TypeProjectArtifacts, Paths: []string{t.TempDir()}, MaxSize: "1G",
	})
	require.NoError(t, err)

	assert.Equal(t, 60*day, p.minIdle)
	assert.Equal(t, 10*time.Second, p.budget)
	assert.Equal(t, 4, p.maxDepth)
	assert.True(t, p.skipDirty)
	assert.True(t, p.skipIfOpen)
	assert.Equal(t, map[artifactKind]bool{kindRust: true, kindNode: true, kindPython: false}, p.kinds)
	assert.True(t, p.Available())
}

func TestNewProvider_SelectsProjectArtifacts(t *testing.T) {
	p, err := NewProvider("project-artifacts", config.Provider{
		Type: config.TypeProjectArtifacts, Paths: []string{t.TempDir()}, MaxSize: "1G",
	})
	require.NoError(t, err)
	assert.IsType(t, &ProjectArtifactsProvider{}, p)
	assert.Implements(t, (*Recoverer)(nil), p)
	assert.Implements(t, (*ProtectionAware)(nil), p)
}

func TestLoadProviders_HandsConfiguredProtectedPathsToArtifactsProvider(t *testing.T) {
	precious := filepath.Join(t.TempDir(), "precious")
	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"project-artifacts": {
				Type: config.TypeProjectArtifacts, Paths: []string{t.TempDir()}, MaxSize: "1G", Enabled: true,
			},
		},
		Protected: []string{precious},
	}

	p, err := LoadProvider("project-artifacts", cfg)
	require.NoError(t, err)

	art, ok := p.(*ProjectArtifactsProvider)
	require.True(t, ok)
	assert.Contains(t, art.protected, filepath.Clean(precious))
}

func TestLastReflogTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "HEAD")
	content := "0 1 A B <a@b.c> 1700000000 +0200\tcommit: one\n" +
		"1 2 A B <a@b.c> 1800000000 -0500\tcheckout: moving from a to b\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	got, ok := lastReflogTime(path)

	require.True(t, ok)
	assert.Equal(t, time.Unix(1800000000, 0), got)

	require.NoError(t, os.WriteFile(path, []byte("garbage\n"), 0o600))
	_, ok = lastReflogTime(path)
	assert.False(t, ok)
	_, ok = lastReflogTime(filepath.Join(dir, "missing"))
	assert.False(t, ok)
}

func TestFindGit(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o750))
	nested := filepath.Join(repo, "pkg", "a")
	require.NoError(t, os.MkdirAll(nested, 0o750))
	plain := filepath.Join(root, "plain", "x")
	require.NoError(t, os.MkdirAll(plain, 0o750))

	gotRoot, gotGit, err := findGit(nested, root, []string{root})
	require.NoError(t, err)
	assert.Equal(t, repo, gotRoot)
	assert.Equal(t, filepath.Join(repo, ".git"), gotGit)

	gotRoot, gotGit, err = findGit(plain, root, []string{root})
	require.NoError(t, err)
	assert.Empty(t, gotRoot)
	assert.Empty(t, gotGit)
}

func TestFindGit_WalksAboveTheScanRootButStopsAtHome(t *testing.T) {
	home := t.TempDir()
	dotfiles := filepath.Join(home, ".git")
	require.NoError(t, os.MkdirAll(dotfiles, 0o750))
	repo := filepath.Join(home, "code", "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o750))
	project := filepath.Join(repo, "packages", "app")
	require.NoError(t, os.MkdirAll(project, 0o750))
	loose := filepath.Join(home, "loose", "app")
	require.NoError(t, os.MkdirAll(loose, 0o750))

	gotRoot, gotGit, err := findGit(project, filepath.Join(repo, "packages"), []string{home})
	require.NoError(t, err)
	assert.Equal(t, repo, gotRoot, "a scan root inside a repo must still find the repo")
	assert.Equal(t, filepath.Join(repo, ".git"), gotGit)

	gotRoot, _, err = findGit(loose, filepath.Dir(loose), []string{home})
	require.NoError(t, err)
	assert.Empty(t, gotRoot, "a repository in home does not own the projects below it")
}

func TestPathWithin(t *testing.T) {
	sep := string(filepath.Separator)
	root := sep + filepath.Join("a", "b")
	assert.True(t, pathWithin(root, root))
	assert.True(t, pathWithin(filepath.Join(root, "c"), root))
	assert.False(t, pathWithin(root+"x", root))
	assert.False(t, pathWithin(sep+"a", root))
}

func TestLexicallyProtected(t *testing.T) {
	assert.True(t, lexicallyProtected("/Users/u/Downloads/proj"))
	assert.True(t, lexicallyProtected("/home/u/.local/share/opencode/x"))
	assert.True(t, lexicallyProtected("/Users/u/Library/Developer/Xcode/Archives/2026"))
	assert.False(t, lexicallyProtected("/Users/u/sideprojects/worktrees/x"))
	assert.False(t, lexicallyProtected("/Users/u/sideprojects/downloader"))
}

func TestProjectArtifacts_SampleLimitTruncates(t *testing.T) {
	dir := t.TempDir()
	for i := range 20 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%02d", i)), "x")
	}

	_, truncated, err := sampleNewest(context.Background(), dir, 5)
	require.NoError(t, err)
	assert.True(t, truncated)

	_, truncated, err = sampleNewest(context.Background(), dir, 100)
	require.NoError(t, err)
	assert.False(t, truncated)
}

func TestProjectArtifacts_UnreadableArtifactIsSkippedWhole(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	h := newArtifactHarness(t, nil)
	nodeProject(t, h.path("web"), 100, 90*day)
	locked := h.path("web", "node_modules", "locked")
	writeFile(t, filepath.Join(locked, "x.js"), "x")
	ageTree(t, h.path("web"), 90*day)
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("web", "node_modules"))
	assert.Contains(t, res.Output, "part of it cannot be read")
}

func TestRunGit_ReportsDirtyAndCleanTrees(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	require.NoError(t, exec.Command(gitPath, "init", "-q", repo).Run())

	out, err := runGit(context.Background(), repo, "status", "--porcelain")
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(out))

	writeFile(t, filepath.Join(repo, "new.txt"), "x")
	out, err = runGit(context.Background(), repo, "status", "--porcelain")
	require.NoError(t, err)
	assert.Contains(t, out, "new.txt")

	_, err = runGit(context.Background(), filepath.Join(repo, "missing"), "status", "--porcelain")
	assert.Error(t, err)
}

func TestLookupToolProcesses_ReadsTheProcessTable(t *testing.T) {
	procs, err := lookupToolProcesses(context.Background())
	if err != nil {
		t.Skipf("process table unavailable: %v", err)
	}
	for _, proc := range procs {
		assert.NotEmpty(t, proc.Tool)
		assert.NotEmpty(t, proc.CommandLine)
	}
}

func TestProjectArtifacts_OversizedNonGitProjectFailsClosed(t *testing.T) {
	h := newArtifactHarness(t, nil)
	nodeProject(t, h.path("web"), 10, 90*day)
	for i := range sampleLimit + 5 {
		writeFile(t, h.path("web", "dist", fmt.Sprintf("f%05d.js", i)), "x")
	}
	ageTree(t, h.path("web"), 90*day)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("web", "node_modules"))
	assert.Contains(t, res.Output, "too many files")
}

func TestProjectArtifacts_ToolStartedDuringChecksBlocksRemoval(t *testing.T) {
	h := newArtifactHarness(t, nil)
	nodeProject(t, h.path("web"), 10, 90*day)
	project := evalDir(t, h.path("web"))
	calls := 0
	h.p.processes = func(context.Context) ([]toolProcess, error) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		return []toolProcess{{Tool: "node", CommandLine: "node dev.js", Cwd: project}}, nil
	}

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("web", "node_modules"))
	assert.Contains(t, res.Output, "node is running")
}

func TestMatchKind_VersionedPython(t *testing.T) {
	tests := []struct {
		cmd  string
		kind artifactKind
		want string
	}{
		{"/proj/.venv/bin/python3.12 serve.py", kindPython, "python3.12"},
		{"pip3.12 install x", kindPython, "pip3.12"},
		{"pythonw.exe app.py", kindPython, "pythonw"},
		{"python3.12 serve.py", kindNode, ""},
		{"cargo build", kindPython, ""},
		{"/usr/bin/pythonista", kindPython, ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, matchKind(tt.cmd, tt.kind), tt.cmd)
	}
	assert.Equal(t, "python3.12", matchAnyKind("python3.12 -m http.server"))
	assert.Empty(t, matchAnyKind("vim notes.txt"))
}

func TestProjectArtifacts_VersionedPythonDaemonBlocksVenv(t *testing.T) {
	on := true
	h := newArtifactHarness(t, func(c *config.Provider) { c.Python = &on })
	pythonProject(t, h.path("svc"), 100, 90*day)
	project := evalDir(t, h.path("svc"))
	h.p.processes = func(context.Context) ([]toolProcess, error) {
		return []toolProcess{{Tool: "python3.12", CommandLine: project + "/.venv/bin/python3.12 serve.py", Cwd: "/"}}, nil
	}

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("svc", ".venv"))
	assert.Contains(t, res.Output, "python3.12 is running")
}

func TestProjectArtifacts_CommandLineThroughSymlinkedRootBlocks(t *testing.T) {
	resolved := evalDirOf(t, t.TempDir())
	nodeProject(t, filepath.Join(resolved, "zz"), 10, 90*day)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(resolved, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p, err := NewProjectArtifactsProvider("project-artifacts", config.Provider{
		Type: config.TypeProjectArtifacts, Paths: []string{link}, MaxSize: "1K",
	})
	require.NoError(t, err)
	p.home = filepath.Join(t.TempDir(), "home")
	p.openCheck = func(context.Context, string) (bool, error) { return false, nil }
	p.openMany = nil
	p.processes = func(context.Context) ([]toolProcess, error) {
		return []toolProcess{{Tool: "node", CommandLine: "node " + filepath.Join(link, "zz", "server.js"), Cwd: "/nowhere"}}, nil
	}

	res, err := p.Clean(context.Background(), CleanOptions{Mode: CleanModeFull})

	require.NoError(t, err)
	assert.DirExists(t, filepath.Join(resolved, "zz", "node_modules"))
	assert.Contains(t, res.Output, "node is running")
}

func evalDirOf(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return resolved
}

func TestProjectArtifacts_MaxDepthAboveLimitIsRejected(t *testing.T) {
	_, err := NewProjectArtifactsProvider("project-artifacts", config.Provider{
		Type: config.TypeProjectArtifacts, Paths: []string{t.TempDir()}, MaxSize: "1G",
		MaxDepth: config.MaxProjectDepth + 1,
	})
	require.ErrorContains(t, err, "max_depth")
}

func TestNewProjectArtifactsProvider_AcceptsMillisecondBudgets(t *testing.T) {
	_, err := NewProjectArtifactsProvider("project-artifacts", config.Provider{
		Paths: []string{t.TempDir()}, MaxSize: "1G", ScanBudget: "500ms", PassBudget: "750ms",
	})
	require.NoError(t, err)

	_, err = NewProjectArtifactsProvider("project-artifacts", config.Provider{
		Paths: []string{t.TempDir()}, MaxSize: "1G", MinIdle: "10ms",
	})
	require.Error(t, err)
}

func dirtyIgnoredHarness(t *testing.T) *artifactHarness {
	t.Helper()
	h := newArtifactHarness(t, nil)
	rustProject(t, h.path("api"), 100, 90*day)
	gitRepo(t, h.path("api"), 90*day)
	h.dirty[evalDir(t, h.path("api"))] = true
	return h
}

func TestProjectArtifacts_DirtyRepoIgnoredArtifactCleanedUnderPressure(t *testing.T) {
	h := dirtyIgnoredHarness(t)

	res := h.clean(CleanOptions{Mode: CleanModeFull, Critical: true})

	assert.NoDirExists(t, h.path("api", "target"))
	require.Len(t, res.Entries, 1)
	assert.Contains(t, res.Entries[0].Detail, "allowed despite uncommitted changes")
}

func TestProjectArtifacts_DirtyRepoIgnoredArtifactMarkedInDryRun(t *testing.T) {
	h := dirtyIgnoredHarness(t)

	res := h.clean(CleanOptions{Mode: CleanModeFull, Critical: true, DryRun: true})

	assert.DirExists(t, h.path("api", "target"))
	assert.Contains(t, res.Output, "would remove")
	assert.Contains(t, res.Output, "allowed despite uncommitted changes")
}

func TestProjectArtifacts_DirtyRepoStaysSkippedBelowCritical(t *testing.T) {
	h := dirtyIgnoredHarness(t)

	res := h.clean(CleanOptions{Mode: CleanModeFull})

	assert.DirExists(t, h.path("api", "target"))
	assert.Contains(t, res.Output, "uncommitted changes")
	for _, call := range h.git {
		assert.Contains(t, call, "status --porcelain")
	}
}

func TestProjectArtifacts_DirtyRepoKeepsArtifactWhenNotSafelyIgnored(t *testing.T) {
	tests := []struct {
		name    string
		answers map[string]gitAnswer
		reason  string
	}{
		{"not ignored", map[string]gitAnswer{"check-ignore": {err: errors.New("exit status 1")}}, "uncommitted changes"},
		{"tracked or unignored file inside", map[string]gitAnswer{"ls-files": {out: "target/keep.rs\n"}}, "uncommitted changes"},
		{"file listing fails", map[string]gitAnswer{"ls-files": {err: errors.New("boom")}}, "git ls-files failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := dirtyIgnoredHarness(t)
			h.gitOut = tt.answers

			res := h.clean(CleanOptions{Mode: CleanModeFull, Critical: true})

			assert.DirExists(t, h.path("api", "target"))
			assert.Contains(t, res.Output, tt.reason)
			assert.Equal(t, 1, res.SkippedEntries)
		})
	}
}

func TestProjectArtifacts_DirtyIgnoredStillHonorsOtherGuards(t *testing.T) {
	h := dirtyIgnoredHarness(t)
	h.p.processes = func(context.Context) ([]toolProcess, error) {
		return []toolProcess{{Tool: "cargo", CommandLine: "cargo build", Cwd: h.path("api")}}, nil
	}

	res := h.clean(CleanOptions{Mode: CleanModeFull, Critical: true})

	assert.DirExists(t, h.path("api", "target"))
	assert.Equal(t, 1, res.SkippedEntries)

	h = dirtyIgnoredHarness(t)
	h.p.openCheck = func(context.Context, string) (bool, error) { return true, nil }

	res = h.clean(CleanOptions{Mode: CleanModeFull, Critical: true})

	assert.DirExists(t, h.path("api", "target"))
	assert.Equal(t, 1, res.SkippedEntries)
}
