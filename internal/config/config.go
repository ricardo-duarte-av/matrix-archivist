// Package config loads and validates the bot's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Matrix struct {
	Homeserver  string `yaml:"homeserver"`
	UserID      string `yaml:"user_id"`
	AccessToken string `yaml:"access_token"`
	DeviceID    string `yaml:"device_id"`
	Password    string `yaml:"password"`
	RoomID      string `yaml:"room_id"`
	// RoomVia lists servers already in the room, used to join it (optional; needed only if
	// automatic detection from the invite fails).
	RoomVia     []string `yaml:"room_via"`
	PickleKey   string   `yaml:"pickle_key"`
	DisplayName string   `yaml:"display_name"`
}

type Summary struct {
	Enabled     bool   `yaml:"enabled"`
	Time        string `yaml:"time"`
	Timezone    string `yaml:"timezone"`
	SkipIfEmpty bool   `yaml:"skip_if_empty"`

	Hour, Minute int            `yaml:"-"`
	Location     *time.Location `yaml:"-"`
}

type Releases struct {
	Enabled            bool   `yaml:"enabled"`
	DownloadAssets     bool   `yaml:"download_assets"`
	MaxAssetSizeRaw    string `yaml:"max_asset_size"`
	IncludePrereleases bool   `yaml:"include_prereleases"`

	MaxAssetSize int64 `yaml:"-"`
}

type Forge struct {
	Host  string `yaml:"host"`
	Type  string `yaml:"type"` // github | gitea | gitlab
	Token string `yaml:"token"`
	// APIURL overrides the API base URL (e.g. GitHub Enterprise). Optional.
	APIURL string `yaml:"api_url"`
}

type HTTP struct {
	Enabled bool   `yaml:"enabled"`
	Bind    string `yaml:"bind"`
	Port    int    `yaml:"port"`
	// PublicURL is the externally reachable base URL (e.g. https://archive.example.org), used for
	// links in bot messages. Optional.
	PublicURL string `yaml:"public_url"`
	// Optional HTTP basic auth. Both must be set to enable it.
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Maximum number of zip downloads generated at the same time.
	MaxConcurrentZips int `yaml:"max_concurrent_zips"`
	// Metrics serves Prometheus metrics at /metrics (behind basic auth when it is enabled).
	Metrics bool `yaml:"metrics"`
	// Widget adds the file browser to the room as a widget (needs public_url).
	Widget Widget `yaml:"widget"`
}

type Widget struct {
	Enabled bool   `yaml:"enabled"`
	Name    string `yaml:"name"`
	// Pin shows the widget at the top of the room for everyone (Element's room layout).
	Pin bool `yaml:"pin"`
}

