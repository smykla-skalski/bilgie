package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
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
