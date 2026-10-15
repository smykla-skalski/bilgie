package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/smykla-skalski/bilgie/internal/config"
	"github.com/smykla-skalski/bilgie/internal/discover"
	"github.com/smykla-skalski/bilgie/internal/doctor"
	"github.com/smykla-skalski/bilgie/internal/provider"
	"github.com/spf13/cobra"
)

// DiscoverCmd suggests the providers this machine needs.
var DiscoverCmd = &cobra.Command{
	Use:   "discover [provider...]",
	Short: "Suggest the providers this machine needs",
	Long: `Looks, without changing anything, at which built-in providers have a cache or a tool on this
machine and at large directories no provider covers, and sorts each into a tier:

  regenerable  the tool rebuilds it: safe to enable
  opt-in       costly to rebuild or depends on how the machine is used; shown with the reason
  protected    never cleaned and never suggested (Downloads, git checkouts, protected paths,
               docker volumes, Xcode archives, anything a protection check cannot verify)

Unmanaged directories come with a ready dir-pattern entry. discover never deletes anything.

--write adds the providers you accept to the config file and nothing else: unrelated keys stay
as they are and no defaults are dumped into the file. It asks about each suggestion, or with --yes
takes every regenerable one without asking. Naming providers limits both to those; with --yes a
named opt-in provider is accepted too.`,
	Example: `  bilgie discover                  # list suggestions
  bilgie discover --json           # the same for tools
  bilgie discover --write          # ask about each suggestion
  bilgie discover --write --yes    # enable every regenerable suggestion
  bilgie discover --write --yes huggingface`,
	SilenceUsage: true,
	RunE:         runDiscover,
}

func init() {
	DiscoverCmd.Flags().Bool("json", false, "Output in JSON format")
	DiscoverCmd.Flags().Bool("write", false, "Add the accepted suggestions to the config file")
	DiscoverCmd.Flags().Bool("yes", false, "With --write, accept every regenerable suggestion (or each named provider) without asking")
	DiscoverCmd.Flags().Bool("all", false, "Also list providers the machine has neither a cache nor a tool for")
	DiscoverCmd.Flags().Int("unmanaged", auto.DefaultUnmanagedTop, "Number of large directories no provider covers to list (0 disables the scan)")
}

// discoverFlags are the options of one discover run.
type discoverFlags struct {
	unmanaged int
	json      bool
	write     bool
	yes       bool
	all       bool
}

// discoverEnv is everything discover touches outside the config, so tests can
// point it at a sandbox.
type discoverEnv struct {
	in           io.Reader
	out          io.Writer
	now          func() time.Time
	lookPath     func(string) (string, error)
	home         string
	roots        []string
	platform     config.Platform
	unmanagedMin int64
	interactive  bool
}

func defaultDiscoverEnv() discoverEnv {
	home, _ := os.UserHomeDir()
	return discoverEnv{
		in:          os.Stdin,
		out:         os.Stdout,
		now:         time.Now,
		lookPath:    exec.LookPath,
		home:        home,
		platform:    config.CurrentPlatform(),
		interactive: term.IsTerminal(os.Stdin.Fd()),
	}
}

// DiscoverOutput is the JSON form of a discover run. Written lists the
// providers --write added or turned on.
type DiscoverOutput struct {
	Config     string               `json:"config,omitempty"`
	Candidates []discover.Candidate `json:"candidates"`
	Written    []string             `json:"written,omitempty"`
	Incomplete bool                 `json:"incomplete,omitempty"`
}

func runDiscover(cmd *cobra.Command, args []string) error {
	var flags discoverFlags
	flags.json, _ = cmd.Flags().GetBool("json")
	flags.write, _ = cmd.Flags().GetBool("write")
	flags.yes, _ = cmd.Flags().GetBool("yes")
	flags.all, _ = cmd.Flags().GetBool("all")
	flags.unmanaged, _ = cmd.Flags().GetInt("unmanaged")
	if flags.unmanaged < 0 {
		return fmt.Errorf("--unmanaged must not be negative, got %d", flags.unmanaged)
	}
	ctx, stop := interruptContext()
	defer stop()
	return runDiscoverWithLoader(ctx, config.NewLoader(), defaultDiscoverEnv(), flags, args)
}

