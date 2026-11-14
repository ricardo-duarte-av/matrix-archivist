package gitops

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Progress is one parsed git progress line, e.g.
// "Receiving objects:  45% (1234/2700), 12.00 MiB | 3.00 MiB/s".
type Progress struct {
	Phase       string
	Done, Total int64
	Percent     int
	Transferred string // "12.00 MiB" (receiving only)
	Speed       string // "3.00 MiB/s"
}

var reProgress = regexp.MustCompile(`^(?:remote: )?([A-Za-z][A-Za-z ]+):\s+(\d+)% \((\d+)/(\d+)\)(?:,\s*([\d.]+ [KMGT]?i?B)(?:\s*\|\s*([\d.]+ [KMGT]?i?B/s))?)?`)

// reChatter matches informational lines that --progress adds (kept out of error messages,
// which are shown in the room).
var reChatter = regexp.MustCompile(`^(Cloning into |remote: (Enumerating|Counting|Compressing) objects|remote: Total |[A-Za-z ]+: .*, done\.$)`)

// ParseProgress parses a git progress line.
func ParseProgress(line string) (Progress, bool) {
	m := reProgress.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return Progress{}, false
	}
	pct, _ := strconv.Atoi(m[2])
	done, _ := strconv.ParseInt(m[3], 10, 64)
	total, _ := strconv.ParseInt(m[4], 10, 64)
	return Progress{Phase: m[1], Percent: pct, Done: done, Total: total, Transferred: m[5], Speed: m[6]}, true
}

// Text is a short description: "Receiving objects 45% · 12.00 MiB · 3.00 MiB/s".
func (p Progress) Text() string {
	s := fmt.Sprintf("%s %d%%", p.Phase, p.Percent)
	if p.Transferred != "" {
		s += " · " + p.Transferred
	}
	if p.Speed != "" {
		s += " · " + p.Speed
	}
	return s
}

// progressWriter splits git's stderr into lines (progress lines end in \r) and passes them
// to onLine; other lines are kept in rest for error messages.
type progressWriter struct {
	onLine func(string)
	rest   *bytes.Buffer
	buf    []byte
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		if _, ok := ParseProgress(line); ok {
			w.onLine(line)
		} else if t := strings.TrimSpace(line); t != "" && !reChatter.MatchString(t) {
			w.rest.WriteString(line + "\n")
		}
	}
	if len(w.buf) > 64<<10 { // no newline in a long time: don't grow forever
		w.rest.Write(w.buf)
		w.buf = nil
	}
	return len(p), nil
}
