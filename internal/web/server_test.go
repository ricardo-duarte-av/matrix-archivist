package web

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
)

func setup(t *testing.T, auth bool) (*httptest.Server, string) {
	t.Helper()
	tmp := t.TempDir()
	storage := filepath.Join(tmp, "repos")
	write := func(p, content string) {
		full := filepath.Join(storage, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("github.com/o/r/checkout/README.md", "hello readme")
	write("github.com/o/r/checkout/src/main.go", "package main")
	write("github.com/o/r/releases/v1 #1/assets/bin.tar.gz", "binary")
	os.WriteFile(filepath.Join(tmp, "secret.txt"), []byte("TOP SECRET"), 0o644)
	// Symlinks pointing outside the storage root must never be served.
	os.Symlink(filepath.Join(tmp, "secret.txt"), filepath.Join(storage, "github.com/o/r/checkout/evil-link"))
	os.Symlink(tmp, filepath.Join(storage, "github.com/o/r/checkout/evil-dir"))

	cfg := config.HTTP{Enabled: true, Port: 0, MaxConcurrentZips: 1}
	if auth {
		cfg.Username, cfg.Password = "u", "p"
	}
	s, err := New(cfg, storage, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, storage
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestBrowseAndDownload(t *testing.T) {
	srv, _ := setup(t, false)

	resp, body := get(t, srv.URL+"/")
	if resp.StatusCode != 200 || !strings.Contains(body, "github.com") {
		t.Fatalf("root: %d %s", resp.StatusCode, body)
	}

	_, body = get(t, srv.URL+"/browse/github.com/o/r/checkout/")
	for _, want := range []string{"README.md", "/download/github.com/o/r/checkout/README.md", "/zip/github.com/o/r/checkout/src", "src"} {
		if !strings.Contains(body, want) {
			t.Errorf("listing missing %q", want)
		}
	}
	if strings.Contains(body, "evil-link") || strings.Contains(body, "evil-dir") {
		t.Error("listing shows symlinks escaping the root")
	}
	// Names needing escaping
	_, body = get(t, srv.URL+"/browse/github.com/o/r/releases/")
	if !strings.Contains(body, "/browse/github.com/o/r/releases/v1%20%231/") {
		t.Errorf("segment not escaped:\n%s", body)
	}

	resp, body = get(t, srv.URL+"/download/github.com/o/r/checkout/README.md")
	if resp.StatusCode != 200 || body != "hello readme" || !strings.Contains(resp.Header.Get("Content-Disposition"), "README.md") {
		t.Fatalf("download: %d %q %v", resp.StatusCode, body, resp.Header)
	}

	// Range requests (resumable downloads)
	req, _ := http.NewRequest("GET", srv.URL+"/download/github.com/o/r/checkout/README.md", nil)
	req.Header.Set("Range", "bytes=6-")
	r2, _ := http.DefaultClient.Do(req)
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != http.StatusPartialContent || string(b2) != "readme" {
		t.Errorf("range: %d %q", r2.StatusCode, b2)
	}
}

func TestNoEscape(t *testing.T) {
	srv, _ := setup(t, false)
	for _, p := range []string{
		"/download/../secret.txt",
		"/download/%2e%2e/secret.txt",
		"/download/github.com/o/r/checkout/evil-link",
		"/download/github.com/o/r/checkout/evil-dir/secret.txt",
		"/browse/github.com/o/r/checkout/evil-dir/",
		"/zip/github.com/o/r/checkout/evil-dir",
	} {
		resp, body := get(t, srv.URL+p)
		if resp.StatusCode == 200 || strings.Contains(body, "TOP SECRET") {
			t.Errorf("%s: served (status %d)", p, resp.StatusCode)
		}
	}
}

func TestZip(t *testing.T) {
	srv, _ := setup(t, false)
	resp, body := get(t, srv.URL+"/zip/github.com/o/r/checkout")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %d %v", resp.StatusCode, resp.Header)
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "checkout.zip") {
		t.Errorf("disposition: %s", resp.Header.Get("Content-Disposition"))
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(body)), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if strings.HasSuffix(f.Name, "README.md") {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			if string(b) != "hello readme" {
				t.Errorf("zip content %q", b)
			}
		}
	}
	sort.Strings(names)
	want := []string{"checkout/", "checkout/README.md", "checkout/src/", "checkout/src/main.go"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("zip entries %v, want %v", names, want)
	}

	if resp, _ := get(t, srv.URL+"/zip/github.com/o/r/checkout/README.md"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("zip of file: %d", resp.StatusCode)
	}
}

func TestBasicAuth(t *testing.T) {
	srv, _ := setup(t, true)
	if resp, _ := get(t, srv.URL+"/browse/"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no auth: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/browse/", nil)
	req.SetBasicAuth("u", "p")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("with auth: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	if resp, _ := get(t, srv.URL+"/healthz"); resp.StatusCode != 200 {
		t.Errorf("healthz should be open: %d", resp.StatusCode)
	}
	if resp, _ := get(t, srv.URL+"/static/style.css"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("static without auth: %d", resp.StatusCode)
	}
}
