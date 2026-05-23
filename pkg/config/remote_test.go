package config

import "testing"

func TestRemoteConfig_Defaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Remote.Enabled {
		t.Error("expected Remote.Enabled=false by default")
	}
	if cfg.Remote.ServerURL != "" {
		t.Errorf("expected Remote.ServerURL empty, got %q", cfg.Remote.ServerURL)
	}
	if cfg.Remote.APIKey != "" {
		t.Errorf("expected Remote.APIKey empty, got %q", cfg.Remote.APIKey)
	}
	if len(cfg.Remote.Projects) != 0 {
		t.Errorf("expected Remote.Projects nil, got %v", cfg.Remote.Projects)
	}
}