// BrowseURL returns the public file-browser URL for a storage-relative directory, or "" when
// the browser is disabled or public_url isn't set.
func (h HTTP) BrowseURL(relDir string) string {
	if !h.Enabled || h.PublicURL == "" {
		return ""
	}
	if relDir == "" {
		return h.PublicURL + "/browse/"
	}
	parts := strings.Split(relDir, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return h.PublicURL + "/browse/" + strings.Join(parts, "/") + "/"
}

// Addr is the listen address, e.g. "0.0.0.0:8080".
func (h HTTP) Addr() string { return fmt.Sprintf("%s:%d", h.Bind, h.Port) }

type Config struct {
	Matrix           Matrix   `yaml:"matrix"`
	CommandPrefix    string   `yaml:"command_prefix"`
	MinPowerLevel    int      `yaml:"min_power_level"`
	StorageDir       string   `yaml:"storage_dir"`
	Database         string   `yaml:"database"`
	CryptoDatabase   string   `yaml:"crypto_database"`
	CheckIntervalRaw string   `yaml:"check_interval"`
	Concurrency      int      `yaml:"concurrency"`
	GitTimeoutRaw    string   `yaml:"git_timeout"`
	FailureAlert     int      `yaml:"failure_alert_threshold"`
	PruneDeleted     bool     `yaml:"prune_deleted"`
	DiskWarnFreeRaw  string   `yaml:"disk_warn_free"`
	LogLevel         string   `yaml:"log_level"`
	Summary          Summary  `yaml:"summary"`
	Releases         Releases `yaml:"releases"`
	Forges           []Forge  `yaml:"forges"`
	HTTP             HTTP     `yaml:"http"`

	CheckInterval time.Duration `yaml:"-"`
	GitTimeout    time.Duration `yaml:"-"`
	DiskWarnFree  int64         `yaml:"-"`
}

func defaults() *Config {
	return &Config{
		CommandPrefix:    "!archivist",
		MinPowerLevel:    50,
		StorageDir:       "/data/repos",
		Database:         "/data/archivist.db",
		CryptoDatabase:   "/data/crypto.db",
		CheckIntervalRaw: "1h",
		Concurrency:      3,
		GitTimeoutRaw:    "30m",
		FailureAlert:     5,
		DiskWarnFreeRaw:  "5GB",
		LogLevel:         "info",
		Summary: Summary{
			Enabled:  true,
			Time:     "09:00",
			Timezone: "UTC",
		},
		HTTP: HTTP{
			Enabled:           true,
			Bind:              "0.0.0.0",
			Port:              8080,
			MaxConcurrentZips: 2,
			Metrics:           true,
			Widget:            Widget{Enabled: true, Name: "Archive"},
		},
		Releases: Releases{
			Enabled:            true,
			DownloadAssets:     true,
			MaxAssetSizeRaw:    "2GB",
			IncludePrereleases: true,
		},
	}
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses and validates config YAML.
func Parse(data []byte) (*Config, error) {
	cfg := defaults()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	m := &c.Matrix
	if m.Homeserver == "" {
		add("matrix.homeserver is required")
	}
	if !strings.HasPrefix(m.UserID, "@") {
		add("matrix.user_id must be a full Matrix ID like @bot:example.org")
	}
	if m.AccessToken == "" && m.Password == "" {
		add("either matrix.access_token or matrix.password is required")
	}
	if m.AccessToken != "" && m.DeviceID == "" {
		add("matrix.device_id is required when using matrix.access_token (E2EE needs a stable device)")
	}
	m.RoomID = strings.TrimSpace(m.RoomID)
	if !strings.HasPrefix(m.RoomID, "!") {
		add("matrix.room_id must be a room ID like !abc:example.org (quote it in YAML: \"!abc:example.org\")")
	} else if !validRoomID(m.RoomID) {
		add("matrix.room_id %q contains invalid or invisible characters; copy it again", m.RoomID)
	}
	if m.PickleKey == "" {
		add("matrix.pickle_key is required")
	}
	if m.DisplayName == "" {
		m.DisplayName = "Archivist"
	}

	c.CommandPrefix = strings.TrimSpace(c.CommandPrefix)
	if c.CommandPrefix == "" || strings.ContainsAny(c.CommandPrefix, " \t\n") {
		add("command_prefix must be a single non-empty word")
	}
	if c.StorageDir == "" {
		add("storage_dir is required")
	}
	if c.Database == "" {
		add("database is required")
	}
	if c.CryptoDatabase == "" {
		add("crypto_database is required")
	}
	if c.Concurrency < 1 {
		c.Concurrency = 1
	}

	var err error
	if c.CheckInterval, err = time.ParseDuration(c.CheckIntervalRaw); err != nil || c.CheckInterval < time.Minute {
		add("check_interval must be a duration >= 1m (e.g. 30m, 1h)")
	}
	if c.GitTimeout, err = time.ParseDuration(c.GitTimeoutRaw); err != nil || c.GitTimeout <= 0 {
		add("git_timeout must be a positive duration (e.g. 30m)")
	}

	if c.Summary.Enabled {
		h, mi, ok := parseClock(c.Summary.Time)
		if !ok {
			add("summary.time must be HH:MM (24h)")
		}
		c.Summary.Hour, c.Summary.Minute = h, mi
	}
	if c.Summary.Timezone == "" {
		c.Summary.Timezone = "UTC"
	}
	if c.Summary.Location, err = time.LoadLocation(c.Summary.Timezone); err != nil {
		add("summary.timezone: %v", err)
	}

	if c.DiskWarnFree, err = ParseSize(c.DiskWarnFreeRaw); err != nil {
		add("disk_warn_free: %v", err)
	}
	if c.HTTP.Widget.Name == "" {
		c.HTTP.Widget.Name = "Archive"
	}

	if c.Releases.MaxAssetSize, err = ParseSize(c.Releases.MaxAssetSizeRaw); err != nil {
		add("releases.max_asset_size: %v", err)
	}

	if c.HTTP.Enabled {
		if c.HTTP.Port < 1 || c.HTTP.Port > 65535 {
			add("http.port must be between 1 and 65535")
		}
		if (c.HTTP.Username == "") != (c.HTTP.Password == "") {
			add("http.username and http.password must both be set (or both empty)")
		}
		if c.HTTP.MaxConcurrentZips < 1 {
			c.HTTP.MaxConcurrentZips = 1
		}
		c.HTTP.PublicURL = strings.TrimRight(c.HTTP.PublicURL, "/")
		if c.HTTP.PublicURL != "" && !strings.HasPrefix(c.HTTP.PublicURL, "http://") && !strings.HasPrefix(c.HTTP.PublicURL, "https://") {
			add("http.public_url must start with http:// or https://")
		}
	}

	for i := range c.Forges {
		f := &c.Forges[i]
		f.Host = strings.ToLower(strings.TrimSpace(f.Host))
		f.Type = strings.ToLower(strings.TrimSpace(f.Type))
		if f.Type == "forgejo" || f.Type == "codeberg" {
			f.Type = "gitea"
		}
		if f.Host == "" {
			add("forges[%d].host is required", i)
		}
		switch f.Type {
		case "github", "gitea", "gitlab":
		default:
			add("forges[%d].type must be github, gitea or gitlab", i)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// ForgeFor returns the configured forge entry for a host, if any.
func (c *Config) ForgeFor(host string) (Forge, bool) {
	host = strings.ToLower(host)
	for _, f := range c.Forges {
		if f.Host == host {
			return f, true
		}
	}
	return Forge{}, false
}

// validRoomID checks a room ID only contains printable ASCII (no spaces or invisible characters).
func validRoomID(s string) bool {
	if len(s) < 2 {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r >= 0x7f {
			return false
		}
	}
	return true
}

func parseClock(s string) (int, int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// ParseSize parses sizes like "500MB", "2GB", "1024" (bytes). 0 or empty means unlimited.
func ParseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			s = strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}
