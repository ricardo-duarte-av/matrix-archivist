package gitops

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// RepoURL is a parsed, validated remote git URL.
type RepoURL struct {
	Raw   string // URL as given (trimmed); used for cloning
	Host  string // lowercase hostname, no port
	Path  string // owner/.../repo without .git, e.g. "torvalds/linux" or "group/sub/project"
	Owner string // everything before the last path segment
	Name  string // last path segment
	HTTPS bool   // scheme is http(s) — release APIs can be derived from it
}

// Key is a normalized identity used to detect duplicates regardless of scheme/.git/trailing slash.
func (r RepoURL) Key() string {
	return strings.ToLower(r.Host + "/" + r.Path)
}

// RelDir is the storage path relative to the storage root: host/owner/.../repo.
func (r RepoURL) RelDir() string {
	return path.Join(r.Host, r.Path)
}

var (
	scpLike     = regexp.MustCompile(`^([A-Za-z0-9._-]+)@([A-Za-z0-9.-]+):(.+)$`)
	safeSegment = regexp.MustCompile(`^[A-Za-z0-9._~+-]+$`)
	safeHost    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)

// ParseRepoURL validates a user-supplied git URL. Only network transports are allowed
// (https, http, ssh, git, and scp-style user@host:path); local paths and file:// are rejected.
func ParseRepoURL(raw string) (RepoURL, error) {
	raw = strings.TrimSpace(raw)
	// Matrix clients sometimes wrap URLs in <...>
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, "<"), ">")
	if raw == "" {
		return RepoURL{}, errors.New("empty URL")
	}
	if strings.HasPrefix(raw, "-") {
		return RepoURL{}, errors.New("URL must not start with '-'")
	}
	if strings.ContainsAny(raw, " \t\r\n\x00") {
		return RepoURL{}, errors.New("URL must not contain whitespace")
	}

	var host, p string
	isHTTPS := false
	if m := scpLike.FindStringSubmatch(raw); m != nil && !strings.Contains(raw, "://") {
		host, p = m[2], m[3]
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			return RepoURL{}, fmt.Errorf("invalid URL: %w", err)
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "http":
			isHTTPS = true
		case "ssh", "git", "git+ssh", "ssh+git":
		case "":
			return RepoURL{}, errors.New("URL needs a scheme (https://, ssh://, git://) or user@host:path form")
		default:
			return RepoURL{}, fmt.Errorf("scheme %q is not allowed", u.Scheme)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return RepoURL{}, errors.New("URL must not contain a query or fragment")
		}
		host, p = u.Hostname(), u.Path
	}

	host = strings.ToLower(host)
	if !safeHost.MatchString(host) {
		return RepoURL{}, fmt.Errorf("invalid host %q", host)
	}
	if host == "localhost" || !strings.Contains(host, ".") {
		return RepoURL{}, fmt.Errorf("host %q is not allowed", host)
	}

	p = strings.Trim(p, "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimSuffix(p, "/")
	segs := strings.Split(p, "/")
	if len(segs) < 1 || p == "" {
		return RepoURL{}, errors.New("URL has no repository path")
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." || !safeSegment.MatchString(s) {
			return RepoURL{}, fmt.Errorf("invalid path segment %q", s)
		}
	}
	// Strip "-/..." GitLab UI suffixes like /-/tree/main: refuse rather than guess.
	for _, s := range segs {
		if s == "-" {
			return RepoURL{}, errors.New("that looks like a web UI link; use the repository clone URL")
		}
	}

	return RepoURL{
		Raw:   raw,
		Host:  host,
		Path:  strings.Join(segs, "/"),
		Owner: strings.Join(segs[:len(segs)-1], "/"),
		Name:  segs[len(segs)-1],
		HTTPS: isHTTPS,
	}, nil
}
