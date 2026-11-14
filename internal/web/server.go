// Package web serves a read-only file browser over the storage directory, with per-file
// downloads and on-the-fly zip downloads of folders.
package web

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/ricardo-duarte-av/matrix-archivist/internal/config"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/gitops"
	"github.com/ricardo-duarte-av/matrix-archivist/internal/live"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	cfg  config.HTTP
	dir  string
	log  zerolog.Logger
	tmpl *template.Template
	zips chan struct{} // semaphore for concurrent zip generation

	// Metrics writes Prometheus metrics; served at /metrics when set and http.metrics is on.
	Metrics func(ctx context.Context, w io.Writer) error
	// Live is the archiver's activity hub; the live page is served when set and http.live is on.
	Live *live.Hub

	streams atomic.Int32 // open /live/events connections
}

func New(cfg config.HTTP, storageDir string, log zerolog.Logger) (*Server, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"size": gitops.HumanSize,
		"path": escapePath,
		"time": func(t time.Time) string { return t.Format("2006-01-02 15:04") },
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:  cfg,
		dir:  storageDir,
		log:  log,
		tmpl: tmpl,
		zips: make(chan struct{}, cfg.MaxConcurrentZips),
	}, nil
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(24*time.Hour, http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/browse/", http.StatusFound) })
	mux.HandleFunc("GET /browse/{path...}", s.handleBrowse)
	mux.HandleFunc("GET /download/{path...}", s.handleDownload)
	mux.HandleFunc("GET /zip/{path...}", s.handleZip)
	if s.cfg.Metrics && s.Metrics != nil {
		mux.HandleFunc("GET /metrics", s.handleMetrics)
	}
	if s.cfg.Live && s.Live != nil {
		mux.HandleFunc("GET /live/{$}", s.handleLivePage)
		mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/live/", http.StatusMovedPermanently)
		})
		mux.HandleFunc("GET /live/events", s.handleLiveEvents)
	}

	var h http.Handler = mux
	if s.cfg.Username != "" {
		h = s.basicAuth(h)
	}
	return securityHeaders(h)
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr(),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: large downloads and zips can take a long time.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.log.Info().Str("addr", srv.Addr).Msg("HTTP file browser listening")
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// cleanPath turns a URL path value into a clean path relative to the storage root ("." for root).
func cleanPath(p string) string {
	c := path.Clean("/" + p)
	if c == "/" {
		return "."
	}
	return strings.TrimPrefix(c, "/")
}

// openRoot opens the storage dir as an os.Root, which refuses any path (including via symlinks)
// that escapes it.
func (s *Server) openRoot() (*os.Root, error) {
	return os.OpenRoot(s.dir)
}

type entry struct {
	Name    string
	Path    string // relative to root, slash-separated
	IsDir   bool
	Size    int64
	ModTime time.Time
}

type crumb struct {
	Name string
	Path string
}

