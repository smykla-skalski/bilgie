package main

import (
	"fmt"
	"io"
	"os"

	"github.com/smykla-skalski/bilgie/internal/appname"
	"github.com/smykla-skalski/bilgie/internal/cli"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/spf13/cobra"
)

var version = "dev"

var rootCmd = &cobra.Command{
	Use:     appname.Name,
	Version: version,
	Short:   "Developer cache manager that keeps free disk space in check",
	Long: `Bilgie works like a bilge pump with a float switch: it idles while free space is above the
configured level, pumps caches out below it and sweeps hard at the critical level.
It manages developer caches on macOS, Linux and Windows with configurable size limits.`,
	Args:          cobra.NoArgs,
	SilenceErrors: true,
	SilenceUsage:  true,
	RunE:          runRoot,
}

func runRoot(_ *cobra.Command, _ []string) error {
	loader := config.NewLoader()
	return cli.RunInteractiveWithLoader(loader, false, true)
}

func init() {
	rootCmd.AddCommand(cli.StatusCmd)
	rootCmd.AddCommand(cli.CleanCmd)
	rootCmd.AddCommand(cli.ConfigCmd)
	rootCmd.AddCommand(cli.InteractiveCmd)
	rootCmd.AddCommand(cli.AutoCmd)
	rootCmd.AddCommand(cli.TickCmd)
	rootCmd.AddCommand(cli.HistoryCmd)
	rootCmd.AddCommand(cli.DoctorCmd)
	rootCmd.AddCommand(cli.DiscoverCmd)
	rootCmd.AddCommand(cli.InstallAgentCmd)
	rootCmd.AddCommand(cli.UninstallAgentCmd)
}

// execute runs the root command and prints a failure once; cobra's own
// error line is silenced so it does not repeat.
func execute(errOut io.Writer) int {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(errOut, "Error:", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(execute(os.Stderr))
}
