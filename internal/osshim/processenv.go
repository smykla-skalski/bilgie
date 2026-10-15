package osshim

import "errors"

// ErrProcessEnvUnsupported is returned where process environments cannot be
// read. The answer is unknown, so a caller must treat every path as in use.
var ErrProcessEnvUnsupported = errors.New("process environments cannot be read on windows")

// ProcessEnv is the environment text of one process. Text holds the
// NAME=value pairs separated by whitespace or NUL bytes, so a caller can only
// search it, not parse it.
type ProcessEnv struct {
	PID  int
	Text string
}
