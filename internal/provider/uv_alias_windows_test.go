//go:build windows

package provider

import (
	"context"
	"testing"

	"github.com/smykla-skalski/bilgie/internal/osshim"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUVProvider_SweepRemovesNothingOnWindows(t *testing.T) {
	f := newSweepFixture(t)
	f.p.processEnvs = osshim.ProcessEnvironments
	f.setProcs(f.aged, f.serverFor(envA))

	res, err := f.p.Clean(context.Background(), CleanOptions{Mode: CleanModeSmart})
	require.NoError(t, err)
	assert.Contains(t, res.SkipReason, "read process environments")
	f.requireIntact(t, envA, envB)
}
