//go:build !linux && !windows

package osshim

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestParsePS(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	procs := parsePS("    1     0 12-03:04:05 /sbin/launchd\n  812   1       01:30 bilgie clean cargo\n  900   1       ?? sleep 1\ngarbage", now)
	assert.Equal(t, []Process{
		{PID: 1, PPID: 0, CommandLine: "/sbin/launchd", Args: "/sbin/launchd", Started: now.Add(-(12*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second))},
		{PID: 812, PPID: 1, CommandLine: "bilgie clean cargo", Args: "bilgie clean cargo", Started: now.Add(-90 * time.Second)},
		{PID: 900, PPID: 1, CommandLine: "sleep 1", Args: "sleep 1"},
		{CommandLine: "garbage", Args: "garbage"},
	}, procs)
}

func TestParseElapsed(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"00:07", 7 * time.Second, true},
		{"59:59", 59*time.Minute + 59*time.Second, true},
		{"01:02:03", time.Hour + 2*time.Minute + 3*time.Second, true},
		{"19-04:05:06", 19*24*time.Hour + 4*time.Hour + 5*time.Minute + 6*time.Second, true},
		{"", 0, false},
		{"12", 0, false},
		{"x-01:02", 0, false},
		{"1:2:3:4", 0, false},
		{"ab:cd", 0, false},
		{"-5:00", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := parseElapsed(tt.in)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}
