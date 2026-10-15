package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/smykla-skalski/bilgie/internal/osshim"
)

const (
	maxLinkHops = 40
	lockWait    = 10 * time.Second
	lockPoll    = 10 * time.Millisecond
)

// resolveLink follows the symbolic links at path, dangling ones included, and
// returns the file a write must replace so the link itself stays a link.
func resolveLink(path string) (string, error) {
	for range maxLinkHops {
		info, err := os.Lstat(path)
		isLink := err == nil && info.Mode()&os.ModeSymlink != 0
		if !isLink {
			return path, nil
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(path), dest)
		}
		path = dest
	}
	return "", fmt.Errorf("too many levels of symbolic links: %s", path)
}

// lockConfig takes an exclusive lock next to the config file, so two writers
// never interleave their read and write. The lock file stays: removing it
// while another process waits on it would let two writers hold "the" lock.
func lockConfig(target string) (release func(), err error) {
	lock := target + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		release, acquired, err := osshim.TryLock(lock)
		if err != nil {
			return nil, fmt.Errorf("lock %s: %w", lock, err)
		}
		if acquired {
			return release, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another bilgie process holds %s", lock)
		}
		time.Sleep(lockPoll)
	}
}

// writeAtomic replaces target with data through a temp file in the same
// directory, so a reader or a crash never sees half a file. The mode of an
// existing file is kept.
func writeAtomic(target string, data []byte) (err error) {
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(target); statErr == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	err = os.Rename(tmp.Name(), target)
	return err
}
