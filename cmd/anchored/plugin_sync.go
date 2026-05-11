package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/jholhewres/anchored/pkg/config"
)

// gitFastForwardTimeout caps how long the SessionStart hook can spend pulling
// the marketplace mirror. Unreachable remote, hung TCP, prompt for
// credentials — any of those would otherwise block Claude Code's launch
// indefinitely. Hooks are best-effort, so missing a sync is fine.
const gitFastForwardTimeout = 10 * time.Second

// syncLockPath is the advisory lock acquired before mutating the marketplace
// mirror or plugin cache. Concurrent SessionStart firings (two Claude Code
// windows opening at once) compete for this lock; whichever loses skips the
// sync silently and falls back to the manual-fix notice.
func syncLockPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".anchored", "plugin_sync.lock")
}

// PluginDrift is the result of comparing the binary version against what is
// installed in the Claude Code plugin cache. It is consumed by the
// SessionStart hook to (a) notify the user via additionalContext, and (b)
// optionally fast-forward the marketplace mirror + clear the stale cache
// when config.Plugin.AutoUpdate is true.
type PluginDrift struct {
	BinaryVersion    string // current binary version (Version var)
	InstalledVersion string // version under cache dir; "" when missing
	HasDrift         bool   // true when InstalledVersion < BinaryVersion
	MarketplaceDir   string // resolved git mirror path
	CacheDir         string // resolved cache parent path
	SyncPerformed    bool   // true when AutoUpdate ran a git pull + cache rm
	SyncError        string // non-empty when AutoUpdate tried and failed
}

// detectPluginDrift inspects the Claude Code plugin cache and compares the
// installed plugin version with the binary's compile-time Version. It never
// errors — failure modes (missing dir, unparseable plugin.json, etc.) are
// captured as "no drift detected" so the hook stays best-effort.
func detectPluginDrift(cfg *config.Config, binaryVersion string) PluginDrift {
	d := PluginDrift{
		BinaryVersion:  binaryVersion,
		MarketplaceDir: cfg.Plugin.MarketplaceDir,
		CacheDir:       cfg.Plugin.CacheDir,
	}
	installed := newestInstalledVersion(cfg.Plugin.CacheDir)
	d.InstalledVersion = installed
	if installed == "" || binaryVersion == "" || binaryVersion == "dev" {
		// "dev" placeholder means the binary was built without ldflags
		// (typical local `go build`). Drift comparison is meaningless then.
		return d
	}
	if compareSemver(installed, binaryVersion) < 0 {
		d.HasDrift = true
	}
	return d
}

// newestInstalledVersion walks CacheDir for sub-directories whose name parses
// as a semver and returns the highest. Returns "" when the directory does
// not exist, no version dirs are present, or none parse cleanly.
func newestInstalledVersion(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return ""
	}
	var best string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v := strings.TrimPrefix(e.Name(), "v")
		if !looksLikeSemver(v) {
			continue
		}
		// Confirm a plugin.json lives inside — protects against half-installed
		// or unrelated directories sharing the cache root.
		if _, err := os.Stat(filepath.Join(cacheDir, e.Name(), ".claude-plugin", "plugin.json")); err != nil {
			continue
		}
		if best == "" || compareSemver(best, v) < 0 {
			best = v
		}
	}
	return best
}

// looksLikeSemver requires EXACTLY three numeric segments before any optional
// "-prerelease" suffix. Strings like "0.4.6.extra", "1.2", "abc.def.ghi", and
// "0.4.6foo" are all rejected so compareSemver never sees garbage.
func looksLikeSemver(v string) bool {
	parts := strings.SplitN(v, "-", 2)
	dots := strings.Split(parts[0], ".")
	if len(dots) != 3 {
		return false
	}
	for _, d := range dots {
		if d == "" {
			return false
		}
		if _, err := strconv.Atoi(d); err != nil {
			return false
		}
	}
	return true
}

// compareSemver returns -1/0/1 for a < b / a == b / a > b. Only the X.Y.Z
// numeric part is compared; pre-release suffixes are ignored on purpose to
// avoid confusing rc1 < rc2 lexical edge cases when a release goes out.
func compareSemver(a, b string) int {
	pa := semverTriple(a)
	pb := semverTriple(b)
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

// semverTriple parses X.Y.Z into three ints via strconv.Atoi. Pre-release
// suffixes and stray characters yield 0 for that position (still total order
// preserving relative to clean versions). Overflow is handled by Atoi.
func semverTriple(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	v = strings.SplitN(v, "-", 2)[0]
	parts := strings.SplitN(v, ".", 3)
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			// Best-effort: trim trailing non-digits so "6.extra" still
			// extracts 6 (matches the previous behavior).
			trimmed := parts[i]
			for j, r := range trimmed {
				if r < '0' || r > '9' {
					trimmed = trimmed[:j]
					break
				}
			}
			if trimmed == "" {
				out[i] = 0
				continue
			}
			n, _ = strconv.Atoi(trimmed)
		}
		out[i] = n
	}
	return out
}

