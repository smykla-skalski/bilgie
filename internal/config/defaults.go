package config

import "maps"

const currentVersion = "1"

// DefaultProviders returns the builtin provider definitions for this OS.
func DefaultProviders() map[string]Provider {
	return DefaultProvidersFor(CurrentPlatform())
}

// DefaultProvidersFor returns the builtin providers that apply on p.
func DefaultProvidersFor(p Platform) map[string]Provider {
	all := make(map[string]Provider)
	for _, group := range []map[string]Provider{
		goProviders(p),
		jsProviders(p),
		systemProviders(p),
		xcodeProviders(),
		otherProviders(p),
		tempDirProviders(p),
		projectProviders(),
		extraCacheProviders(p),
	} {
		maps.Copy(all, group)
	}
	for name := range all {
		if !AppliesOn(name, p.OS) {
			delete(all, name)
		}
	}
	return all
}

// perOS picks the path list for p's OS; Linux is the fallback for other Unixes.
func perOS(p Platform, darwin, linux, windows []string) []string {
	switch p.OS {
	case OSDarwin:
		return darwin
	case OSWindows:
		return windows
	default:
		return linux
	}
}

func goProviders(p Platform) map[string]Provider {
	return map[string]Provider{
		"go-build": {
			Enabled:  true,
			Paths:    []string{p.cache("go-build")},
			MaxSize:  "10G",
			MaxAge:   "30d",
			CleanCmd: "go clean -cache",
		},
		"go-mod": {
			Enabled:  true,
			Paths:    []string{"~/go/pkg/mod"},
			MaxSize:  "5G",
			MaxAge:   "30d",
			CleanCmd: "go clean -modcache",
		},
	}
}

func jsProviders(p Platform) map[string]Provider {
	return map[string]Provider{
		"npm": {
			Enabled: true,
			Paths: perOS(p,
				[]string{"~/.npm"},
				[]string{"~/.npm"},
				[]string{p.cache("npm-cache")}),
			MaxSize:  "3G",
			MaxAge:   "30d",
			CleanCmd: "npm cache clean --force",
		},
		"yarn": {
			Enabled: true,
			Paths: perOS(p,
				[]string{p.cache("Yarn")},
				[]string{p.cache("yarn")},
				[]string{p.cache("Yarn/Cache")}),
			MaxSize:  "2G",
			MaxAge:   "30d",
			CleanCmd: "yarn cache clean",
		},
		"pnpm": {
			Enabled: true,
			Paths: perOS(p,
				[]string{p.data("pnpm/store"), "~/Library/pnpm/store"},
				[]string{p.data("pnpm/store")},
				[]string{p.cache("pnpm/store")}),
			MaxSize:  "5G",
			MaxAge:   "30d",
			CleanCmd: "pnpm store prune",
		},
	}
}

func systemProviders(p Platform) map[string]Provider {
	dockerPaths := perOS(p,
		[]string{"~/Library/Containers/com.docker.docker"},
		[]string{"~/.docker/desktop"},
		[]string{p.cache("Docker")})
	return map[string]Provider{
		"homebrew": {
			Enabled:  true,
			Paths:    []string{p.cache("Homebrew")},
			MaxSize:  "5G",
			MaxAge:   "30d",
			CleanCmd: "brew cleanup -s",
		},
		"mise": {
			Enabled:  true,
			Paths:    misePaths(p),
			MaxSize:  "8G",
			MaxAge:   "30d",
			CleanCmd: "mise prune",
		},
		"docker": {
			Enabled:  true,
			Paths:    dockerPaths,
			MaxSize:  "50G",
			MaxAge:   "30d",
			CleanCmd: "docker system prune -af",
		},
		"docker-volumes": {
			Enabled:  false,
			Paths:    dockerPaths,
			MaxSize:  "50G",
			CleanCmd: "docker volume prune -f",
		},
	}
}

// xcodeProviders are macOS-only: Xcode and iOS Simulator caches.
func xcodeProviders() map[string]Provider {
	return map[string]Provider{
		"xcode-deriveddata": {
			Enabled:  true,
			Paths:    []string{"~/Library/Developer/Xcode/DerivedData"},
			MaxSize:  "20G",
			MaxAge:   "30d",
			CleanCmd: "",
		},
		"xcode-archives": {
			Enabled:  true,
			Paths:    []string{"~/Library/Developer/Xcode/Archives"},
			MaxSize:  "10G",
			MaxAge:   "30d",
			CleanCmd: "",
		},
		"ios-simulator": {
			Enabled:  true,
			Paths:    []string{"~/Library/Developer/CoreSimulator/Caches"},
			MaxSize:  "10G",
			MaxAge:   "30d",
			CleanCmd: "xcrun simctl delete unavailable",
		},
	}
}

func otherProviders(p Platform) map[string]Provider {
	return map[string]Provider{
		"uv": {
			Enabled: false,
			Paths: perOS(p,
				[]string{"~/.cache/uv"},
				[]string{p.cache("uv")},
				[]string{p.cache("uv/cache")}),
			MaxSize:  "4G",
			MaxAge:   "30d",
			CleanCmd: "uv cache clean",
		},
		"jetbrains": {
			Enabled:  true,
			Paths:    []string{p.cache("JetBrains")},
			MaxSize:  "3G",
			MaxAge:   "30d",
			CleanCmd: "",
		},
		"cargo": {
			Enabled:  true,
			Paths:    []string{"~/.cargo/registry", "~/.cargo/git"},
			MaxSize:  "5G",
			MaxAge:   "30d",
			CleanCmd: "",
		},
		"gradle": {
			Enabled:  true,
			Paths:    []string{"~/.gradle/caches"},
			MaxSize:  "10G",
			MaxAge:   "30d",
			CleanCmd: "",
		},
		"pip": {
			Enabled: true,
			Paths: perOS(p,
				[]string{"~/.cache/pip", p.cache("pip")},
				[]string{p.cache("pip")},
				[]string{p.cache("pip/Cache")}),
			MaxSize:  "3G",
			MaxAge:   "30d",
			CleanCmd: "pip cache purge",
		},
	}
}

