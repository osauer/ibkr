package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "no-such.toml"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	res, err := cfg.Resolve("")
	if err != nil {
		t.Fatalf("Resolve empty: %v", err)
	}
	if res.ProfileName != "live" {
		t.Errorf("default profile = %q, want live", res.ProfileName)
	}
	if res.Profile.Port != 4001 {
		t.Errorf("default port = %d, want 4001", res.Profile.Port)
	}
	if res.Profile.ClientID != 15 {
		t.Errorf("default client_id = %d, want 15", res.Profile.ClientID)
	}
	if res.Daemon.IdleTimeout.Std() != 30*time.Minute {
		t.Errorf("default idle = %v, want 30m", res.Daemon.IdleTimeout.Std())
	}
	if _, ok := res.Scans["top-movers"]; !ok {
		t.Errorf("top-movers preset missing from defaults")
	}
}

func TestLoad_FileOverridesAndProfileSelection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `default_profile = "paper"

[profiles.paper]
host       = "127.0.0.1"
port       = 4002
client_id  = 16
account    = "DU111"
tls        = false

[profiles.live]
host       = "127.0.0.1"
port       = 4001
client_id  = 15
account    = "U222"
tls        = false

[daemon]
idle_timeout = "5m"
log_level    = "debug"

[scans.movers]
type     = "TOP_PERC_GAIN"
exchange = "STK.US.MAJOR"
limit    = 10
`
	if err := writeFile(path, body); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	res, err := cfg.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.ProfileName != "paper" {
		t.Errorf("ProfileName = %q, want paper", res.ProfileName)
	}
	if res.Profile.Account != "DU111" {
		t.Errorf("Account = %q, want DU111", res.Profile.Account)
	}
	if res.Daemon.IdleTimeout.Std() != 5*time.Minute {
		t.Errorf("idle = %v, want 5m", res.Daemon.IdleTimeout.Std())
	}
	if got, ok := res.Scans["movers"]; !ok || got.Limit != 10 {
		t.Errorf("scans[movers] = %+v, want Limit=10", got)
	}

	res2, err := cfg.Resolve("live")
	if err != nil {
		t.Fatalf("Resolve live: %v", err)
	}
	if res2.Profile.Port != 4001 || res2.Profile.Account != "U222" {
		t.Errorf("live profile mis-resolved: %+v", res2.Profile)
	}
}

func TestResolve_UnknownProfileErrors(t *testing.T) {
	cfg := &Config{Profiles: map[string]Profile{"paper": {Host: "x"}}}
	if _, err := cfg.Resolve("ghost"); err == nil {
		t.Fatal("expected error for unknown profile")
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
