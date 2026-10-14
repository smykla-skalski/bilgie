//go:build windows

package osshim

import "context"

// AnyFileOpen always returns ErrOpenFilesUnsupported: handles cannot be
// listed on Windows, so the answer is unknown and counts as open.
func AnyFileOpen(context.Context, []string) (bool, error) {
	return false, ErrOpenFilesUnsupported
}
