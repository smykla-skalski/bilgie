//go:build !windows

package fsx

import "syscall"

const (
	openFlags    = syscall.O_NONBLOCK
	noFollowFlag = syscall.O_NOFOLLOW
)

// errLoop is what O_NOFOLLOW reports for a link at the last path element.
var errLoop = syscall.ELOOP
