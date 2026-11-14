package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		code  int
		body  string
		api   bool
		state string
		has   string
	}{
		{200, `{}`, true, Available, ""},
		{404, `{"message":"Not Found"}`, true, Gone, "404"},
		{410, ``, false, Gone, "410"},
		{451, `{"message":"Repository access blocked","block":{"reason":"dmca","html_url":"https://github.com/github/dmca/blob/master/x.md"}}`, true, Legal, "DMCA takedown: https://github.com/github/dmca"},
		{403, `{"message":"Repository access blocked","block":{"reason":"tos"}}`, true, Blocked, "tos"},
		{403, `{"message":"API rate limit exceeded"}`, true, Unknown, ""},
		{500, ``, true, Unknown, ""},
	} {
		st := classify(tc.code, []byte(tc.body), tc.api)
		if st.State != tc.state || !strings.Contains(st.Detail, tc.has) {
			t.Errorf("%d %s: got %+v, want %s containing %q", tc.code, tc.body, st, tc.state, tc.has)
		}
	}
	if st := ClassifyGitError("remote: Repository unavailable due to DMCA takedown."); st.State != Legal {
		t.Errorf("git dmca: %+v", st)
	}
	if st := ClassifyGitError("fatal: could not read Username"); st.State != Unknown {
		t.Errorf("git other: %+v", st)
	}
}

func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/repos/o/alive":
			w.Write([]byte(`{"full_name":"o/alive"}`))
		case "/api/repos/o/flaky":
			http.Error(w, "oops", http.StatusBadGateway)
		case "/web/o/flaky":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient()
	p, _ := c.New("github", "example.org", srv.URL+"/api", "")
	ctx := context.Background()
	if st := c.Probe(ctx, p, "o", "alive", ""); st.State != Available {
		t.Errorf("alive: %+v", st)
	}
	if st := c.Probe(ctx, p, "o", "deleted", ""); st.State != Gone {
		t.Errorf("deleted: %+v", st)
	}
	// API inconclusive → falls back to the web page.
	if st := c.Probe(ctx, p, "o", "flaky", srv.URL+"/web/o/flaky"); st.State != Gone {
		t.Errorf("flaky: %+v", st)
	}
	if st := c.Probe(ctx, nil, "", "", "http://127.0.0.1:1/x"); st.State != Unknown {
		t.Errorf("unreachable: %+v", st)
	}
}
