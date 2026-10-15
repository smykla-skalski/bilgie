//go:build !linux && !windows

package osshim

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func isLinux() bool { return false }

func TestParsePIDs(t *testing.T) {
	pids, err := parsePIDs("123\n456\n")
	require.NoError(t, err)
	assert.Equal(t, []int{123, 456}, pids)

	pids, err = parsePIDs("")
	require.NoError(t, err)
	assert.Empty(t, pids)

	_, err = parsePIDs("lsof: WARNING\n")
	assert.Error(t, err)
}
