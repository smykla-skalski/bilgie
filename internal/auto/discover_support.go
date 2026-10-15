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

// PathProtection explains why auto will not clean path, or returns "" when
// the path is clear. It runs the same verdict as a clean pass, including the
// bounded scan for git checkouts below path, so a tree it cannot verify
// counts as protected. The error is the context's, when it ended the scan.
func PathProtection(ctx context.Context, path, home string, protected []string) (string, error) {
	return pathReason(ctx, path, false, true, home, protected)
}
