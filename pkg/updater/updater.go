// Package updater performs background self-update from GitHub releases.
//
// On startup the MCP server invokes Run, which checks the latest release,
// downloads the matching tarball, and atomically replaces the running
// binary. The currently executing process keeps the old in-memory image
// (Linux holds the inode open via /proc/self/exe), so the swap is safe;
// the next invocation picks up the new binary.
package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRepo  = "jholhewres/anchored"
	apiURL       = "https://api.github.com/repos/%s/releases/latest"
	checkTimeout = 8 * time.Second
	dlTimeout    = 90 * time.Second
)

// Options controls a single update attempt.
type Options struct {
	Repo           string // GitHub owner/repo. Empty falls back to defaultRepo.
	CurrentVersion string // Semver without leading "v".
	BinPath        string // Path to the binary to replace. Empty resolves via os.Executable.
	Logger         *slog.Logger
}

// Run performs a non-blocking self-update. It is safe to invoke from a
// goroutine: any failure is logged and swallowed.
func Run(ctx context.Context, opts Options) {
	if os.Getenv("ANCHORED_NO_AUTOUPDATE") == "1" {
		return
	}

	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	if opts.CurrentVersion == "" {
		return
	}

	binPath := opts.BinPath
	if binPath == "" {
		exe, err := os.Executable()
		if err != nil {
			log.Debug("autoupdate: cannot resolve executable", "error", err)
			return
		}
		binPath, _ = filepath.EvalSymlinks(exe)
		if binPath == "" {
			binPath = exe
		}
	}

	// Only auto-update binaries installed under ~/.anchored/bin. Dev builds
	// (running ./bin/anchored from a checkout) must never be overwritten.
	home, _ := os.UserHomeDir()
	canonical := filepath.Join(home, ".anchored", "bin")
	if !strings.HasPrefix(binPath, canonical+string(filepath.Separator)) {
		log.Debug("autoupdate: skip, binary outside canonical dir", "path", binPath)
		return
	}

	repo := opts.Repo
	if repo == "" {
		repo = defaultRepo
	}

	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	latest, asset, err := fetchLatest(checkCtx, repo)
	if err != nil {
		log.Debug("autoupdate: check failed", "error", err)
		return
	}

	if !isNewer(latest, opts.CurrentVersion) {
		log.Debug("autoupdate: already on latest", "current", opts.CurrentVersion, "latest", latest)
		return
	}

	log.Info("autoupdate: new version available", "current", opts.CurrentVersion, "latest", latest)

	dlCtx, dlCancel := context.WithTimeout(ctx, dlTimeout)
	defer dlCancel()

	if err := downloadAndReplace(dlCtx, asset, binPath); err != nil {
		log.Warn("autoupdate: install failed", "error", err)
		return
	}

	log.Info("autoupdate: installed, restart MCP server to activate", "version", latest, "path", binPath)
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatest(ctx context.Context, repo string) (version string, assetURL string, err error) {
	url := fmt.Sprintf(apiURL, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "anchored-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("github releases: HTTP %d", resp.StatusCode)
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", fmt.Errorf("decode release: %w", err)
	}

	version = strings.TrimPrefix(rel.TagName, "v")
	if version == "" {
		return "", "", errors.New("empty tag_name in release")
	}

	wantSuffix := fmt.Sprintf("_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, wantSuffix) {
			return version, a.BrowserDownloadURL, nil
		}
	}
	return "", "", fmt.Errorf("no asset for %s/%s with version %s", runtime.GOOS, runtime.GOARCH, version)
}

func downloadAndReplace(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "anchored-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	tmpPath := dst + ".new"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}

	written := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "anchored" {
			continue
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("write tmp: %w", err)
		}
		written = true
		break
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close tmp: %w", err)
	}
	if !written {
		os.Remove(tmpPath)
		return errors.New("anchored binary not found in tarball")
	}

	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// isNewer reports whether latest > current using lexical semver split.
// Pre-release suffixes (-rc1) compare lexicographically; "1.2.3" beats "1.2.3-rc1".
func isNewer(latest, current string) bool {
	a := splitSemver(latest)
	b := splitSemver(current)
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	// Equal numeric parts: stable (no suffix) > pre-release.
	la := preRelease(latest)
	lc := preRelease(current)
	if la == "" && lc != "" {
		return true
	}
	if la != "" && lc == "" {
		return false
	}
	return la > lc
}

func splitSemver(v string) [3]int {
	v = strings.SplitN(v, "-", 2)[0]
	parts := strings.SplitN(v, ".", 3)
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		out[i], _ = strconv.Atoi(parts[i])
	}
	return out
}

func preRelease(v string) string {
	if i := strings.Index(v, "-"); i >= 0 {
		return v[i+1:]
	}
	return ""
}
