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

func TestParseLightAndSources(t *testing.T) {
	base := `
matrix: {homeserver: "https://m.example.org", user_id: "@b:example.org", password: x, room_id: "!r:example.org", pickle_key: k}
`
	cfg, err := Parse([]byte(base + `
sources:
  - {name: recomps, url: "http://recompbot:8080/api/recomps", interval: 30m, mode: Full}
`))
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.Light
	if l.CheckInterval != 12*time.Hour || l.MaxClonesPerCycle != 50 || !l.Releases.DownloadAssets || l.Releases.KeepLast != 1 || l.Releases.MaxAssetSize != 200<<20 {
		t.Errorf("light defaults: %+v", l)
	}
	if s := cfg.Sources[0]; s.Interval != 30*time.Minute || s.Mode != ModeFull || s.URLField != "url" {
		t.Errorf("source: %+v", s)
	}
	_, err = Parse([]byte(base + `
light: {check_interval: 1s, releases: {keep_last: -1}}
sources:
  - {name: a, url: "file:///etc/passwd"}
  - {name: a, url: "https://x.example.org/list", mode: heavy}
  - {url: "https://x.example.org/list"}
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"light.check_interval", "light.releases.keep_last", "sources[0].url", "used twice", "sources[1].mode", "sources[2].name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestParseMirror(t *testing.T) {
	const m = `matrix: {homeserver: "https://m.example.org", user_id: "@b:example.org", password: x, room_id: "!r:example.org", pickle_key: k}
`
	cfg, err := Parse([]byte(m + `mirror: {enabled: true, url: "http://forgejo:3000/", token: t}`))
	if err != nil {
		t.Fatal(err)
	}
	mi := cfg.Mirror
	if mi.URL != "http://forgejo:3000" || mi.PublicURL != mi.URL || mi.Concurrency != 2 || mi.MaxNewPerCycle != 50 ||
		!mi.Releases || !mi.ReleaseFiles || !mi.Snapshots {
		t.Errorf("defaults: %+v", mi)
	}
	if got := mi.RepoURL("o/r"); got != "http://forgejo:3000/o/r" {
		t.Errorf("repo url: %s", got)
	}
	_, err = Parse([]byte(m + `mirror: {enabled: true, url: "forgejo:3000"}`))
	if err == nil || !strings.Contains(err.Error(), "mirror.url") || !strings.Contains(err.Error(), "mirror.token") {
		t.Errorf("expected url and token errors, got %v", err)
	}
	if _, err := Parse([]byte(m + `mirror: {enabled: false, url: "nonsense"}`)); err != nil {
		t.Errorf("a disabled mirror isn't validated: %v", err)
	}
}
