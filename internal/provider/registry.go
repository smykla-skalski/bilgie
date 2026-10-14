package provider

import (
	"errors"
	"fmt"
	"os"

	"github.com/smykla-skalski/bilgie/internal/config"
)

// fileBasedProviders lists providers that clean by deleting files.
var fileBasedProviders = map[string]bool{
	"edge":          true,
	"vivaldi":       true,
	"gh":            true,
	"vscode-shipit": true,
}

// entryBasedProviders lists providers that must delete whole top-level
// entries, because partial deletion corrupts them.
var entryBasedProviders = map[string]bool{
	"huggingface":         true,
	"playwright":          true,
	"lima":                true,
	"chrome-devtools-mcp": true,
}

// LoadError is a provider that failed to load from its config. Its text is
// the line shown to the user: "provider <name>: <reason>".
type LoadError struct {
	Err  error
	Name string
}

func (e *LoadError) Error() string {
	return fmt.Sprintf("provider %s: %v", e.Name, e.Err)
}

func (e *LoadError) Unwrap() error { return e.Err }

// Reason is the cause without the provider prefix.
func (e *LoadError) Reason() string { return e.Err.Error() }

// NewProvider creates a provider from config. A failure is a *LoadError.
func NewProvider(name string, cfg config.Provider) (Provider, error) {
	p, err := newProvider(name, cfg)
	if err != nil {
		if _, ok := errors.AsType[*LoadError](err); ok {
			return nil, err
		}
		return nil, &LoadError{Name: name, Err: err}
	}
	return p, nil
}

func newProvider(name string, cfg config.Provider) (Provider, error) {
	if cfg.Type == config.TypeDirPattern {
		return NewDirPatternProvider(name, cfg)
	}

	if cfg.Type == config.TypeProjectArtifacts {
		return NewProjectArtifactsProvider(name, cfg)
	}

	if name == "docker" {
		return NewDockerProvider(name, cfg)
	}

	if name == "docker-volumes" {
		return NewDockerVolumesProvider(name, cfg)
	}

	if name == "mise" {
		return NewMiseProvider(name, cfg)
	}

	if spec, ok := treeSpecs[name]; ok {
		return NewTreeProvider(name, cfg, spec)
	}

	if entryBasedProviders[name] {
		return NewEntryProvider(name, cfg)
	}

	if name == "uv" {
		return NewUVProvider(name, cfg)
	}

	if name == "sqlite-compact" {
		return NewSQLiteCompactProvider(name, cfg)
	}

	if name == "rustup" {
		return NewRustupProvider(name, cfg)
	}

	if name == "jetbrains" {
		return NewJetBrainsProvider(name, cfg)
	}

	if fileBasedProviders[name] {
		return NewFileProvider(name, cfg)
	}

	if cfg.CleanCmd == "" {
		return nil, fmt.Errorf("unknown provider %q requires clean_cmd", name)
	}

	return NewCommandProvider(name, cfg)
}

// LoadProviders creates all enabled providers from config.
func LoadProviders(cfg *config.Config) ([]Provider, error) {
	var providers []Provider

	for _, name := range cfg.EnabledProviders() {
		provCfg, ok := cfg.GetProvider(name)
		if !ok {
			continue
		}

		p, err := NewProvider(name, provCfg)
		if err != nil {
			return nil, err
		}
		applyProtected(p, cfg)

		providers = append(providers, p)
	}

	return providers, nil
}

// LoadProvider creates a single provider by name.
func LoadProvider(name string, cfg *config.Config) (Provider, error) {
	provCfg, ok := cfg.GetProvider(name)
	if !ok {
		return nil, &LoadError{Name: name, Err: errors.New("not found")}
	}

	p, err := NewProvider(name, provCfg)
	if err != nil {
		return nil, err
	}
	applyProtected(p, cfg)
	return p, nil
}

// applyProtected hands the configured protected paths to providers that
// enforce them themselves.
func applyProtected(p Provider, cfg *config.Config) {
	aware, ok := p.(ProtectionAware)
	if !ok {
		return
	}
	home, _ := os.UserHomeDir()
	aware.SetProtected(expandProtected(config.MergeProtected(cfg.Protected), home))
}
