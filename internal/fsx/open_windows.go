//go:build windows

package fsx

import "errors"

// Windows has no O_NOFOLLOW; the Lstat guard in openNoLink covers links.
const (
	openFlags    = 0
	noFollowFlag = 0
)

var errLoop = errors.New("unreachable on windows")
