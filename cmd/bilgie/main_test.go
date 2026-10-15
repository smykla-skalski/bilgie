package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecute_PrintsAFailureOnce(t *testing.T) {
	probe := &cobra.Command{
		Use:  "probe-fail",
		RunE: func(*cobra.Command, []string) error { return errors.New("boom") },
	}
	rootCmd.AddCommand(probe)
	var errOut bytes.Buffer
	rootCmd.SetErr(&errOut)
	rootCmd.SetOut(&errOut)
	rootCmd.SetArgs([]string{"probe-fail"})
	t.Cleanup(func() {
		rootCmd.RemoveCommand(probe)
		rootCmd.SetErr(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetArgs(nil)
	})

	code := execute(&errOut)

	assert.Equal(t, 1, code)
	assert.Equal(t, 1, strings.Count(errOut.String(), "boom"), errOut.String())
}

func TestExecute_ConfigLoadFailureDoesNotPrintUsage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "bilgie", "config.yaml"), 0o750))

	for _, args := range [][]string{{}, {"status"}, {"interactive"}, {"config", "show"}} {
		t.Run(strings.Join(append([]string{"bilgie"}, args...), " "), func(t *testing.T) {
			var out bytes.Buffer
			rootCmd.SetErr(&out)
			rootCmd.SetOut(&out)
			rootCmd.SetArgs(args)
			t.Cleanup(func() {
				rootCmd.SetErr(nil)
				rootCmd.SetOut(nil)
				rootCmd.SetArgs(nil)
			})

			code := execute(&out)

			assert.Equal(t, 1, code)
			assert.Contains(t, out.String(), "not a regular file")
			assert.NotContains(t, out.String(), "Usage:")
		})
	}
}
