package bot

import (
	"html"
	"net/url"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
)

// cell is one table cell: HTML for clients that render formatted bodies, plain text for the
// fallback body (notifications, text-only clients).
type cell struct{ html, text string }

func textCell(s string) cell { return cell{html.EscapeString(s), s} }

// mdCell renders a short, trusted Markdown fragment.
func mdCell(md string) cell {
	h, t := renderMD(md)
	return cell{h, t}
}

func linkCell(href, label string) cell {
	return cell{anchor(href, label), label}
}

func joinCells(sep string, cs ...cell) cell {
	var hs, ts []string
	for _, c := range cs {
		if c.text == "" && c.html == "" {
			continue
		}
		hs, ts = append(hs, c.html), append(ts, c.text)
	}
	return cell{strings.Join(hs, html.EscapeString(sep)), strings.Join(ts, sep)}
}

// anchor is an <a> to an http(s) URL; anything else is shown as text only.
func anchor(href, label string) string {
	u, err := url.Parse(href)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return html.EscapeString(label)
	}
	return `<a href="` + html.EscapeString(href) + `">` + html.EscapeString(label) + `</a>`
}

// renderMD converts Markdown to (HTML, plain text) the same way plain replies are rendered.
func renderMD(md string) (string, string) {
	c := format.RenderMarkdown(md, true, false)
	if c.Format == event.FormatHTML {
		return c.FormattedBody, c.Body
	}
	return strings.ReplaceAll(html.EscapeString(c.Body), "\n", "<br>"), c.Body
}

// rich builds a message mixing Markdown blocks and HTML tables.
type rich struct {
	h, t strings.Builder
}

func (m *rich) MD(md string) *rich {
	h, t := renderMD(strings.TrimSpace(md))
	if !startsWithBlock(h) {
		h = "<p>" + h + "</p>" // the renderer unwraps a lone paragraph
	}
	m.h.WriteString(h)
	m.sep()
	m.t.WriteString(t)
	return m
}

func startsWithBlock(h string) bool {
	for _, t := range []string{"<p>", "<ul>", "<ol>", "<h1>", "<h2>", "<h3>", "<h4>", "<blockquote>", "<pre>", "<table>", "<hr"} {
		if strings.HasPrefix(h, t) {
			return true
		}
	}
	return false
}

func (m *rich) sep() {
	if m.t.Len() > 0 && !strings.HasSuffix(m.t.String(), "\n\n") {
		m.t.WriteString("\n\n")
	}
}

// Table appends a table. head may be nil for a key/value table without a header row.
func (m *rich) Table(head []string, rows [][]cell) *rich {
	m.h.WriteString("<table>")
	if len(head) > 0 {
		m.h.WriteString("<thead><tr>")
		for _, c := range head {
			m.h.WriteString("<th>" + html.EscapeString(c) + "</th>")
		}
		m.h.WriteString("</tr></thead>")
	}
	m.h.WriteString("<tbody>")
	m.sep()
	for _, r := range rows {
		m.h.WriteString(rowHTML(r))
		m.t.WriteString(rowText(r, head == nil) + "\n")
	}
	m.h.WriteString("</tbody></table>")
	return m
}

func rowHTML(r []cell) string {
	var sb strings.Builder
	sb.WriteString("<tr>")
	for _, c := range r {
		sb.WriteString("<td>" + c.html + "</td>")
	}
	sb.WriteString("</tr>")
	return sb.String()
}

// rowText is "a · b · c", or "a: b" for key/value rows.
func rowText(r []cell, kv bool) string {
	var parts []string
	for _, c := range r {
		if c.text != "" {
			parts = append(parts, c.text)
		}
	}
	if kv && len(parts) == 2 {
		return parts[0] + ": " + parts[1]
	}
	return strings.Join(parts, " · ")
}

func (m *rich) Len() int { return m.h.Len() + m.t.Len() }

func (m *rich) Content() *event.MessageEventContent {
	return &event.MessageEventContent{
		MsgType:       event.MsgNotice,
		Body:          strings.TrimSpace(m.t.String()),
		Format:        event.FormatHTML,
		FormattedBody: m.h.String(),
		Mentions:      &event.Mentions{},
	}
}

// maxRichBytes keeps a message (HTML + text, before encryption overhead) well below the
// 64 KiB event size limit.
const maxRichBytes = 24 << 10

// TableMessages splits a long table over several messages: intro goes before the first
// part, outro after the last.
func TableMessages(intro string, head []string, rows [][]cell, outro string) []*rich {
	var out []*rich
	start := 0
	for start < len(rows) || len(out) == 0 {
		m := &rich{}
		if len(out) == 0 && intro != "" {
			m.MD(intro)
		}
		size := m.Len()
		end := start
		for end < len(rows) {
			n := len(rowHTML(rows[end])) + len(rowText(rows[end], false)) + 1
			if end > start && size+n > maxRichBytes {
				break
			}
			size += n
			end++
		}
		if end > start {
			m.Table(head, rows[start:end])
		}
		start = end
		if start >= len(rows) && outro != "" {
			m.MD(outro)
		}
		out = append(out, m)
	}
	return out
}