func runDiscoverWithLoader(ctx context.Context, loader *config.Loader, env discoverEnv, flags discoverFlags, names []string) error {
	if err := checkDiscoverFlags(env, flags); err != nil {
		return err
	}
	cfg, err := loadValidConfig(loader)
	if err != nil {
		return err
	}

	report := discover.Run(ctx, discover.Options{
		Cfg:             cfg,
		Platform:        env.platform,
		Now:             env.now(),
		Home:            env.home,
		LookPath:        env.lookPath,
		Roots:           env.roots,
		Unmanaged:       flags.unmanaged,
		UnmanagedMin:    env.unmanagedMin,
		UnmanagedBudget: auto.DefaultUnmanagedBudget,
		Sizes:           true,
		All:             flags.all || len(names) > 0,
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := narrow(&report, names); err != nil {
		return err
	}

	out := DiscoverOutput{Candidates: report.Candidates, Incomplete: report.Incomplete}
	if flags.write {
		if out.Config, err = loader.Path(); err != nil {
			return err
		}
		accepted := accept(env, report, flags, len(names) > 0)
		if out.Written, err = write(loader, accepted); err != nil {
			return err
		}
	}

	if flags.json {
		enc := json.NewEncoder(env.out)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printDiscover(env.out, out, flags)
	return nil
}

func checkDiscoverFlags(env discoverEnv, flags discoverFlags) error {
	switch {
	case flags.yes && !flags.write:
		return errors.New("--yes needs --write")
	case flags.write && !flags.yes && flags.json:
		return errors.New("--write with --json needs --yes: there is no prompt to answer")
	case flags.write && !flags.yes && !env.interactive:
		return errors.New("--write asks about each provider and stdin is not a terminal; add --yes to accept the regenerable suggestions")
	}
	return nil
}

// narrow keeps the candidates the user named. A protected one is refused, so
// naming it never reads as a way to enable it.
func narrow(report *discover.Report, names []string) error {
	if len(names) == 0 {
		return nil
	}
	var kept []discover.Candidate
	for _, name := range names {
		c, ok := findCandidate(report.Candidates, name)
		switch {
		case !ok:
			return fmt.Errorf("%q is not a built-in provider or a candidate found on this machine", name)
		case c.Tier == discover.TierProtected:
			return fmt.Errorf("%s is protected and is never suggested for enabling: %s", name, c.Reason)
		}
		kept = append(kept, c)
	}
	report.Candidates = kept
	return nil
}

func findCandidate(cands []discover.Candidate, name string) (discover.Candidate, bool) {
	i := slices.IndexFunc(cands, func(c discover.Candidate) bool { return c.Name == name })
	if i < 0 {
		return discover.Candidate{}, false
	}
	return cands[i], true
}

// accept picks the suggestions to write. With --yes and no names that is every
// regenerable one; named providers are taken whatever their tier. Without
// --yes each suggestion is put to the user.
func accept(env discoverEnv, report discover.Report, flags discoverFlags, named bool) []discover.Candidate {
	offered := report.Suggestions()
	if flags.yes {
		if named {
			return offered
		}
		return slices.DeleteFunc(offered, func(c discover.Candidate) bool { return c.Action != discover.ActionEnable })
	}

	reader := bufio.NewReader(env.in)
	var accepted []discover.Candidate
	for i := range offered {
		c := &offered[i]
		fmt.Fprintf(env.out, "%s: %s, %s", c.Name, c.Size, c.Tier)
		if c.Reason != "" {
			fmt.Fprintf(env.out, " (%s)", c.Reason)
		}
		fmt.Fprint(env.out, "\nEnable it? [y/N]: ")
		answer, err := reader.ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "y" || answer == "yes" {
			accepted = append(accepted, *c)
		}
		if err != nil {
			break
		}
	}
	return accepted
}

func write(loader *config.Loader, accepted []discover.Candidate) ([]string, error) {
	if len(accepted) == 0 {
		return nil, nil
	}
	entries := make([]config.Override, 0, len(accepted))
	for i := range accepted {
		entries = append(entries, config.Override{Name: accepted[i].Name, Define: accepted[i].Define})
	}
	written, err := loader.WriteOverrides(entries)
	if err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	return written, nil
}

func printDiscover(w io.Writer, out DiscoverOutput, flags discoverFlags) {
	if len(out.Candidates) == 0 {
		fmt.Fprintln(w, dimStyle.Render("Nothing to suggest: no built-in provider has a cache or a tool here"))
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSIZE\tAGE\tTIER\tACTION\tENABLED")
		for i := range out.Candidates {
			c := &out.Candidates[i]
			age := "-"
			if c.Modified != nil {
				age = doctor.Age(time.Duration(c.AgeSeconds) * time.Second)
			}
			mark := ""
			if c.Partial {
				mark = "+"
			}
			fmt.Fprintf(tw, "%s\t%s%s\t%s\t%s\t%s\t%t\n", c.Name, c.Size, mark, age, c.Tier, c.Action, c.Enabled)
		}
		_ = tw.Flush()
		printNotes(w, out.Candidates)
	}
	if out.Incomplete {
		fmt.Fprintln(w, dimStyle.Render("Scan stopped early; sizes may be incomplete."))
	}
	printOutcome(w, out, flags)
}

func printNotes(w io.Writer, cands []discover.Candidate) {
	fmt.Fprintln(w)
	for i := range cands {
		c := &cands[i]
		if c.Reason != "" {
			fmt.Fprintf(w, "  %s: %s\n", c.Name, c.Reason)
		}
		if c.Error != "" {
			fmt.Fprintf(w, "  %s: %s\n", c.Name, errorStyle.Render(c.Error))
		}
		if c.Kind != discover.KindUnmanaged {
			printPaths(w, c.Paths)
		}
		if c.Entry != nil && c.Suggested() {
			fmt.Fprintf(w, "    entry: type %s, path %s, min_idle %s, max_size %s\n", c.Entry.Type, c.Entry.Paths[0], c.Entry.MinIdle, c.Entry.MaxSize)
		}
	}
}

// pathsShown caps the locations listed per provider; --json has them all.
const pathsShown = 3

func printPaths(w io.Writer, paths []string) {
	for i, path := range paths {
		if i == pathsShown {
			fmt.Fprintf(w, "    and %d more\n", len(paths)-pathsShown)
			return
		}
		fmt.Fprintf(w, "    %s\n", path)
	}
}

func printOutcome(w io.Writer, out DiscoverOutput, flags discoverFlags) {
	fmt.Fprintln(w)
	switch {
	case flags.write && len(out.Written) > 0:
		fmt.Fprintf(w, "Wrote %d provider(s) to %s: %s\n", len(out.Written), out.Config, strings.Join(out.Written, ", "))
	case flags.write:
		fmt.Fprintln(w, "Nothing written.")
	case hasSuggestion(out.Candidates):
		fmt.Fprintln(w, "Nothing was changed. Enable regenerable ones with: bilgie discover --write --yes")
	default:
		fmt.Fprintln(w, "Nothing was changed.")
	}
}

func hasSuggestion(cands []discover.Candidate) bool {
	return slices.ContainsFunc(cands, discover.Candidate.Suggested)
}

// suggestionNames lists what discover offers for the config, without
// measuring anything, for the commands that only mention the count.
func suggestionNames(ctx context.Context, cfg *config.Config, home string, lookPath func(string) (string, error), newProvider func(string, config.Provider) (provider.Provider, error)) []string {
	report := discover.Run(ctx, discover.Options{
		Cfg:         cfg,
		Platform:    config.CurrentPlatform(),
		Home:        home,
		LookPath:    lookPath,
		NewProvider: newProvider,
	})
	suggested := report.Suggestions()
	names := make([]string, 0, len(suggested))
	for i := range suggested {
		names = append(names, suggested[i].Name)
	}
	return names
}
