// Command version-sync rewrites the plugin manifests so they always agree
// with the canonical version in /VERSION.
//
// Run via `make sync-version` after bumping VERSION. The tool is intentionally
// dumb: it loads each JSON file, rewrites the version-bearing fields, and
// writes back with the same indentation. Goreleaser already picks the version
// from the git tag (which should match VERSION) so it doesn't need rewriting.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	version, err := readVersion("VERSION")
	if err != nil {
		fail("read VERSION: %v", err)
	}

	if err := syncPluginJSON(filepath.Join(".claude-plugin", "plugin.json"), version); err != nil {
		fail("sync plugin.json: %v", err)
	}
	if err := syncMarketplaceJSON(filepath.Join(".claude-plugin", "marketplace.json"), version); err != nil {
		fail("sync marketplace.json: %v", err)
	}

	fmt.Printf("synced manifests to v%s\n", version)
}

func readVersion(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", fmt.Errorf("VERSION is empty")
	}
	if strings.HasPrefix(v, "v") {
		v = v[1:]
	}
	return v, nil
}

// syncPluginJSON updates the top-level "version" field in plugin.json. The
// file is small and hand-authored, so we round-trip through a generic map and
// rewrite with the same 2-space indent the file ships with.
func syncPluginJSON(path, version string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	doc["version"] = version
	return writeJSON(path, doc)
}

// syncMarketplaceJSON updates BOTH metadata.version and plugins[*].version
// (where the inner plugin name matches the manifest "name"), since the
// marketplace format duplicates the version in two places.
func syncMarketplaceJSON(path, version string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if md, ok := doc["metadata"].(map[string]any); ok {
		md["version"] = version
	}
	if plugins, ok := doc["plugins"].([]any); ok {
		for _, p := range plugins {
			if pm, ok := p.(map[string]any); ok {
				pm["version"] = version
			}
		}
	}
	return writeJSON(path, doc)
}

func writeJSON(path string, doc any) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "version-sync: "+format+"\n", args...)
	os.Exit(1)
}
