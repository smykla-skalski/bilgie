// Package fsx reads files that sit in user-controlled places without ever
// blocking on a FIFO, socket or device someone left at the path.
package fsx

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var (
	ErrNotRegular = errors.New("not a regular file")
	ErrTooLarge   = errors.New("file too large")
	ErrSymlink    = errors.New("refusing to write through a symbolic link")
)

// CheckRegular is nil for a missing path or a regular file (links followed)
// and an ErrNotRegular error for anything else, so a caller about to open the
// path with a plain os.Open never blocks on a pipe.
func CheckRegular(path string) error {
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is a %s", ErrNotRegular, path, kind(info.Mode()))
	}
	return nil
}

// OpenRegular opens a regular file for reading (links followed). Anything
// else (directory, FIFO, socket, device, a link to one) fails with
// ErrNotRegular. The file is opened non-blocking and checked again after the
// open, so a path swapped for a FIFO after a check cannot hang the caller.
func OpenRegular(path string) (*os.File, error) {
	if err := CheckRegular(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return nil, err
	}
	if err := requireRegular(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// ReadRegular reads at most limit bytes of a regular file through
// OpenRegular; a file over limit fails with ErrTooLarge.
func ReadRegular(path string, limit int64) ([]byte, error) {
	f, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, path, limit)
	}
	return data, nil
}

// WriteFileNoLink replaces the content of a regular file, creating it with
// perm when missing. A symbolic link at path fails with ErrSymlink and any
// other special file with ErrNotRegular, before anything is written; a FIFO
// swapped in after the check fails instead of blocking.
func WriteFileNoLink(path string, data []byte, perm os.FileMode) error {
	f, err := openNoLink(path, os.O_WRONLY|os.O_CREATE, perm)
	if err != nil {
		return err
	}
	err = f.Truncate(0)
	if err == nil {
		_, err = f.Write(data)
	}
	return errors.Join(err, f.Close())
}

// OpenAppendNoLink opens a regular file for reading and appending, creating
// it with perm when missing, under the same refusals as WriteFileNoLink.
func OpenAppendNoLink(path string, perm os.FileMode) (*os.File, error) {
	return openNoLink(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, perm)
}

func openNoLink(path string, flag int, perm os.FileMode) (*os.File, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return nil, fmt.Errorf("%w: %s", ErrSymlink, path)
	case err == nil && !info.Mode().IsRegular():
		return nil, fmt.Errorf("%w: %s is a %s", ErrNotRegular, path, kind(info.Mode()))
	}
	f, err := os.OpenFile(path, flag|openFlags|noFollowFlag, perm)
	if err != nil {
		if errors.Is(err, errLoop) {
			return nil, fmt.Errorf("%w: %s", ErrSymlink, path)
		}
		return nil, err
	}
	if err := requireRegular(f, path); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func requireRegular(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is a %s", ErrNotRegular, path, kind(info.Mode()))
	}
	return nil
}

func kind(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m&os.ModeNamedPipe != 0:
		return "named pipe"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeDevice != 0:
		return "device"
	}
	return "special file"
}
