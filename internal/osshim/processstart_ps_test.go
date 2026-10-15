//go:build !linux && !windows

package osshim

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEtime(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"00:07", 7 * time.Second},
		{"12:34", 12*time.Minute + 34*time.Second},
		{"01:02:03", time.Hour + 2*time.Minute + 3*time.Second},
		{"3-04:05:06", 3*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second},
	}
	for _, tt := range tests {
		got, err := parseEtime(tt.in)
		require.NoError(t, err, tt.in)
		assert.Equal(t, tt.want, got, tt.in)
	}
}

func TestParseEtimeRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "12", "a:b", "1:2:3:4", "x-01:02", "-1-01:02", "01:-2"} {
		_, err := parseEtime(in)
		require.Error(t, err, in)
	}
}
