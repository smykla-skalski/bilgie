package osshim

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const (
	helperEnv = "OSSHIM_HELPER"
	readyEnv  = "OSSHIM_READY"
)

// TestMain doubles as a child-process fixture: the test binary re-executes
// itself as "parent" (spawns a "child") or "child" (heartbeats to a file).
func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "parent":
		os.Exit(runParent())
	case "child":
		os.Exit(runChild())
	}
	os.Exit(m.Run())
}

func runParent() int {
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), helperEnv+"=child")
	if err := child.Start(); err != nil {
		return 1
	}
	_ = child.Wait()
	return 0
}

// readyLine is what a child prints once it is running, for a parent that must
// not look for it before it has finished starting.
const readyLine = "ready"

func runChild() int {
	beat := os.Getenv("OSSHIM_HEARTBEAT")
	if os.Getenv(readyEnv) != "" {
		if _, err := fmt.Println(readyLine); err != nil {
			return 1
		}
	}
	for i := 0; ; i++ {
		if err := os.WriteFile(beat, []byte(strconv.Itoa(i)), 0o600); err != nil {
			return 1
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func heartbeatPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "beat")
}
