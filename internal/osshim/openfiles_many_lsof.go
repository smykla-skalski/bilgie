//go:build !linux && !windows

package osshim

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// OpenFilesUnder reports, for each dir, whether any process has a file open
// inside it, from one lsof listing of every open file matched in Go. An error
// means the answer is unknown and every dir must count as open.
func OpenFilesUnder(ctx context.Context, dirs []string) (map[string]bool, error) {
	path, err := exec.LookPath("lsof")
	if err != nil {
		return nil, errors.New("lsof not found")
	}
	live := make([]string, 0, len(dirs))
	resolved := make([]string, 0, len(dirs))
	open := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		r, evalErr := filepath.EvalSymlinks(dir)
		switch {
		case errors.Is(evalErr, fs.ErrNotExist):
		case evalErr != nil:
			open[dir] = true
		default:
			live, resolved = append(live, dir), append(resolved, r)
		}
	}
	if len(live) == 0 {
		return open, nil
	}

	cmd := exec.CommandContext(ctx, path, "-n", "-P", "-Fn")
	cmd.Env = append(os.Environ(), "LC_ALL=en_US.UTF-8")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	byDir := make(map[string][]string, len(resolved))
	for i, dir := range resolved {
		key := lsofKey(dir)
		byDir[key] = append(byDir[key], live[i])
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		name, ok := strings.CutPrefix(scanner.Text(), "n")
		if !ok {
			continue
		}
		markOpenAncestors(open, byDir, name)
		if decoded := decodeLsofName(name); decoded != name {
			markOpenAncestors(open, byDir, decoded)
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if scanErr != nil {
		return nil, scanErr
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 1 {
			return nil, waitErr
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("lsof could not inspect every file: %s", msg)
		}
	}
	return open, nil
}

// lsofKey is the form in which lsof names are compared to directories.
func lsofKey(path string) string {
	if runtime.GOOS == "darwin" {
		return strings.ToLower(path)
	}
	return path
}

// markOpenAncestors marks every directory in byDir that name is, or lies
// inside, by looking up name's ancestors: the cost per listed file is its
// depth, not the number of directories.
func markOpenAncestors(open map[string]bool, byDir map[string][]string, name string) {
	for p := lsofKey(name); p != "" && p != "." && p != "/"; {
		for _, dir := range byDir[p] {
			open[dir] = true
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
}

// decodeLsofName undoes lsof's escaping of unprintable bytes (\xHH, \n, ^X),
// which it applies under the C locale. Callers also test the raw name, since a
// real path may contain a caret or backslash.
func decodeLsofName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '\\' && i+3 < len(name)+0 && name[i+1] == 'x':
			if v, err := strconv.ParseUint(name[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
			b.WriteByte(c)
		case c == '\\' && i+1 < len(name) && strings.IndexByte("abfnrtv", name[i+1]) >= 0:
			b.WriteByte("\a\b\f\n\r\t\v"[strings.IndexByte("abfnrtv", name[i+1])])
			i++
		case c == '^' && i+1 < len(name) && name[i+1] >= '@' && name[i+1] <= '_':
			b.WriteByte(name[i+1] - '@')
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