// xdgCache is ~/.cache/rel, or the XDG cache root on Linux. These tools keep
// their cache there on macOS and Windows too.
func xdgCache(p Platform, rel string) string {
	if p.OS == OSLinux {
		return p.cache(rel)
	}
	return p.home(".cache/" + rel)
}

func plainCache(maxSize string, paths ...string) Provider {
	return Provider{
		Enabled: true,
		Paths:   paths,
		MaxSize: maxSize,
		MaxAge:  "30d",
	}
}

// optInCache is disabled by default: entry mtime records download time, not
// use, so a daily-use entry can be evicted before an idle one.
func optInCache(maxSize string, paths ...string) Provider {
	pr := plainCache(maxSize, paths...)
	pr.Enabled = false
	return pr
}

// extraCacheProviders covers browser, ML, tooling and toolchain caches.
// huggingface covers hub only, because the cache root also holds the login
// token. chrome-devtools-mcp skips chrome-profile-*, which holds browser
// logins and cookies. rustup is opt-in because toolchains are not a cache. codex-sessions is
// opt-in because removing a rollout drops that session from codex resume.
func extraCacheProviders(p Platform) map[string]Provider {
	edge := perOS(p,
		[]string{p.cache("Microsoft Edge")},
		[]string{p.cache("microsoft-edge")},
		[]string{
			p.cache("Microsoft/Edge/User Data/Default/Cache"),
			p.cache("Microsoft/Edge/User Data/Default/Code Cache"),
		})
	vivaldi := perOS(p,
		[]string{p.cache("Vivaldi")},
		[]string{p.cache("vivaldi")},
		[]string{
			p.cache("Vivaldi/User Data/Default/Cache"),
			p.cache("Vivaldi/User Data/Default/Code Cache"),
		})
	return map[string]Provider{
		"edge":        plainCache("3G", edge...),
		"vivaldi":     plainCache("3G", vivaldi...),
		"huggingface": optInCache("20G", xdgCache(p, "huggingface/hub")),
		"playwright":  optInCache("5G", p.cache("ms-playwright")),
		"lima":        plainCache("10G", p.cache("lima")),
		"gh":          plainCache("1G", xdgCache(p, "gh")),
		"chrome-devtools-mcp": {
			Enabled:      false,
			Paths:        []string{xdgCache(p, "chrome-devtools-mcp")},
			MaxSize:      "2G",
			MaxAge:       "30d",
			SkipPrefixes: []string{"chrome-profile-"},
		},
		"codex-sessions": {
			Enabled: false,
			Paths:   []string{"~/.codex/sessions"},
			MaxSize: "20G",
			MaxAge:  "90d",
		},
		"vscode-shipit": plainCache("1G", p.cache("com.microsoft.VSCode.ShipIt")),
		"rustup": {
			Enabled:  false,
			Paths:    []string{"~/.rustup/toolchains"},
			MaxSize:  "10G",
			CleanCmd: "rustup toolchain uninstall",
		},
	}
}

// DefaultConfig returns config with all default providers for this OS.
func DefaultConfig() *Config {
	return DefaultConfigFor(CurrentPlatform())
}

// DefaultConfigFor returns config with the default providers that apply on p.
func DefaultConfigFor(p Platform) *Config {
	return &Config{
		Version:   currentVersion,
		Providers: DefaultProvidersFor(p),
		Auto:      DefaultAuto(),
		Protected: DefaultProtected(),
		goos:      p.OS,
	}
}

// tempDirProviders are opt-in: they delete whole directories, so they stay
// disabled until the user enables them.
func tempDirProviders(p Platform) map[string]Provider {
	return map[string]Provider{
		"sail-dirs": {
			Enabled: false,
			Type:    TypeDirPattern,
			Paths:   p.tempGlobs("sail*"),
			MaxSize: "20G",
			MinIdle: "2h",
		},
	}
}

// projectRoots are the home-relative directories project-artifacts scans when
// they exist. They stay in ~/ form so a saved config is portable.
var projectRoots = []string{"~/sideprojects", "~/kong", "~/work", "~/src", "~/code", "~/projects"}

// projectProviders removes whole build artifacts of idle projects. Rust and
// Node are on by default, Python is opt-in.
func projectProviders() map[string]Provider {
	on, off := true, false
	return map[string]Provider{
		"project-artifacts": {
			Enabled: true,
			Type:    TypeProjectArtifacts,
			Paths:   append([]string(nil), projectRoots...),
			MaxSize: "20G",
			MinIdle: "60d",
			Rust:    &on,
			Node:    &on,
			Python:  &off,
		},
	}
}

// misePaths is mise's data directory plus its cache directory when that is a
// different place (on Windows both are the same directory).
func misePaths(p Platform) []string {
	data, cache := p.data("mise"), p.cache("mise")
	if data == cache {
		return []string{data}
	}
	return []string{data, cache}
}
