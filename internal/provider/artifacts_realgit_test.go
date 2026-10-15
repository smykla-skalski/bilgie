package provider

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realGitProject commits a Rust project with real git, leaves an unrelated
// untracked file so the repository is dirty, and ages everything past every
// pressure tier. The provider is switched to real git, so the safety checks
// run against git's own answers instead of stubbed ones.
func realGitProject(t *testing.T, h *artifactHarness, name, gitignore string, forceTracked ...string) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))

	dir := h.path(name)
	rustProject(t, dir, 100, 90*day)
	writeFile(t, filepath.Join(dir, ".gitignore"), gitignore)
	for _, f := range forceTracked {
		writeFile(t, filepath.Join(dir, f), "tracked")
	}

	old := time.Now().Add(-90 * day).Format(time.RFC3339)
	t.Setenv("GIT_AUTHOR_DATE", old)
	t.Setenv("GIT_COMMITTER_DATE", old)
	git := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir, "-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)
		out, runErr := exec.Command(gitPath, full...).CombinedOutput()
		require.NoError(t, runErr, string(out))
	}
	git("init", "-q")
	git("add", "Cargo.toml", "src", ".gitignore")
	for _, f := range forceTracked {
		git("add", "-f", f)
	}
	git("commit", "-q", "-m", "init")

	writeFile(t, filepath.Join(dir, "NOTES.md"), "uncommitted")
	ageTree(t, dir, 90*day)
	h.p.git = runGit
	return dir
}

func TestProjectArtifacts_DirtyRepoSafetyChecksUseRealGit(t *testing.T) {
	tests := []struct {
		name         string
		gitignore    string
		forceTracked []string
		removed      bool
	}{
		{"ignored-only artifact is allowed", "target/\n", nil, true},
		{"file tracked inside an ignored artifact protects it", "target/\n", []string{"target/keep.rs"}, false},
		{"artifact git does not ignore stays protected", "\n", nil, false},
		{"partly ignored artifact keeps its unignored files", "target/debug/\n", nil, false},
	}
	for _, tt := range tests {
		for name, pressure := range map[string]Pressure{"critical": PressureCritical, "emergency": PressureEmergency} {
			t.Run(tt.name+"/"+name, func(t *testing.T) {
				h := newArtifactHarness(t, nil)
				dir := realGitProject(t, h, "api", tt.gitignore, tt.forceTracked...)

				res := h.clean(CleanOptions{Mode: CleanModeFull, Pressure: pressure})

				if tt.removed {
					assert.NoDirExists(t, filepath.Join(dir, "target"))
					require.Len(t, res.Entries, 1)
					assert.Contains(t, res.Entries[0].Detail, "allowed despite uncommitted changes")
					return
				}
				assert.DirExists(t, filepath.Join(dir, "target"))
				assert.Empty(t, res.Entries)
				assert.Contains(t, res.Output, "uncommitted changes")
			})
		}
	}
}

func TestProjectArtifacts_DirtyRepoWithIgnoredArtifactStaysProtectedWithoutPressure(t *testing.T) {
	h := newArtifactHarness(t, nil)
	dir := realGitProject(t, h, "api", "target/\n")

	res := h.clean(CleanOptions{Mode: CleanModeFull, Pressure: PressureNone})

	assert.DirExists(t, filepath.Join(dir, "target"))
	assert.Empty(t, res.Entries)
	assert.Contains(t, res.Output, "uncommitted changes")
}