type listing struct {
	Path    string
	Crumbs  []crumb
	Entries []entry
	Parent  string
	IsRoot  bool
	ZipName string
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	rel := cleanPath(r.PathValue("path"))
	root, err := s.openRoot()
	if err != nil {
		s.fail(w, err)
		return
	}
	defer root.Close()
	if err := checkPath(root, rel); err != nil {
		s.fail(w, err)
		return
	}

	st, err := root.Stat(rel)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !st.IsDir() {
		http.Redirect(w, r, "/download/"+escapePath(rel), http.StatusFound)
		return
	}
	// Canonical directory URLs end with a slash so relative links work.
	if !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	}

	f, err := root.Open(rel)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	dirents, err := f.ReadDir(-1)
	if err != nil {
		s.fail(w, err)
		return
	}

	data := listing{Path: rel, IsRoot: rel == "."}
	for _, d := range dirents {
		info, err := d.Info()
		if err != nil {
			continue
		}
		p := d.Name()
		if rel != "." {
			p = rel + "/" + d.Name()
		}
		isDir := d.IsDir()
		// Symlinks are never followed (an archived repo could point one at another repo's
		// git config), and git config files can hold credentials from the clone URL.
		if !d.Type().IsRegular() && !isDir || hiddenPath(p) {
			continue
		}
		data.Entries = append(data.Entries, entry{Name: d.Name(), Path: p, IsDir: isDir, Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(data.Entries, func(i, j int) bool {
		a, b := data.Entries[i], data.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})

	data.Crumbs = []crumb{{Name: "storage", Path: ""}}
	if rel != "." {
		parts := strings.Split(rel, "/")
		for i := range parts {
			data.Crumbs = append(data.Crumbs, crumb{Name: parts[i], Path: strings.Join(parts[:i+1], "/")})
		}
		data.Parent = path.Dir(rel)
		if data.Parent == "." {
			data.Parent = ""
		}
	}
	data.ZipName = zipName(rel)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		s.log.Warn().Err(err).Msg("Template error")
	}
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	rel := cleanPath(r.PathValue("path"))
	root, err := s.openRoot()
	if err != nil {
		s.fail(w, err)
		return
	}
	defer root.Close()
	if err := checkPath(root, rel); err != nil {
		s.fail(w, err)
		return
	}
	f, err := root.Open(rel)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.fail(w, err)
		return
	}
	if st.IsDir() {
		http.Redirect(w, r, "/browse/"+escapePath(rel)+"/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Disposition", attachment(st.Name()))
	w.Header().Set("Content-Type", "application/octet-stream")
	// ServeContent handles Range requests, so interrupted downloads can resume.
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

func (s *Server) handleZip(w http.ResponseWriter, r *http.Request) {
	rel := cleanPath(r.PathValue("path"))
	root, err := s.openRoot()
	if err != nil {
		s.fail(w, err)
		return
	}
	defer root.Close()
	if err := checkPath(root, rel); err != nil {
		s.fail(w, err)
		return
	}
	st, err := root.Stat(rel)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !st.IsDir() {
		http.Error(w, "not a directory", http.StatusBadRequest)
		return
	}

	select {
	case s.zips <- struct{}{}:
		defer func() { <-s.zips }()
	case <-r.Context().Done():
		return
	case <-time.After(30 * time.Second):
		http.Error(w, "too many zip downloads in progress, try again shortly", http.StatusServiceUnavailable)
		return
	}

	name := zipName(rel)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", attachment(name+".zip"))
	start := time.Now()
	n, err := writeZip(r.Context(), w, root.FS(), rel, name)
	if err != nil {
		// Headers are already sent; the client sees a truncated download.
		s.log.Warn().Err(err).Str("path", rel).Msg("Zip download aborted")
		return
	}
	s.log.Info().Str("path", rel).Int("files", n).Dur("took", time.Since(start)).Msg("Served zip")
}

// writeZip streams all regular files under dir (in fsys) into a zip, inside a top folder named
// prefix. Symlinks and special files are skipped. Returns the number of files written.
func writeZip(ctx context.Context, w io.Writer, fsys fs.FS, dir, prefix string) (int, error) {
	zw := zip.NewWriter(w)
	count := 0
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel := p
		if dir != "." {
			rel = strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		}
		name := prefix
		if rel != "" && rel != "." {
			name = prefix + "/" + rel
		}
		if d.IsDir() {
			_, err := zw.CreateHeader(&zip.FileHeader{Name: name + "/", Method: zip.Store})
			return err
		}
		if !d.Type().IsRegular() || hiddenPath(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Method = zip.Deflate
		if alreadyCompressed(d.Name()) {
			hdr.Method = zip.Store
		}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := fsys.Open(p)
		if err != nil {
			return nil
		}
		_, err = io.Copy(fw, f)
		f.Close()
		if err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		return count, err
	}
	return count, zw.Close()
}

var compressedExt = map[string]bool{
	".pack": true, ".zip": true, ".gz": true, ".tgz": true, ".xz": true, ".zst": true, ".bz2": true,
	".7z": true, ".rar": true, ".jar": true, ".apk": true, ".deb": true, ".rpm": true, ".dmg": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".mp4": true, ".mkv": true,
	".mp3": true, ".ogg": true, ".flac": true, ".woff2": true, ".exe": true, ".msi": true, ".appimage": true,
}

func alreadyCompressed(name string) bool {
	return compressedExt[strings.ToLower(path.Ext(name))]
}

func zipName(rel string) string {
	if rel == "." {
		return "storage"
	}
	return path.Base(rel)
}

func attachment(name string) string {
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": name}); v != "" {
		return v
	}
	return "attachment"
}

// errHidden is returned for paths the browser refuses to serve.
var errHidden = fmt.Errorf("%w: hidden path", fs.ErrNotExist)

// checkPath refuses paths that go through a symlink or point at a hidden file. (os.Root
// already stops symlinks leaving the storage dir; this also stops those that stay inside.)
func checkPath(root *os.Root, rel string) error {
	if rel == "." {
		return nil
	}
	if hiddenPath(rel) {
		return errHidden
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		st, err := root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return errHidden
		}
	}
	return nil
}

// hiddenPath reports git config files (mirror.git/config, checkout/.git/config): they may
// contain credentials from a clone URL like https://user:token@host/….
func hiddenPath(rel string) bool {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if parts[i] == "config" && (parts[i-1] == ".git" || strings.HasSuffix(parts[i-1], ".git")) {
			return true
		}
	}
	return false
}

// escapePath URL-escapes each segment of a slash-separated path.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, fs.ErrPermission), strings.Contains(err.Error(), "escapes from parent"), strings.Contains(err.Error(), "path escapes"):
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		// Details (which include server paths) go to the log only.
		s.log.Warn().Err(err).Msg("HTTP error")
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if err := s.Metrics(r.Context(), &buf); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = buf.WriteTo(w)
}

func (s *Server) basicAuth(next http.Handler) http.Handler {
	user, pass := []byte(s.cfg.Username), []byte(s.cfg.Password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), user) != 1 || subtle.ConstantTimeCompare([]byte(p), pass) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="archivist", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func cacheFor(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		next.ServeHTTP(w, r)
	})
}
