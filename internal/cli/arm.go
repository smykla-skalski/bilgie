package cli

import (
	"errors"
	"fmt"

	"github.com/smykla-skalski/bilgie/internal/auto"
	"github.com/spf13/cobra"
)

// ArmCmd puts the agent back in preview mode.
var ArmCmd = &cobra.Command{
	Use:   "arm",
	Short: "Make the next real pass only preview (the first-run dry-run)",
	Long: `Writes the first-run marker: the next real pass is a dry-run that deletes nothing, and
only a completed preview clears the marker. install-agent arms it too. bilgie disarm undoes it.
Fails while a pass is running, and when the marker is a symbolic link.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runArm,
}

// DisarmCmd lets the next real pass delete.
var DisarmCmd = &cobra.Command{
	Use:   "disarm",
	Short: "Let the next real pass delete instead of previewing",
	Long: `Clears the first-run marker, so the next real pass deletes without a preview first. Use it
once the preview output of bilgie auto --dry-run looks right. bilgie arm undoes it, and so does
running install-agent again.
Fails while a pass is running, and when the marker is a symbolic link.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runDisarm,
}

func runArm(_ *cobra.Command, _ []string) error {
	env, err := defaultAutoEnv()
	if err != nil {
		return err
	}
	return runArmWithEnv(env)
}

func runDisarm(_ *cobra.Command, _ []string) error {
	env, err := defaultAutoEnv()
	if err != nil {
		return err
	}
	return runDisarmWithEnv(env)
}

func runArmWithEnv(env autoEnv) error {
	changed, err := auto.Arm(env.stateDir)
	if err != nil {
		return markerError("arm", err)
	}
	if changed {
		fmt.Fprintf(env.out, "armed: %s; run bilgie disarm to let it delete\n", auto.ArmedDetail)
	} else {
		fmt.Fprintf(env.out, "already armed: %s\n", auto.ArmedDetail)
	}
	return nil
}

func runDisarmWithEnv(env autoEnv) error {
	changed, err := auto.Disarm(env.stateDir)
	if err != nil {
		return markerError("disarm", err)
	}
	if changed {
		fmt.Fprintf(env.out, "disarmed: %s; run bilgie arm to go back to previewing\n", auto.DisarmedDetail)
	} else {
		fmt.Fprintf(env.out, "already disarmed: %s\n", auto.DisarmedDetail)
	}
	return nil
}

func markerError(verb string, err error) error {
	if errors.Is(err, auto.ErrPassRunning) {
		return fmt.Errorf("%s: %w; try again when it finishes", verb, err)
	}
	return fmt.Errorf("%s: %w", verb, err)
}
