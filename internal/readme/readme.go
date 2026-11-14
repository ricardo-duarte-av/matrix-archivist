// Package readme finds a repository's README and extracts a title, a one-line summary and
// community links (Discord, Matrix, Telegram, …) from it.
package readme

import (
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxText caps how much README text is kept for searching.
const MaxText = 64 << 10

// Info is what Extract found.
type Info struct {
	Title   string
	Summary string
	Links   []string // display form without scheme, e.g. "discord.gg/abc"
	Text    string
}

var extRank = map[string]int{"md": 0, "markdown": 1, "": 2, "txt": 3, "rst": 4, "org": 5, "adoc": 6}

// Find returns the path of the README in dir ("" if none). Markdown is preferred.
func Find(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestRank := "", 99
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		base, ext, _ := strings.Cut(name, ".")
		if base != "readme" {
			continue
		}
		rank, known := extRank[ext]
		if !known {
			rank = 10
		}
		if rank < bestRank {
			best, bestRank = filepath.Join(dir, e.Name()), rank
		}
	}
	if best != "" {
		// Don't follow a symlink out of the checkout.
		if fi, err := os.Lstat(best); err != nil || !fi.Mode().IsRegular() {
			return ""
		}
	}
	return best
}

// Read finds and extracts the README in dir. ok is false if there is none.
func Read(dir string) (info Info, ok bool) {
	p := Find(dir)
	if p == "" {
		return Info{}, false
	}
	f, err := os.Open(p)
	if err != nil {
		return Info{}, false
	}
	defer f.Close()
	buf := make([]byte, MaxText)
	n, _ := f.Read(buf)
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		n += m
		if err != nil || m == 0 {
			break
		}
	}
	return Extract(string(buf[:n])), true
}

var (
	reTag       = regexp.MustCompile(`(?s)<[^>]*>`)
	reComment   = regexp.MustCompile(`(?s)<!--.*?-->`)
	reHeadingH  = regexp.MustCompile(`(?is)<h([1-3])[^>]*>(.*?)</h[1-3]>`)
	reImage     = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)|!\[[^\]]*\]\[[^\]]*\]`)
	reLink      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)|\[([^\]]*)\]\[[^\]]*\]`)
	reRefDef    = regexp.MustCompile(`^\s*\[[^\]]+\]:\s*\S+`)
	reEmphasis  = regexp.MustCompile("[*_`~]{1,3}")
	reAnchor    = regexp.MustCompile(`(?is)<a\b[^>]*>.*?</a>`)
	reImageAlt  = regexp.MustCompile(`(?i)!\[([^\]]+)\]\(|<img\b[^>]*\balt="([^"]+)"`)
	reBadgeAlt  = regexp.MustCompile(`(?i)badge|build|status|licen[cs]e|^logo$|shield|discord|ci\b|version|downloads|stars|screenshot|banner|icon`)
	reCommunity = regexp.MustCompile(`(?i)\b(?:https?://)?(?:www\.)?(` +
		`discord\.gg/[A-Za-z0-9-]+|discord(?:app)?\.com/invite/[A-Za-z0-9-]+|` +
		`matrix\.to/#/[#!@][^\s)"'<>\]]+|` +
		`t\.me/[A-Za-z0-9_+/-]+|` +
		`(?:old\.)?reddit\.com/r/[A-Za-z0-9_]+|` +
		`patreon\.com/[A-Za-z0-9_-]+|ko-fi\.com/[A-Za-z0-9_-]+|` +
		`[A-Za-z0-9-]+\.zulipchat\.com|` +
		`web\.libera\.chat/#[^\s)"'<>]+)`)
)

