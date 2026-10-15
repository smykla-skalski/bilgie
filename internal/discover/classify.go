package discover

import (
	"github.com/kballard/go-shellquote"
	"github.com/smykla-skalski/bilgie/internal/config"
)

// Tier says how safe it is to let bilgie clean a candidate. A regenerable
// cache is rebuilt by its tool and safe to enable. An opt-in candidate is
// costly to rebuild or depends on how the machine is used, so it is offered
// only with its reason. Protected data is never cleaned and never suggested.
type Tier string

// Tiers, from safest to never.
const (
	TierRegenerable Tier = "regenerable"
	TierOptIn       Tier = "opt-in"
	TierProtected   Tier = "protected"
)

// Action is what discover suggests for a candidate: enable a regenerable
// provider, offer an opt-in one with its reason, do nothing for a provider
// that is already on, or never touch protected data.
type Action string

// Actions.
const (
	ActionEnable Action = "enable"
	ActionOptIn  Action = "opt-in"
	ActionNone   Action = "none"
	ActionNever  Action = "never"
)

// optInReasons explains why a built-in provider is not simply regenerable.
// A provider that is off by default and missing here still gets the generic
// reason; one that is on by default and listed here keeps its tier as opt-in
// because the cost of a clean is real even though it is on.
var optInReasons = map[string]string{
	"huggingface":         "entry mtime records download time, not use, so a model in daily use can be evicted before an idle one; models are large to download again",
	"playwright":          "entry mtime records download time, not use, so a browser build in daily use can be evicted before an idle one",
	"rustup":              "toolchains are not a cache: removing one needs a reinstall",
	"codex-sessions":      "removing a rollout drops that session from codex resume",
	"chrome-devtools-mcp": "the cache root also holds browser profiles; profile directories are skipped but the rest is opt-in",
	"sqlite-compact":      "rewrites databases in place; run it only while the tool that owns them is closed",
	"uv":                  "uv decides what goes; off by default until its safety follow-ups land",
	"sail-dirs":           "removes whole stale directories under the temp dir once they pass min_idle",
	"sail-e2e-dirs":       "removes whole stale directories under the cache dir once they pass min_idle",
	"agent-scratch-dirs":  "removes whole stale directories under the temp dir that carry an owner marker",
	"docker":              "prunes every unused image and the build cache; the next build pulls and rebuilds them",
	"go-mod":              "the module cache is downloaded again, which needs the network and takes a while",
	"mise":                "removes tool versions nothing refers to; a project that pins one reinstalls it",
	"project-artifacts":   "removes build output of idle projects; the next build compiles from scratch",
}

const genericOptInReason = "off by default: its clean is costly or depends on how this machine is used"

// toolBinaries names the executable that marks a provider's tool as
// installed when the provider has no clean_cmd to read it from. An empty
// name means the provider has no tool to look for.
var toolBinaries = map[string]string{
	"cargo":          "cargo",
	"gradle":         "gradle",
	"rustup":         "rustup",
	"codex-sessions": "codex",
	"huggingface":    "huggingface-cli",
	"playwright":     "playwright",
	"lima":           "limactl",
	"gh":             "gh",
	"golangci-lint":  "golangci-lint",
	"node-gyp":       "node-gyp",
	"copilot":        "copilot",
	"sqlite-compact": "sqlite3",
	"homebrew":       "brew",
	"jetbrains":      "",
}

// toolFor returns the executable that marks the provider's tool as present,
// or "" when it has none.
func toolFor(name string, pc config.Provider) string {
	if bin, ok := toolBinaries[name]; ok {
		return bin
	}
	if pc.CleanCmd == "" {
		return ""
	}
	words, err := shellquote.Split(pc.CleanCmd)
	if err != nil || len(words) == 0 {
		return ""
	}
	return words[0]
}

// tierFor classifies a built-in provider that passed the protection checks.
func tierFor(name string, builtin config.Provider) (tier Tier, reason string) {
	if reason, ok := optInReasons[name]; ok {
		return TierOptIn, reason
	}
	if !builtin.Enabled {
		return TierOptIn, genericOptInReason
	}
	return TierRegenerable, ""
}
