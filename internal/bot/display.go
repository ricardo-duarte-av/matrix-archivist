package bot

import (
	"fmt"
	"html"
	"net/url"
	"sort"
	"strings"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/store"
)

// listHead is the header of repository tables (list, search).
var listHead = []string{"#", "Repository", "About", "Links", "Status"}

// displayURL is the repo URL without scheme or trailing slash: "github.com/o/r".
func displayURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	return strings.TrimSuffix(u, "/")
}

func idCell(r *store.Repo) cell {
	s := fmt.Sprintf("#%d", r.ID)
	return cell{"<strong>" + s + "</strong>", s}
}

// repoCell links the repository and shows its tags.
func repoCell(r *store.Repo) cell {
	c := linkCell(r.URL, displayURL(r.URL))
	for _, t := range r.Tags {
		c.html += " <code>" + html.EscapeString(t) + "</code>"
		c.text += " `" + t + "`"
	}
	return c
}

// About describes a repository in one line (see store.Repo.About).
func About(r *store.Repo) string { return r.About() }

func aboutCell(r *store.Repo, max int) cell {
	return textCell(oneLineMD(About(r), max))
}

// ExtraLinks are the homepage (unless it's the repository page itself) and community
// links found in the README, as (href, label) pairs.
func ExtraLinks(r *store.Repo) [][2]string {
	var out [][2]string
	seen := map[string]bool{}
	add := func(href string) {
		label := strings.TrimSuffix(displayURL(href), "/")
		label = strings.TrimPrefix(label, "www.")
		k := strings.ToLower(label)
		if label == "" || seen[k] || strings.EqualFold(label, displayURL(r.URL)) {
			return
		}
		seen[k] = true
		out = append(out, [2]string{href, label})
	}
	if h := r.Homepage; h != "" {
		if u, err := url.Parse(h); err == nil && u.Host != "" {
			add(h)
		}
	}
	for _, l := range r.Links {
		add("https://" + l)
	}
	return out
}

func linksCell(r *store.Repo) cell {
	var cs []cell
	for _, l := range ExtraLinks(r) {
		cs = append(cs, linkCell(l[0], l[1]))
	}
	return joinCells(" · ", cs...)
}

func repoRow(r *store.Repo) []cell {
	return []cell{idCell(r), repoCell(r), aboutCell(r, 140), linksCell(r), textCell(repoState(r))}
}

// RankRepos orders search results: matches in the repository name and description first,
// then notes/tags/topics, then README-only matches; ties keep id order.
func RankRepos(repos []*store.Repo, terms []string) []*store.Repo {
	score := make(map[int64]int, len(repos))
	for _, r := range repos {
		name := strings.ToLower(displayURL(r.URL))
		about := strings.ToLower(r.Description + "\n" + r.ReadmeTitle + "\n" + r.ReadmeSummary)
		meta := strings.ToLower(r.Notes + "\n" + strings.Join(r.Tags, " ") + "\n" + strings.Join(r.Topics, " ") + "\n" + r.Homepage + "\n" + strings.Join(r.Links, " "))
		s := 0
		for _, t := range terms {
			t = strings.ToLower(t)
			switch {
			case strings.HasPrefix(t, "#") && !isDigits(t[1:]):
				s += 3 // exact tag filter: every result matches
			case strings.Contains(name, t):
				s += 10
			case strings.Contains(about, t):
				s += 6
			case strings.Contains(meta, t):
				s += 4
			default:
				s++ // README body
			}
		}
		score[r.ID] = s
	}
	out := append([]*store.Repo(nil), repos...)
	sort.SliceStable(out, func(i, j int) bool { return score[out[i].ID] > score[out[j].ID] })
	return out
}
