package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/smykla-skalski/bilgie/internal/appname"
)

const (
	configDir  = ".config/" + appname.Name
	configFile = "config.yaml"
)

// StateDir returns ~/.local/state/bilgie, where runs keep their locks and
// markers. It ignores XDG_STATE_HOME on purpose: install-agent runs in a shell
// and the agent under launchd, and both must find the same files.
func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", appname.Name), nil
}

// ExpandTilde replaces ~ prefix with home directory.
func ExpandTilde(path string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		return path, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}

	if path == "~" {
		return home, nil
	}

	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, filepath.FromSlash(path[2:])), nil
	}
	if runtime.GOOS == "windows" && strings.HasPrefix(path, `~\`) {
		return filepath.Join(home, filepath.FromSlash(strings.ReplaceAll(path[2:], `\`, "/"))), nil
	}

	return path, nil
}

// ExpandPaths expands ~ and globs in path patterns.
func ExpandPaths(patterns []string) ([]string, error) {
	var result []string

	for _, pattern := range patterns {
		expanded, err := ExpandTilde(pattern)
		if err != nil {
			return nil, err
		}

		if strings.ContainsAny(expanded, "*?[") {
			matches, err := filepath.Glob(expanded)
			if err != nil {
				return nil, fmt.Errorf("glob %q: %w", pattern, err)
			}
			result = append(result, matches...)
		} else {
			result = append(result, expanded)
		}
	}

	return result, nil
}

// DirPath returns ~/.config/bilgie.
func DirPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, configDir), nil
}

// Path returns ~/.config/bilgie/config.yaml.
func Path() (string, error) {
	dir, err := DirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFile), nil
}

// EnsureDir creates config directory if missing.
func EnsureDir() error {
	dir, err := DirPath()
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o750)
}

// PathsExist checks if any of the given paths exist on disk.
func PathsExist(patterns []string) bool {
	paths, err := ExpandPaths(patterns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: path expansion failed: %v\n", err)
		return false
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}
