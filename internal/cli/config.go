package cli

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/kballard/go-shellquote"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// ConfigCmd manages configuration.
var ConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage configuration",
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Display current configuration",
	RunE:  runConfigShow,
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create default configuration file",
	RunE:  runConfigInit,
}

var configEditCmd = &cobra.Command{
	Use:   "edit",
	Short: "Open configuration in $EDITOR",
	RunE:  runConfigEdit,
}

func init() {
	ConfigCmd.AddCommand(configShowCmd)
	ConfigCmd.AddCommand(configInitCmd)
	ConfigCmd.AddCommand(configEditCmd)
}

func runConfigShow(_ *cobra.Command, _ []string) error {
	return runConfigShowWithLoader(config.NewLoader())
}

func runConfigShowWithLoader(loader *config.Loader) error {
	cfg, err := loader.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	configPath, _ := loader.Path()
	fmt.Printf("# %s\n", configPath)
	fmt.Print(string(out))
	for _, note := range defaultDriftNotes(cfg) {
		fmt.Println(note)
	}
	return pathProblems(cfg)
}

// pathProblems fails config show for an enabled provider whose paths clean and
// status would reject, so the problem does not wait for the next clean.
func pathProblems(cfg *config.Config) error {
	var lines []string
	for _, name := range cfg.AllEnabledProviders() {
		pc, _ := cfg.GetProvider(name)
		if err := pc.PathError(); err != nil {
			lines = append(lines, fmt.Sprintf("provider %s: %v", name, err))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("invalid provider paths:\n%s", strings.Join(lines, "\n"))
}

// defaultDriftNotes returns a YAML comment for each provider whose saved
// enabled flag differs from the current default, sorted by provider name.
func defaultDriftNotes(cfg *config.Config) []string {
	defaults := config.DefaultProviders()
	var notes []string
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		def, ok := defaults[name]
		if !ok || cfg.Providers[name].Enabled == def.Enabled {
			continue
		}
		notes = append(notes, fmt.Sprintf(
			"# note: %s has enabled: %t saved; the current default is %t",
			name, cfg.Providers[name].Enabled, def.Enabled))
	}
	return notes
}

func runConfigInit(_ *cobra.Command, _ []string) error {
	return runConfigInitWithLoader(config.NewLoader())
}

func runConfigInitWithLoader(loader *config.Loader) error {
	created, err := loader.InitDefault()
	if err != nil {
		return fmt.Errorf("init config: %w", err)
	}

	configPath, _ := loader.Path()
	if created {
		fmt.Printf("Created %s\n", configPath)
	} else {
		fmt.Printf("Config already exists: %s\n", configPath)
	}
	return nil
}

func runConfigEdit(_ *cobra.Command, _ []string) error {
	return runConfigEditWithLoader(config.NewLoader(), os.Getenv("EDITOR"))
}

func runConfigEditWithLoader(loader *config.Loader, editor string) error {
	if editor == "" {
		editor = "vi"
	}

	// Ensure config exists
	if _, err := loader.InitDefault(); err != nil {
		return fmt.Errorf("init config: %w", err)
	}

	configPath, err := loader.Path()
	if err != nil {
		return fmt.Errorf("get config path: %w", err)
	}

	parts, err := shellquote.Split(editor)
	if err != nil || len(parts) == 0 {
		return fmt.Errorf("invalid editor: %s", editor)
	}

	cmd := exec.Command(parts[0], append(parts[1:], configPath)...) //nolint:gosec // G204: user-provided $EDITOR command; arguments are shell-split and executed without a shell
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}
