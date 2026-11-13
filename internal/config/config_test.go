package config

import (
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cfg, err := Parse([]byte(`
matrix:
  homeserver: https://matrix.example.org
  user_id: "@bot:example.org"
  access_token: tok
  device_id: DEV
  room_id: "!r:example.org"
  pickle_key: k
command_prefix: "!cloner"
check_interval: 15m
summary: {time: "07:30", timezone: Europe/Lisbon}
releases: {max_asset_size: 500MB}
forges:
  - {host: git.example.org, type: forgejo}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CommandPrefix != "!cloner" || cfg.CheckInterval != 15*time.Minute || cfg.Summary.Hour != 7 || cfg.Summary.Minute != 30 {
		t.Errorf("unexpected: %+v", cfg)
	}
	if cfg.Releases.MaxAssetSize != 500<<20 {
		t.Errorf("size: %d", cfg.Releases.MaxAssetSize)
	}
	if f, ok := cfg.ForgeFor("git.example.org"); !ok || f.Type != "gitea" {
		t.Errorf("forge: %+v", f)
	}
}

func TestParseErrors(t *testing.T) {
	_, err := Parse([]byte(`matrix: {access_token: x}
summary: {time: "25:00"}
check_interval: 5s`))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"homeserver", "device_id", "room_id", "pickle_key", "summary.time", "check_interval"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
