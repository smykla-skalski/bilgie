package osshim

import "errors"

// ErrFileHoldersUnsupported is returned where the processes holding a file
// cannot be listed. The answer is unknown, so a caller must treat the file as
// held by an unknown process.
var ErrFileHoldersUnsupported = errors.New("file holders cannot be listed on windows")
