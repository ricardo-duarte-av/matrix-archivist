// Package forge fetches release metadata and assets from GitHub, Gitea/Forgejo and GitLab.
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const userAgent = "matrix-archivist (+https://github.com/ricardo-duarte-av/matrix-archivist)"

type Asset struct {
	Name string
	URL  string
	Size int64 // 0 if unknown
	// Header to send when downloading (e.g. auth/accept). May be nil.
	Header http.Header
}

type Release struct {
	Tag        string
	Name       string
	Body       string
	Published  time.Time
	Prerelease bool
	Draft      bool
	Assets     []Asset
	Raw        json.RawMessage
}

// Provider lists releases of one repository.
type Provider interface {
	Type() string
	ListReleases(ctx context.Context, owner, repo string) ([]Release, error)
	// repoURL is the API URL describing the repository itself (used by Probe).
	repoURL(owner, repo string) string
	header() http.Header
}

// ErrNoRepo is returned when the forge says the repository does not exist (404).
var ErrNoRepo = errors.New("repository not found on forge")

// Client is a shared HTTP client wrapper.
type Client struct {
	HTTP *http.Client
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 0}} // per-request contexts provide timeouts
}

// New builds a provider of the given type for a host.
// apiURL may be empty to use the default for the type.
func (c *Client) New(typ, host, apiURL, token string) (Provider, error) {
	switch typ {
	case "github":
		if apiURL == "" {
			if host == "github.com" {
				apiURL = "https://api.github.com"
			} else {
				apiURL = "https://" + host + "/api/v3" // GitHub Enterprise
			}
		}
		return &GitHub{c: c, base: strings.TrimRight(apiURL, "/"), token: token}, nil
	case "gitea":
		if apiURL == "" {
			apiURL = "https://" + host + "/api/v1"
		}
		return &Gitea{c: c, base: strings.TrimRight(apiURL, "/"), token: token}, nil
	case "gitlab":
		if apiURL == "" {
			apiURL = "https://" + host + "/api/v4"
		}
		return &GitLab{c: c, base: strings.TrimRight(apiURL, "/"), token: token}, nil
	}
	return nil, fmt.Errorf("unknown forge type %q", typ)
}

// KnownHostType returns the forge type for well-known public hosts.
func KnownHostType(host string) string {
	switch host {
	case "github.com":
		return "github"
	case "gitlab.com", "gitlab.gnome.org", "gitlab.freedesktop.org", "salsa.debian.org", "invent.kde.org", "framagit.org":
		return "gitlab"
	case "codeberg.org", "gitea.com", "git.disroot.org", "next.forgejo.org":
		return "gitea"
	}
	return ""
}

// Detect probes an unknown host for a Gitea/Forgejo or GitLab API. Returns "" if none found.
func (c *Client) Detect(ctx context.Context, host string) string {
	probe := func(path string, check func(body []byte) bool) bool {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+path, nil)
		if err != nil {
			return false
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return err == nil && check(body)
	}
	if probe("/api/v1/version", func(b []byte) bool {
		var v struct {
			Version string `json:"version"`
		}
		return json.Unmarshal(b, &v) == nil && v.Version != ""
	}) {
		return "gitea"
	}
	if probe("/api/v4/projects?per_page=1&simple=true", func(b []byte) bool {
		var v []map[string]any
		return json.Unmarshal(b, &v) == nil
	}) {
		return "gitlab"
	}
	return ""
}

// getJSON GETs url and decodes into out. Returns the response headers.
func (c *Client) getJSON(ctx context.Context, url string, header http.Header, out any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", userAgent)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoRepo
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("GET %s: %s: %s", redact(url), resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", redact(url), err)
	}
	return resp.Header, nil
}

func redact(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[:i]
	}
	return u
}

// ErrTooLarge is returned when an asset exceeds the configured size cap.
var ErrTooLarge = errors.New("asset exceeds max_asset_size")

// Download saves an asset into dir. It skips files that already exist with the expected size
// (or at all, when the size is unknown). maxSize <= 0 means unlimited. Returns true if downloaded.
func (c *Client) Download(ctx context.Context, a Asset, dir string, maxSize int64) (bool, error) {
	name := SafeName(a.Name)
	if name == "" {
		return false, fmt.Errorf("invalid asset name %q", a.Name)
	}
	dst := filepath.Join(dir, name)
	if st, err := os.Stat(dst); err == nil && (a.Size == 0 || st.Size() == a.Size) {
		return false, nil
	}
	if maxSize > 0 && a.Size > maxSize {
		return false, fmt.Errorf("%s (%d bytes): %w", name, a.Size, ErrTooLarge)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return false, err
	}
	for k, v := range a.Header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("download %s: %s", name, resp.Status)
	}
	if maxSize > 0 && resp.ContentLength > maxSize {
		return false, fmt.Errorf("%s (%d bytes): %w", name, resp.ContentLength, ErrTooLarge)
	}

	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	var src io.Reader = resp.Body
	if maxSize > 0 {
		src = io.LimitReader(resp.Body, maxSize+1)
	}
	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && maxSize > 0 && n > maxSize {
		err = fmt.Errorf("%s: %w", name, ErrTooLarge)
	}
	if err == nil && a.Size > 0 && n != a.Size {
		err = fmt.Errorf("%s: size mismatch (got %d, want %d)", name, n, a.Size)
	}
	if err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, os.Rename(tmp, dst)
}

// SafeName turns a tag or asset name into a single safe path component.
func SafeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == ':' || r < 0x20 || r == 0x7f:
			return '_'
		}
		return r
	}, s)
	s = strings.TrimLeft(s, ".")
	if s == "" {
		return ""
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
