//go:build windows

package osshim

import "context"

// ProcessEnvironments always returns ErrProcessEnvUnsupported on Windows.
func ProcessEnvironments(context.Context) ([]ProcessEnv, error) {
	return nil, ErrProcessEnvUnsupported
}
