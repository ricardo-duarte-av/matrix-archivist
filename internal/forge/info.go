package forge

import (
	"context"
	"strings"
)

// RepoInfo is the descriptive metadata a forge keeps about a repository.
type RepoInfo struct {
	Description string
	Homepage    string
	Topics      []string
}

// Info fetches a repository's description, homepage and topics from the forge API.
// GitHub, Gitea/Forgejo and GitLab all answer on the same endpoint Probe uses.
func (c *Client) Info(ctx context.Context, p Provider, owner, name string) (RepoInfo, error) {
	var raw struct {
		Description string   `json:"description"`
		Homepage    string   `json:"homepage"` // GitHub
		Website     string   `json:"website"`  // Gitea/Forgejo
		Topics      []string `json:"topics"`   // GitHub, Gitea, GitLab ≥ 14
		TagList     []string `json:"tag_list"` // older GitLab
	}
	if _, err := c.getJSON(ctx, p.repoURL(owner, name), p.header(), &raw); err != nil {
		return RepoInfo{}, err
	}
	info := RepoInfo{
		Description: strings.TrimSpace(raw.Description),
		Homepage:    strings.TrimSpace(raw.Homepage),
		Topics:      raw.Topics,
	}
	if info.Homepage == "" {
		info.Homepage = strings.TrimSpace(raw.Website)
	}
	switch h := info.Homepage; {
	case strings.HasPrefix(h, "https://"), strings.HasPrefix(h, "http://"):
	case strings.Contains(h, ".") && !strings.ContainsAny(h, " <>\"\\:"):
		info.Homepage = "https://" + h // GitHub allows "example.org"
	default:
		info.Homepage = ""
	}
	if len(info.Topics) == 0 {
		info.Topics = raw.TagList
	}
	return info, nil
}
