package mirror

import (
	"regexp"
	"strings"
)

// reservedOwners can't be used as user or organization names on Forgejo/Gitea.
var reservedOwners = map[string]bool{
	"admin": true, "api": true, "assets": true, "attachments": true, "avatar": true, "avatars": true,
	"captcha": true, "commits": true, "debug": true, "devtest": true, "error": true, "explore": true,
	"favicon.ico": true, "ghost": true, "issues": true, "login": true, "manifest.json": true,
	"metrics": true, "milestones": true, "new": true, "notifications": true, "org": true,
	"pulls": true, "raw": true, "repo": true, "repo-avatars": true, "robots.txt": true,
	"search": true, "serviceworker.js": true, "ssh_info": true, "swagger.v1.json": true,
	"user": true, "v2": true, "gitea-actions": true, "forgejo-actions": true, "actions": true,
}

var (
	badNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	repeatedSep  = regexp.MustCompile(`[._-]{2,}`)
	badTopic     = regexp.MustCompile(`[^a-z0-9.-]+`)
)

// Path maps an upstream repository (host and owner/…/name path) to its owner and name on the
// forge: github.com/zeldaret/oot → zeldaret/oot. Nested groups (gitlab.com/a/b/c) keep the
// top group as the owner and join the rest: a/b-c.
func Path(host, path string) (owner, name string) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) < 2 {
		host, _, _ = strings.Cut(host, ".")
		segs = append([]string{host}, segs...)
	}
	return OwnerName(segs[0]), RepoName(strings.Join(segs[1:], "-"))
}

// OwnerName makes a valid Forgejo user/organization name.
func OwnerName(s string) string {
	s = badNameChars.ReplaceAllString(s, "-")
	s = repeatedSep.ReplaceAllStringFunc(s, func(m string) string { return m[:1] })
	s = strings.Trim(s, "._-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "._-")
	}
	if s == "" {
		s = "unknown"
	}
	if reservedOwners[strings.ToLower(s)] {
		s += "-mirror"
	}
	return s
}

// RepoName makes a valid Forgejo repository name.
func RepoName(s string) string {
	s = badNameChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, ".")
	if len(s) > 100 {
		s = s[:100]
	}
	lower := strings.ToLower(s)
	for _, suf := range []string{".git", ".wiki", ".rss", ".atom"} {
		if strings.HasSuffix(lower, suf) {
			s = s[:len(s)-len(suf)] + "-" + suf[1:]
			break
		}
	}
	if s == "" || s == "-" {
		s = "repo"
	}
	return s
}

// WithSuffix makes a repository name unique by appending a suffix (e.g. the upstream host),
// staying within the length limit.
func WithSuffix(name, suffix string) string {
	suffix = "-" + RepoName(suffix)
	if len(name)+len(suffix) > 100 {
		name = name[:100-len(suffix)]
	}
	return name + suffix
}

// Topics turns tags into valid, de-duplicated forge topics (lowercase letters, digits, '-' and
// '.', starting with a letter or digit, at most 35 characters, at most 25 topics).
func Topics(tags []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range tags {
		t = badTopic.ReplaceAllString(strings.ToLower(strings.TrimSpace(t)), "-")
		t = strings.TrimLeft(t, ".-")
		if len(t) > 35 {
			t = strings.TrimRight(t[:35], ".-")
		}
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == 25 {
			break
		}
	}
	return out
}

// CleanText makes text safe for the forge's database: invalid UTF-8 is dropped, and so are
// NUL bytes and other control characters except tab and line breaks (PostgreSQL refuses NUL
// in text, and upstream release notes sometimes contain one).
func CleanText(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return r
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
}
