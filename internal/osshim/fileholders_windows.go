//go:build windows

package osshim

import "context"

// FileHolders always returns ErrFileHoldersUnsupported on Windows.
func FileHolders(context.Context, string) ([]int, error) {
	return nil, ErrFileHoldersUnsupported
}
