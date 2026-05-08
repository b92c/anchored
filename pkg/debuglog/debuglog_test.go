package debuglog

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jholhewres/anchored/pkg/config"
)

func TestSnippet_NoTruncation(t *testing.T) {
	in := "short"
	if got := Snippet(in, 10); got != in {
		t.Fatalf("expected pass-through, got %q", got)
	}
}

func TestSnippet_RuneAwareWithMultibyte(t *testing.T) {
	// 100 instances of "ção " (4 runes, 6 bytes each) = 400 runes / 600 bytes.
	// A byte-slice at 50 lands mid-codepoint and produces invalid UTF-8.
	in := strings.Repeat("ção ", 100)
	out := Snippet(in, 50)

	if !utf8.ValidString(out) {
		t.Fatalf("Snippet produced invalid UTF-8: %q", out)
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("expected trailing ellipsis, got %q", out)
	}
	if got := utf8.RuneCountInString(strings.TrimSuffix(out, "…")); got != 50 {
		t.Fatalf("expected 50 runes before ellipsis, got %d", got)
	}
}

func TestSnippet_ZeroOrNegative(t *testing.T) {
	if Snippet("hello", 0) != "" {
		t.Fatal("n=0 should return empty")
	}
	if Snippet("hello", -1) != "" {
		t.Fatal("n<0 should return empty")
	}
}

func TestResolve_DefaultsDisabled(t *testing.T) {
	t.Setenv("ANCHORED_DEBUG", "")
	t.Setenv("ANCHORED_DEBUG_PATH", "")
	en, _ := resolve(config.Defaults())
	if en {
		t.Fatal("default config should be disabled")
	}
}

func TestResolve_EnvEnablesOverConfig(t *testing.T) {
	t.Setenv("ANCHORED_DEBUG", "1")
	t.Setenv("ANCHORED_DEBUG_PATH", "")
	cfg := &config.Config{Debug: config.DebugConfig{Enabled: false, Path: "/tmp/x.log"}}

	en, p := resolve(cfg)
	if !en {
		t.Fatal("ANCHORED_DEBUG=1 should force enable")
	}
	if p != "/tmp/x.log" {
		t.Fatalf("config path should be preserved, got %q", p)
	}
}

func TestResolve_EnvPathOverridesConfig(t *testing.T) {
	t.Setenv("ANCHORED_DEBUG", "1")
	t.Setenv("ANCHORED_DEBUG_PATH", "/tmp/override.log")
	cfg := &config.Config{Debug: config.DebugConfig{Enabled: true, Path: "/tmp/cfg.log"}}

	_, p := resolve(cfg)
	if p != "/tmp/override.log" {
		t.Fatalf("env path should win, got %q", p)
	}
}

func TestResolve_EnvCanDisable(t *testing.T) {
	t.Setenv("ANCHORED_DEBUG", "0")
	cfg := &config.Config{Debug: config.DebugConfig{Enabled: true, Path: "/tmp/x.log"}}

	en, _ := resolve(cfg)
	if en {
		t.Fatal("ANCHORED_DEBUG=0 should force disable even when config enables")
	}
}
