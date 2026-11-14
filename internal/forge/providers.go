package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

const maxPages = 50

// ---------- GitHub ----------

type GitHub struct {
	c     *Client
	base  string
	token string
}

func (g *GitHub) Type() string { return "github" }

func (g *GitHub) repoURL(owner, repo string) string {
	return fmt.Sprintf("%s/repos/%s/%s", g.base, url.PathEscape(owner), url.PathEscape(repo))
}

type ghRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
	CreatedAt   string `json:"created_at"`
	Assets      []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		URL                string `json:"url"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (g *GitHub) header() http.Header {
	h := http.Header{}
	h.Set("Accept", "application/vnd.github+json")
	h.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		h.Set("Authorization", "Bearer "+g.token)
	}
	return h
}

func (g *GitHub) ListReleases(ctx context.Context, owner, repo string) ([]Release, error) {
	const perPage = 100
	var out []Release
	for page := 1; page <= maxPages; page++ {
		var raw []json.RawMessage
		u := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=%d&page=%d", g.base, url.PathEscape(owner), url.PathEscape(repo), perPage, page)
		if _, err := g.c.getJSON(ctx, u, g.header(), &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			var gr ghRelease
			if err := json.Unmarshal(r, &gr); err != nil {
				return nil, err
			}
			rel := Release{
				Tag: gr.TagName, Name: gr.Name, Body: gr.Body, Draft: gr.Draft, Prerelease: gr.Prerelease,
				Published: parseTime(gr.PublishedAt), Raw: r,
			}
			if rel.Published.IsZero() {
				rel.Published = parseTime(gr.CreatedAt)
			}
			for _, a := range gr.Assets {
				asset := Asset{Name: a.Name, Size: a.Size, URL: a.BrowserDownloadURL}
				if g.token != "" && a.URL != "" {
					// API URL works for private repos too; it redirects to the storage backend.
					h := http.Header{}
					h.Set("Accept", "application/octet-stream")
					h.Set("Authorization", "Bearer "+g.token)
					asset.URL, asset.Header = a.URL, h
				}
				rel.Assets = append(rel.Assets, asset)
			}
			out = append(out, rel)
		}
		if len(raw) < perPage {
			break
		}
	}
	return out, nil
}

// ---------- Gitea / Forgejo ----------

type Gitea struct {
	c     *Client
	base  string
	token string
}

func (g *Gitea) Type() string { return "gitea" }

func (g *Gitea) repoURL(owner, repo string) string {
	return fmt.Sprintf("%s/repos/%s/%s", g.base, url.PathEscape(owner), url.PathEscape(repo))
}

type giteaRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
	CreatedAt   string `json:"created_at"`
	Assets      []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (g *Gitea) header() http.Header {
	h := http.Header{}
	if g.token != "" {
		h.Set("Authorization", "token "+g.token)
	}
	return h
}

func (g *Gitea) ListReleases(ctx context.Context, owner, repo string) ([]Release, error) {
	const perPage = 50
	var out []Release
	for page := 1; page <= maxPages; page++ {
		var raw []json.RawMessage
		u := fmt.Sprintf("%s/repos/%s/%s/releases?limit=%d&page=%d", g.base, url.PathEscape(owner), url.PathEscape(repo), perPage, page)
		if _, err := g.c.getJSON(ctx, u, g.header(), &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			var gr giteaRelease
			if err := json.Unmarshal(r, &gr); err != nil {
				return nil, err
			}
			rel := Release{
				Tag: gr.TagName, Name: gr.Name, Body: gr.Body, Draft: gr.Draft, Prerelease: gr.Prerelease,
				Published: parseTime(gr.PublishedAt), Raw: r,
			}
			if rel.Published.IsZero() {
				rel.Published = parseTime(gr.CreatedAt)
			}
			for _, a := range gr.Assets {
				rel.Assets = append(rel.Assets, Asset{Name: a.Name, Size: a.Size, URL: a.BrowserDownloadURL, Header: g.header()})
			}
			out = append(out, rel)
		}
		// Servers may cap limit below what we asked for; stop only on an empty page.
		if len(raw) == 0 {
			break
		}
	}
	return out, nil
}

// ---------- GitLab ----------

type GitLab struct {
	c     *Client
	base  string
	token string
}

func (g *GitLab) Type() string { return "gitlab" }

func (g *GitLab) repoURL(owner, repo string) string {
	return fmt.Sprintf("%s/projects/%s", g.base, url.PathEscape(owner+"/"+repo))
}

type glRelease struct {
	TagName         string `json:"tag_name"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	ReleasedAt      string `json:"released_at"`
	CreatedAt       string `json:"created_at"`
	UpcomingRelease bool   `json:"upcoming_release"`
	Assets          struct {
		Links []struct {
			Name           string `json:"name"`
			URL            string `json:"url"`
			DirectAssetURL string `json:"direct_asset_url"`
		} `json:"links"`
	} `json:"assets"`
}

func (g *GitLab) header() http.Header {
	h := http.Header{}
	if g.token != "" {
		h.Set("PRIVATE-TOKEN", g.token)
	}
	return h
}

func (g *GitLab) ListReleases(ctx context.Context, owner, repo string) ([]Release, error) {
	const perPage = 100
	project := url.PathEscape(owner + "/" + repo)
	var out []Release
	for page := 1; page <= maxPages; page++ {
		var raw []json.RawMessage
		u := fmt.Sprintf("%s/projects/%s/releases?per_page=%d&page=%d", g.base, project, perPage, page)
		if _, err := g.c.getJSON(ctx, u, g.header(), &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			var gr glRelease
			if err := json.Unmarshal(r, &gr); err != nil {
				return nil, err
			}
			rel := Release{
				Tag: gr.TagName, Name: gr.Name, Body: gr.Description,
				Prerelease: gr.UpcomingRelease, Published: parseTime(gr.ReleasedAt), Raw: r,
			}
			if rel.Published.IsZero() {
				rel.Published = parseTime(gr.CreatedAt)
			}
			for _, l := range gr.Assets.Links {
				link := l.DirectAssetURL
				if link == "" {
					link = l.URL
				}
				// Only send the token to the GitLab host itself, never to external links.
				var h http.Header
				if sameHost(link, g.base) {
					h = g.header()
				}
				rel.Assets = append(rel.Assets, Asset{Name: l.Name, URL: link, Header: h})
			}
			out = append(out, rel)
		}
		if len(raw) < perPage {
			break
		}
	}
	return out, nil
}

func sameHost(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Host == ub.Host
}