// applyPluginAutoUpdate runs when config.Plugin.AutoUpdate is true and drift
// was detected. It fast-forwards the marketplace git clone and removes the
// stale cache directory; Claude Code re-installs from the updated mirror on
// its next launch. Returns the drift struct with SyncPerformed/SyncError
// filled in.
//
// Safety:
//   - Uses `git pull --ff-only` with a 10s timeout and a sanitized env
//     (GIT_TERMINAL_PROMPT=0, askpass=/bin/true) so a divergent mirror,
//     auth prompt, or unreachable remote can't hang the SessionStart hook.
//   - Holds an advisory flock at ~/.anchored/plugin_sync.lock; concurrent
//     SessionStart firings (two Claude Code windows opening at once) skip
//     silently rather than racing on the cache dir.
//   - Only removes the directory that matches the currently installed
//     version — never wipes the cache root.
//   - Every step swallows errors into SyncError; the hook never aborts.
//
// Caveat: if Claude Code already loaded hooks from the cache dir we are
// about to delete, in-flight invocations between this rm and the user's
// restart could fail with ENOENT in the shell wrapper. The window is
// seconds — the user is instructed to restart immediately. Treat as
// acceptable.
func applyPluginAutoUpdate(d PluginDrift) PluginDrift {
	if !d.HasDrift || d.InstalledVersion == "" {
		return d
	}

	unlock, locked := tryAcquireSyncLock()
	if !locked {
		d.SyncError = "another anchored sync is in progress; skipping"
		return d
	}
	defer unlock()

	if err := gitFastForward(d.MarketplaceDir); err != nil {
		d.SyncError = "git pull failed: " + err.Error()
		return d
	}

	oldCache := filepath.Join(d.CacheDir, d.InstalledVersion)
	if err := os.RemoveAll(oldCache); err != nil {
		d.SyncError = "remove cache failed: " + err.Error()
		return d
	}
	d.SyncPerformed = true
	return d
}

// tryAcquireSyncLock attempts a non-blocking exclusive flock on
// ~/.anchored/plugin_sync.lock. Returns (releaseFn, true) on success and
// (nop, false) when the lock is already held by another anchored process.
// Best-effort: when the home dir is unwritable or flock is unsupported
// (Windows), the lock is treated as held to avoid clobbering.
func tryAcquireSyncLock() (release func(), ok bool) {
	path := syncLockPath()
	if path == "" {
		return func() {}, false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}, false
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return func() {}, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true
}

func gitFastForward(dir string) error {
	if dir == "" {
		return fmt.Errorf("marketplace dir empty")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return fmt.Errorf("not a git repo: %s", dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitFastForwardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "pull", "--ff-only", "--quiet")
	cmd.Dir = dir
	// Strip anything that could open an interactive prompt: no terminal
	// prompts, no SSH askpass GUI, no credential helper UI. If auth is
	// required and not pre-cached, we'd rather fail fast and tell the user
	// than hang the hook.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/true",
		"SSH_ASKPASS=/bin/true",
		"GIT_OPTIONAL_LOCKS=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timeout after %s", gitFastForwardTimeout)
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// renderPluginUpdateNotice builds an XML snippet for the SessionStart
// additionalContext when drift is detected. Shape mirrors the rest of the
// anchored bundle (XML-tagged sections) so the agent can route on a stable
// element name.
func renderPluginUpdateNotice(d PluginDrift) string {
	if !d.HasDrift {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<anchored_plugin_update")
	fmt.Fprintf(&sb, " installed=%q binary=%q", d.InstalledVersion, d.BinaryVersion)
	if d.SyncPerformed {
		sb.WriteString(" auto_synced=\"true\"")
	}
	sb.WriteString(">\n")
	if d.SyncPerformed {
		sb.WriteString("  Plugin cache was auto-synced. Restart Claude Code to load the new hooks.\n")
	} else if d.SyncError != "" {
		sb.WriteString("  Plugin is out of date and auto-update failed: ")
		// Cap SyncError so a verbose git stderr can't blow the bundle.
		// 200 runes is enough for the meaningful first line of any
		// realistic error path here.
		errMsg := d.SyncError
		if utf8.RuneCountInString(errMsg) > 200 {
			r := []rune(errMsg)
			errMsg = string(r[:200]) + "…"
		}
		sb.WriteString(escapeText(errMsg))
		sb.WriteString("\n  Manual fix: /plugin marketplace update anchored && /plugin uninstall anchored@anchored && /plugin install anchored@anchored\n")
	} else {
		sb.WriteString("  Plugin is out of date. Run: /plugin marketplace update anchored && /plugin uninstall anchored@anchored && /plugin install anchored@anchored — then restart.\n")
	}
	sb.WriteString("</anchored_plugin_update>")
	return sb.String()
}

// pluginManifestVersion reads the version field from a plugin.json. Used by
// tests; production code goes through newestInstalledVersion which is more
// resilient to half-installed states.
func pluginManifestVersion(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	return doc.Version, nil
}
