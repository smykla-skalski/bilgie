package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
		if !isSymlink(path) {
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

func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

// lockConfig takes an exclusive lock for the config file, so two writers never
// interleave their read and write. The lock lives in the state directory, not
// next to the file, which may sit in a dotfiles repository. It stays on disk:
// removing it while another process waits would let two writers both hold the
// lock. A state directory the user cannot write to leaves the write unlocked,
// as it was before locking existed.
func lockConfig(target string) (release func(), err error) {
	dir, err := StateDir()
	if err != nil {
		return withoutLock(err)
	}
	sum := sha256.Sum256([]byte(target))
	lock := filepath.Join(dir, "config-"+hex.EncodeToString(sum[:8])+".lock")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return unlocked(err)
	}
	deadline := time.Now().Add(lockWait)
	for {
		release, acquired, err := osshim.TryLock(lock)
		if err != nil {
			return unlocked(err)
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

func unlocked(err error) (func(), error) {
	if errors.Is(err, os.ErrPermission) {
		return withoutLock(err)
	}
	return nil, fmt.Errorf("lock config: %w", err)
}

func withoutLock(error) (func(), error) {
	return func() {}, nil
}

// writeAtomic replaces target with data through a temp file in the same
// directory, so a reader or a crash never sees half a file. The mode of an
// existing file is kept. In a directory the user cannot add files to, the
// file is rewritten in place instead.
func writeAtomic(target string, data []byte) (err error) {
	mode := os.FileMode(0o600)
	if info, statErr := os.Stat(target); statErr == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp-*")
	if errors.Is(err, os.ErrPermission) {
		return os.WriteFile(target, data, mode)
	}
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
