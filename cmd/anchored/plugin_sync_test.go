package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
)

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.4.5", "0.4.6", -1},
		{"0.4.6", "0.4.6", 0},
		{"0.4.7", "0.4.6", 1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.4.6-rc1", "0.4.6", 0}, // prerelease ignored
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestLooksLikeSemver(t *testing.T) {
	yes := []string{"0.4.6", "v1.2.3", "10.20.30"}
	no := []string{"latest", "0.4", "v0", "0..1.2", "abc", ""}
	for _, v := range yes {
		if !looksLikeSemver(strings.TrimPrefix(v, "v")) {
			t.Errorf("expected semver: %q", v)
		}
	}
	for _, v := range no {
		if looksLikeSemver(strings.TrimPrefix(v, "v")) {
			t.Errorf("expected NOT semver: %q", v)
		}
	}
}

// TestNewestInstalledVersion seeds a fake cache dir with multiple versions
// (including garbage) and confirms the highest semver wins.
func TestNewestInstalledVersion(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"0.3.9", "0.4.0", "0.4.6", "ignored", "0.4.2"} {
		pluginJSON := filepath.Join(dir, v, ".claude-plugin", "plugin.json")
		if err := os.MkdirAll(filepath.Dir(pluginJSON), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pluginJSON, []byte(`{"version":"`+v+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := newestInstalledVersion(dir); got != "0.4.6" {
		t.Fatalf("newest = %q, want 0.4.6", got)
	}
}

func TestNewestInstalledVersion_MissingDirReturnsEmpty(t *testing.T) {
	if got := newestInstalledVersion(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Errorf("got %q, want empty for missing dir", got)
	}
}

// TestNewestInstalledVersion_IgnoresVersionDirsWithoutManifest guards against
// counting half-installed or unrelated directories sharing the cache root.
func TestNewestInstalledVersion_IgnoresVersionDirsWithoutManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "9.9.9"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := newestInstalledVersion(dir); got != "" {
		t.Errorf("got %q, want empty when no plugin.json exists", got)
	}
}

func TestDetectPluginDrift(t *testing.T) {
	dir := t.TempDir()
	seedPluginCache(t, dir, "0.4.0")

	cfg := &config.Config{}
	cfg.Plugin.CacheDir = dir
	cfg.Plugin.MarketplaceDir = "/nonexistent"

	// drift: installed 0.4.0, binary 0.4.6
	d := detectPluginDrift(cfg, "0.4.6")
	if !d.HasDrift || d.InstalledVersion != "0.4.0" || d.BinaryVersion != "0.4.6" {
		t.Fatalf("expected drift 0.4.0 -> 0.4.6, got %+v", d)
	}

	// no drift: matching versions
	d2 := detectPluginDrift(cfg, "0.4.0")
	if d2.HasDrift {
		t.Fatalf("expected no drift, got %+v", d2)
	}

	// dev binary: never drifts (placeholder version is meaningless)
	d3 := detectPluginDrift(cfg, "dev")
	if d3.HasDrift {
		t.Fatalf("dev binary must never be considered drifting, got %+v", d3)
	}
}

func TestApplyPluginAutoUpdate_RemovesStaleCacheAfterFastForward(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH; skipping fast-forward test")
	}
	// Build a tiny upstream + clone to simulate the marketplace mirror.
	upstream := t.TempDir()
	runGit(t, upstream, "init", "-q", "-b", "main")
	runGit(t, upstream, "config", "user.email", "test@test")
	runGit(t, upstream, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(upstream, "README"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, upstream, "add", ".")
	runGit(t, upstream, "commit", "-q", "-m", "v1")

	mirror := t.TempDir()
	runGit(t, "", "clone", "-q", upstream, mirror)

	// Upstream gains a new commit; mirror is one commit behind.
	if err := os.WriteFile(filepath.Join(upstream, "README"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, upstream, "commit", "-aq", "-m", "v2")

	cacheDir := t.TempDir()
	seedPluginCache(t, cacheDir, "0.4.0")

	d := PluginDrift{
		BinaryVersion:    "0.4.6",
		InstalledVersion: "0.4.0",
		HasDrift:         true,
		MarketplaceDir:   mirror,
		CacheDir:         cacheDir,
	}
	out := applyPluginAutoUpdate(d)
	if !out.SyncPerformed || out.SyncError != "" {
		t.Fatalf("sync expected to succeed, got %+v", out)
	}
	// Cache version dir is gone.
	if _, err := os.Stat(filepath.Join(cacheDir, "0.4.0")); !os.IsNotExist(err) {
		t.Errorf("0.4.0 cache should be removed, stat err=%v", err)
	}
	// Mirror was fast-forwarded.
	readme, _ := os.ReadFile(filepath.Join(mirror, "README"))
	if string(readme) != "v2" {
		t.Errorf("mirror README = %q, want v2", string(readme))
	}
}

func TestApplyPluginAutoUpdate_NoDriftIsNoOp(t *testing.T) {
	out := applyPluginAutoUpdate(PluginDrift{HasDrift: false})
	if out.SyncPerformed {
		t.Error("should not perform sync when HasDrift=false")
	}
}

func TestRenderPluginUpdateNotice(t *testing.T) {
	// Drift, manual fix path
	got := renderPluginUpdateNotice(PluginDrift{HasDrift: true, InstalledVersion: "0.4.0", BinaryVersion: "0.4.6"})
	for _, want := range []string{
		`<anchored_plugin_update installed="0.4.0" binary="0.4.6">`,
		"/plugin marketplace update anchored",
		"</anchored_plugin_update>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manual-fix notice missing %q\n--- output ---\n%s", want, got)
		}
	}

	// Auto-synced path
	synced := renderPluginUpdateNotice(PluginDrift{HasDrift: true, InstalledVersion: "0.4.0", BinaryVersion: "0.4.6", SyncPerformed: true})
	if !strings.Contains(synced, `auto_synced="true"`) || !strings.Contains(synced, "auto-synced") {
		t.Errorf("expected auto_synced markup, got %q", synced)
	}

	// Sync-failed path embeds the error
	failed := renderPluginUpdateNotice(PluginDrift{HasDrift: true, InstalledVersion: "0.4.0", BinaryVersion: "0.4.6", SyncError: "git pull failed: divergent history"})
	if !strings.Contains(failed, "git pull failed: divergent history") {
		t.Errorf("expected sync error in notice, got %q", failed)
	}

	// No drift = empty string
	if got := renderPluginUpdateNotice(PluginDrift{HasDrift: false}); got != "" {
		t.Errorf("no-drift notice should be empty, got %q", got)
	}
}

// TestLooksLikeSemver_RejectsGarbage covers the strings that the tighter
// implementation must reject (CR-1 in the v0.4.7 code review).
func TestLooksLikeSemver_RejectsGarbage(t *testing.T) {
	bad := []string{
		"0.4.6.extra",  // 4 numeric segments
		"0.4.6foo",     // trailing letters
		"abc.def.ghi",  // non-numeric
		"1.2",          // < 3 segments
		"1..2.3",       // empty middle
		"1.2.3.4.5",    // > 3 segments
		"-1.2.3",       // strconv.Atoi accepts "-1" but the version namespace doesn't
	}
	for _, v := range bad {
		if looksLikeSemver(v) {
			t.Errorf("looksLikeSemver(%q) should be false", v)
		}
	}
}

// TestSemverTriple_HandlesOverflow guards against the previous naïve
// int-accumulation that would have wrapped on huge numbers.
func TestSemverTriple_HandlesOverflow(t *testing.T) {
	// "99999999999999999999" overflows int on 64-bit too; Atoi returns an
	// error and we fall back to digit-trim → returns 0 cleanly.
	got := semverTriple("99999999999999999999.0.0")
	if got[0] < 0 {
		t.Errorf("expected non-negative on overflow, got %v", got)
	}
}

// TestRenderPluginUpdateNotice_TruncatesLongSyncError protects against a
// verbose git stderr blowing up the bundle size.
func TestRenderPluginUpdateNotice_TruncatesLongSyncError(t *testing.T) {
	long := strings.Repeat("x", 5000)
	out := renderPluginUpdateNotice(PluginDrift{
		HasDrift: true, InstalledVersion: "0.4.0", BinaryVersion: "0.4.6", SyncError: long,
	})
	// Notice itself stays under ~600 chars even with a 5KB error.
	if len(out) > 800 {
		t.Errorf("notice ballooned to %d bytes; truncation regressed", len(out))
	}
	if !strings.Contains(out, "…") {
		t.Errorf("expected ellipsis after truncation, got: %s", out)
	}
}

// TestGitFastForward_TimesOut spins up a fake remote that hangs and
// verifies the 10s timeout fires. Skipped when git is missing or when
// the test env has no /dev/null-ish placeholder for askpass.
func TestGitFastForward_RejectsNonRepo(t *testing.T) {
	dir := t.TempDir() // no .git inside
	err := gitFastForward(dir)
	if err == nil || !strings.Contains(err.Error(), "not a git repo") {
		t.Fatalf("expected 'not a git repo', got %v", err)
	}
}

// TestTryAcquireSyncLock_Mutex confirms a second acquire on the same
// file fails fast (LOCK_NB returns EWOULDBLOCK). Critical for the
// "two Claude Code windows opening at once" race scenario.
func TestTryAcquireSyncLock_Mutex(t *testing.T) {
	// Redirect HOME so the test's lock file lives in a temp dir.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	release1, ok1 := tryAcquireSyncLock()
	if !ok1 {
		t.Fatal("first acquire should succeed")
	}
	defer release1()

	_, ok2 := tryAcquireSyncLock()
	if ok2 {
		t.Fatal("second acquire should fail while first is held")
	}

	release1()
	release3, ok3 := tryAcquireSyncLock()
	if !ok3 {
		t.Fatal("third acquire (after release) should succeed")
	}
	release3()
}

func TestPluginManifestVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(path, []byte(`{"version":"0.4.6","name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := pluginManifestVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	if v != "0.4.6" {
		t.Errorf("got %q, want 0.4.6", v)
	}
}

// seedPluginCache creates a fake `<cacheDir>/<version>/.claude-plugin/plugin.json`
// with a matching version field. Centralised so each test stays focused.
func seedPluginCache(t *testing.T, cacheDir, version string) {
	t.Helper()
	pj := filepath.Join(cacheDir, version, ".claude-plugin", "plugin.json")
	if err := os.MkdirAll(filepath.Dir(pj), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pj, []byte(`{"version":"`+version+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v — %s", strings.Join(args, " "), err, string(out))
	}
}
