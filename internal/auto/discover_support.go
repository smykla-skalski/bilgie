package auto

import (
	"context"

	"github.com/smykla-skalski/bilgie/internal/config"
)

// NeverRuns reports whether auto never cleans the provider, whatever its
// settings: docker volumes and Xcode archives, however they are named.
func NeverRuns(name string, pc config.Provider) bool {
	return neverRun(name, pc)
}

// PathProtection explains why auto will not sweep path, or returns "" when
// the path is clear. It runs the verdict a directory-pattern sweep gets,
// which adds the protection by name (Downloads, worktrees) to the same
// checks every provider gets, including the
// bounded scan for git checkouts below path, so a tree it cannot verify
// counts as protected. The error is the context's, when it ended the scan.
func PathProtection(ctx context.Context, path, home string, protected []string) (string, error) {
	return pathReason(ctx, path, true, true, home, protected)
}
