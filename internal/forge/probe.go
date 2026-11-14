package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Upstream availability as determined by Probe.
const (
	Available = "available"
	Gone      = "gone"    // 404 / 410: deleted, renamed away or made private
	Legal     = "legal"   // 451: unavailable for legal reasons (e.g. DMCA takedown)
	Blocked   = "blocked" // the forge blocked access (ToS violation etc.)
	Unknown   = "unknown" // couldn't tell (network error, rate limit, server error…)
)

// RepoStatus is the outcome of probing a repository upstream.
type RepoStatus struct {
	State  string
	Code   int    // HTTP status, 0 if the request failed
	Detail string // human-readable explanation
}

// Probe checks whether a repository still exists upstream. It asks the forge API when p is
// non-nil, and falls back to the repository's web page (https://host/path).
func (c *Client) Probe(ctx context.Context, p Provider, owner, name, webURL string) RepoStatus {
	if p != nil && owner != "" {
		if st := c.probe(ctx, p.repoURL(owner, name), p.header(), true); st.State != Unknown {
			return st
		}
	}
	if webURL == "" {
		return RepoStatus{State: Unknown}
	}
	return c.probe(ctx, webURL, nil, false)
}

func (c *Client) probe(ctx context.Context, url string, header http.Header, api bool) RepoStatus {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return RepoStatus{State: Unknown, Detail: err.Error()}
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", userAgent)
	if api {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return RepoStatus{State: Unknown, Detail: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	return classify(resp.StatusCode, body, api)
}

// classify maps an HTTP response about a repository to an availability state.
func classify(code int, body []byte, api bool) RepoStatus {
	st := RepoStatus{Code: code}
	reason, link := blockInfo(body, api)
	switch {
	case code >= 200 && code < 300:
		st.State = Available
	case code == http.StatusNotFound || code == http.StatusGone:
		st.State = Gone
		st.Detail = fmt.Sprintf("%d %s: deleted, renamed or made private", code, http.StatusText(code))
	case code == http.StatusUnavailableForLegalReasons:
		st.State = Legal
		st.Detail = "451 Unavailable For Legal Reasons"
	case code == http.StatusForbidden && reason != "":
		// GitHub answers 403 "Repository access blocked" for ToS takedowns. A plain 403 is
		// usually a rate limit, so it stays Unknown.
		st.State = Blocked
		st.Detail = "403 Repository access blocked"
	default:
		st.State = Unknown
		st.Detail = fmt.Sprintf("%d %s", code, http.StatusText(code))
		return st
	}
	if reason != "" {
		if strings.EqualFold(reason, "dmca") {
			reason = "DMCA takedown"
			if st.State == Blocked {
				st.State = Legal
			}
		}
		st.Detail += " (" + reason
		if link != "" {
			st.Detail += ": " + link
		}
		st.Detail += ")"
	}
	return st
}

// blockInfo extracts GitHub's block reason from an API error body:
// {"message":"Repository access blocked","block":{"reason":"dmca","html_url":"https://…"}}
func blockInfo(body []byte, api bool) (reason, link string) {
	if !api {
		return "", ""
	}
	var v struct {
		Message string `json:"message"`
		Block   struct {
			Reason  string `json:"reason"`
			HTMLURL string `json:"html_url"`
		} `json:"block"`
	}
	if json.Unmarshal(body, &v) != nil {
		return "", ""
	}
	if v.Block.Reason != "" {
		return v.Block.Reason, v.Block.HTMLURL
	}
	if strings.Contains(strings.ToLower(v.Message), "access blocked") {
		return v.Message, ""
	}
	return "", ""
}

// ClassifyGitError recognises takedown messages in git's own error output, for hosts whose
// API couldn't be asked. Returns Unknown when nothing matches.
func ClassifyGitError(msg string) RepoStatus {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "dmca"):
		return RepoStatus{State: Legal, Detail: "git: repository unavailable due to DMCA takedown"}
	case strings.Contains(l, "unavailable for legal reasons") || strings.Contains(l, "error: 451"):
		return RepoStatus{State: Legal, Detail: "git: 451 Unavailable For Legal Reasons"}
	case strings.Contains(l, "repository access blocked") || strings.Contains(l, "access to this repository has been disabled"):
		return RepoStatus{State: Blocked, Detail: "git: repository access blocked"}
	}
	return RepoStatus{State: Unknown}
}