// Extract parses README text (Markdown, or anything resembling plain text).
func Extract(text string) Info {
	text = strings.ToValidUTF8(strings.ReplaceAll(text, "\r\n", "\n"), "")
	if len(text) > MaxText {
		text = truncateUTF8(text, MaxText)
	}
	info := Info{Text: text, Links: communityLinks(text)}

	body := reComment.ReplaceAllString(text, "")
	// HTML headings (centered READMEs) count as headings.
	body = reHeadingH.ReplaceAllStringFunc(body, func(m string) string {
		sub := reHeadingH.FindStringSubmatch(m)
		return "\n" + strings.Repeat("#", len(sub[1])) + " " + strings.TrimSpace(reTag.ReplaceAllString(sub[2], " ")) + "\n"
	})
	lines := strings.Split(body, "\n")

	var para []string
	inFence := false
	flush := func() bool {
		if len(para) == 0 {
			return false
		}
		joined := strings.Join(para, " ")
		para = nil
		s := clean(joined)
		// Rows of links ("Website • Discord") are navigation, not a description.
		if usefulSummary(s) && letters(clean(reAnchor.ReplaceAllString(reLink.ReplaceAllString(joined, ""), ""))) >= 12 {
			info.Summary = shorten(s, 200)
			return true
		}
		return false
	}
	for i, raw := range lines {
		l := strings.TrimSpace(raw)
		if strings.HasPrefix(l, "```") || strings.HasPrefix(l, "~~~") {
			inFence = !inFence
			if flush() {
				break
			}
			continue
		}
		if inFence {
			continue
		}
		// Setext heading: "Title\n=====".
		if i+1 < len(lines) && l != "" && len(para) == 0 && isUnderline(strings.TrimSpace(lines[i+1]), '=') {
			if info.Title == "" {
				info.Title = shorten(clean(l), 100)
			}
			continue
		}
		switch {
		case l == "", isUnderline(l, '='), isUnderline(l, '-'), isUnderline(l, '*'):
			if flush() {
				goto done
			}
		case strings.HasPrefix(l, "#"):
			if flush() {
				goto done
			}
			h := clean(strings.TrimLeft(l, "# "))
			if info.Title == "" && h != "" && !genericHeading[strings.ToLower(strings.Trim(h, " :"))] {
				info.Title = shorten(h, 100)
			}
		case strings.HasPrefix(l, "|"), strings.HasPrefix(l, ">"), strings.HasPrefix(l, "- "),
			strings.HasPrefix(l, "* "), strings.HasPrefix(l, "+ "), reRefDef.MatchString(l), strings.HasPrefix(l, ".."):
			// Tables, quotes (often admonitions), lists, link definitions, RST directives.
			if flush() {
				goto done
			}
		default:
			para = append(para, l)
		}
	}
	flush()
done:
	if info.Title == "" {
		head := body
		if len(head) > 2048 { // only the top of the README (logo/banner)
			head = truncateUTF8(head, 2048)
		}
		for _, m := range reImageAlt.FindAllStringSubmatch(head, 5) {
			alt := strings.TrimSpace(m[1] + m[2])
			if letters(alt) >= 3 && !reBadgeAlt.MatchString(alt) {
				info.Title = shorten(clean(alt), 100)
				break
			}
		}
	}
	return info
}

func communityLinks(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reCommunity.FindAllStringSubmatch(text, -1) {
		l := strings.TrimRight(m[1], ".,;:!?'\"")
		l = strings.TrimSuffix(l, "/")
		k := strings.ToLower(l)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, l)
		if len(out) == 4 {
			break
		}
	}
	return out
}

// clean turns a Markdown/HTML fragment into plain text.
func clean(s string) string {
	s = reImage.ReplaceAllString(s, "")
	s = reLink.ReplaceAllString(s, "$1$2")
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reEmphasis.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, `\`, "")
	return strings.Join(strings.Fields(s), " ") // Fields also splits on NBSP
}

// usefulSummary rejects leftovers such as badge rows, lone links and "Table of contents".
func usefulSummary(s string) bool {
	if utf8.RuneCountInString(s) < 15 || !strings.Contains(s, " ") {
		return false
	}
	return letters(s)*2 > len(s)/2
}

var genericHeading = map[string]bool{"overview": true, "introduction": true, "about": true, "description": true,
	"table of contents": true, "contents": true, "features": true, "readme": true, "summary": true, "info": true}

func letters(s string) int {
	n := 0
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127 {
			n++
		}
	}
	return n
}

func isUnderline(l string, c rune) bool {
	if len(l) < 3 {
		return false
	}
	for _, r := range l {
		if r != c && r != ' ' {
			return false
		}
	}
	return true
}

// shorten cuts s to about n runes at a word boundary.
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)[:n]
	cut := string(r)
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-—") + "…"
}

func truncateUTF8(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
