package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviders(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/gh/repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprintf(w, `[{"tag_name":"v1","name":"One","body":"notes","published_at":"2026-01-02T03:04:05Z",
			"assets":[{"name":"bin.tar.gz","size":5,"url":"x","browser_download_url":"%s/dl/bin"}]},
			{"tag_name":"v0","draft":true,"assets":[]}]`, srv.URL)
	})
	mux.HandleFunc("/gt/repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprintf(w, `[{"tag_name":"v2","prerelease":true,"published_at":"2026-01-02T03:04:05Z",
			"assets":[{"name":"a.zip","size":5,"browser_download_url":"%s/dl/bin"}]}]`, srv.URL)
	})
	mux.HandleFunc("/gl/projects/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.EscapedPath(), "group%2Fsub%2Fp") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `[{"tag_name":"v3","name":"Three","description":"d","released_at":"2026-01-02T03:04:05Z",
			"assets":{"links":[{"name":"pkg.deb","url":"%s/dl/bin"}]}}]`, srv.URL)
	})
	mux.HandleFunc("/dl/bin", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "hello") })
	srv = httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient()
	ctx := context.Background()

	gh, _ := c.New("github", "x", srv.URL+"/gh", "")
	rels, err := gh.ListReleases(ctx, "o", "r")
	if err != nil || len(rels) != 2 || rels[0].Tag != "v1" || !rels[1].Draft || len(rels[0].Assets) != 1 {
		t.Fatalf("github: %v %+v", err, rels)
	}

	gt, _ := c.New("gitea", "x", srv.URL+"/gt", "")
	rels, err = gt.ListReleases(ctx, "o", "r")
	if err != nil || len(rels) != 1 || !rels[0].Prerelease || rels[0].Assets[0].Size != 5 {
		t.Fatalf("gitea: %v %+v", err, rels)
	}

	gl, _ := c.New("gitlab", "x", srv.URL+"/gl", "tok")
	rels, err = gl.ListReleases(ctx, "group/sub", "p")
	if err != nil || len(rels) != 1 || rels[0].Name != "Three" || rels[0].Assets[0].Name != "pkg.deb" {
		t.Fatalf("gitlab: %v %+v", err, rels)
	}
	if _, err := gl.ListReleases(ctx, "nope", "p"); err != ErrNoRepo {
		t.Fatalf("expected ErrNoRepo, got %v", err)
	}

	// Download, skip-existing and size cap.
	dir := t.TempDir()
	a := rels[0].Assets[0]
	if ok, err := c.Download(ctx, a, dir, 0); !ok || err != nil {
		t.Fatalf("download: %v %v", ok, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "pkg.deb")); string(b) != "hello" {
		t.Fatalf("content %q", b)
	}
	if ok, _ := c.Download(ctx, a, dir, 0); ok {
		t.Fatal("expected skip of existing file")
	}
	if _, err := c.Download(ctx, Asset{Name: "big", URL: srv.URL + "/dl/bin"}, dir, 3); err == nil {
		t.Fatal("expected too large error")
	}
	if _, err := os.Stat(filepath.Join(dir, "big.part")); err == nil {
		t.Fatal("partial file left behind")
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"v1.0":          "v1.0",
		"release/2026":  "release_2026",
		"../../etc":     "_.._etc",
		"..":            "",
		"name\x00\nbad": "name__bad",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
}
