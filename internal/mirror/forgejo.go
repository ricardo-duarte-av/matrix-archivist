// Package mirror publishes archived repositories to a self-hosted Forgejo (or Gitea) instance,
// as a read-only copy for browsing. The archive stays the authority: the forge copy is
// overwritten from it on every sync.
package mirror

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrNotFound is returned for a 404 from the forge API.
var ErrNotFound = errors.New("not found on forge")

// Client talks to the Forgejo/Gitea API with a token.
type Client struct {
	Base  string // e.g. http://forgejo:3000 (no trailing slash)
	Token string
	HTTP  *http.Client

	login string // the token's user, filled by Login
}

func NewClient(base, token string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Token: token, HTTP: &http.Client{}}
}

// APIError is a non-2xx answer from the forge.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("forge %s %s: %d %s", e.Method, e.Path, e.Status, msg)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req, method, path, out)
}

func (c *Client) send(req *http.Request, method, path string, out any) error {
	req.Header.Set("Authorization", "token "+c.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
			if len(e.Message) > 200 {
				e.Message = e.Message[:200] + "…"
			}
		}
		return &APIError{Status: resp.StatusCode, Method: method, Path: path, Message: e.Message}
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

// Login checks the token and returns the user it belongs to.
func (c *Client) Login(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, "GET", "/user", nil, &u); err != nil {
		return "", err
	}
	c.login = u.Login
	return u.Login, nil
}

// PushAuth is the value of an HTTP Authorization header git can use to push as the token's user.
func (c *Client) PushAuth() string {
	user := c.login
	if user == "" {
		user = "archivist"
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+c.Token))
}

// GitURL is the clone/push URL of a repository on the forge.
func (c *Client) GitURL(owner, name string) string {
	return c.Base + "/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + ".git"
}

func repoPath(owner, name string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// EnsureOrg creates the organization unless it (or the token's own user) already exists.
func (c *Client) EnsureOrg(ctx context.Context, org, description, website string, private bool) error {
	if strings.EqualFold(org, c.login) {
		return nil
	}
	err := c.do(ctx, "GET", "/orgs/"+url.PathEscape(org), nil, nil)
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	vis := "public"
	if private {
		vis = "private"
	}
	return c.do(ctx, "POST", "/orgs", map[string]any{
		"username":    org,
		"visibility":  vis,
		"description": description,
		"website":     website,
	}, nil)
}

// Repo is the part of a forge repository the archiver looks at.
type Repo struct {
	Archived      bool   `json:"archived"`
	Empty         bool   `json:"empty"`
	DefaultBranch string `json:"default_branch"`
}

// GetRepo returns the repository, or ErrNotFound.
func (c *Client) GetRepo(ctx context.Context, owner, name string) (*Repo, error) {
	var r Repo
	if err := c.do(ctx, "GET", repoPath(owner, name), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateRepo creates an empty repository in an organization (or in the token user's account
// when owner is that user). defaultBranch may be "".
func (c *Client) CreateRepo(ctx context.Context, owner, name, description, defaultBranch string, private bool) error {
	path := "/orgs/" + url.PathEscape(owner) + "/repos"
	if strings.EqualFold(owner, c.login) {
		path = "/user/repos"
	}
	body := map[string]any{
		"name":        name,
		"description": description,
		"private":     private,
	}
	if defaultBranch != "" {
		body["default_branch"] = defaultBranch
	}
	return c.do(ctx, "POST", path, body, nil)
}

// Settings are applied to every mirrored repository.
type Settings struct {
	Description   string
	Website       string
	DefaultBranch string // "" leaves it unchanged
	Private       bool
	Archived      bool
}

// EditRepo applies settings and turns off everything a read-only mirror doesn't need
// (issues, pull requests, wiki, projects, packages, actions).
func (c *Client) EditRepo(ctx context.Context, owner, name string, s Settings) error {
	body := map[string]any{
		"description":       s.Description,
		"website":           s.Website,
		"private":           s.Private,
		"archived":          s.Archived,
		"has_issues":        false,
		"has_wiki":          false,
		"has_pull_requests": false,
		"has_projects":      false,
		"has_packages":      false,
		"has_actions":       false,
		"has_releases":      true,
	}
	if s.DefaultBranch != "" {
		body["default_branch"] = s.DefaultBranch
	}
	return c.do(ctx, "PATCH", repoPath(owner, name), body, nil)
}

// SetArchived only flips a repository's archived (read-only) flag.
func (c *Client) SetArchived(ctx context.Context, owner, name string, archived bool) error {
	return c.do(ctx, "PATCH", repoPath(owner, name), map[string]any{"archived": archived}, nil)
}

// SetTopics replaces a repository's topics.
func (c *Client) SetTopics(ctx context.Context, owner, name string, topics []string) error {
	if topics == nil {
		topics = []string{}
	}
	return c.do(ctx, "PUT", repoPath(owner, name)+"/topics", map[string]any{"topics": topics}, nil)
}

// DeleteRepo deletes a repository from the forge.
func (c *Client) DeleteRepo(ctx context.Context, owner, name string) error {
	return c.do(ctx, "DELETE", repoPath(owner, name), nil, nil)
}

// Release is a release on the forge.
type Release struct {
	ID         int64        `json:"id"`
	Tag        string       `json:"tag_name"`
	Name       string       `json:"name"`
	Body       string       `json:"body"`
	Prerelease bool         `json:"prerelease"`
	Draft      bool         `json:"draft"`
	Assets     []Attachment `json:"assets"`
}

// Attachment is a file attached to a release.
type Attachment struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Releases lists every release of a repository.
func (c *Client) Releases(ctx context.Context, owner, name string) ([]Release, error) {
	var all []Release
	for page := 1; page <= 1000; page++ {
		var batch []Release
		if err := c.do(ctx, "GET", fmt.Sprintf("%s/releases?page=%d&limit=50", repoPath(owner, name), page), nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) == 0 {
			break
		}
	}
	return all, nil
}

// ReleaseOptions are the fields of a release the archiver sets.
type ReleaseOptions struct {
	Tag        string `json:"tag_name"`
	Name       string `json:"name"`
	Body       string `json:"body"`
	Prerelease bool   `json:"prerelease"`
}

// CreateRelease creates a release for an existing tag.
func (c *Client) CreateRelease(ctx context.Context, owner, name string, o ReleaseOptions) (*Release, error) {
	var r Release
	if err := c.do(ctx, "POST", repoPath(owner, name)+"/releases", o, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// EditRelease updates a release's title, notes and pre-release flag.
func (c *Client) EditRelease(ctx context.Context, owner, name string, id int64, o ReleaseOptions) error {
	return c.do(ctx, "PATCH", fmt.Sprintf("%s/releases/%d", repoPath(owner, name), id), map[string]any{
		"name": o.Name, "body": o.Body, "prerelease": o.Prerelease, "draft": false,
	}, nil)
}

// DeleteRelease deletes a release (its tag stays).
func (c *Client) DeleteRelease(ctx context.Context, owner, name string, id int64) error {
	return c.do(ctx, "DELETE", fmt.Sprintf("%s/releases/%d", repoPath(owner, name), id), nil, nil)
}

// DeleteAttachment removes a file from a release.
func (c *Client) DeleteAttachment(ctx context.Context, owner, name string, releaseID, id int64) error {
	return c.do(ctx, "DELETE", fmt.Sprintf("%s/releases/%d/assets/%d", repoPath(owner, name), releaseID, id), nil, nil)
}

// UploadAttachment uploads a local file to a release under the given name, streaming it.
func (c *Client) UploadAttachment(ctx context.Context, owner, name string, releaseID int64, fileName, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("attachment", fileName)
		if err == nil {
			_, err = io.Copy(part, f)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	p := fmt.Sprintf("%s/releases/%d/assets?name=%s", repoPath(owner, name), releaseID, url.QueryEscape(fileName))
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base+"/api/v1"+p, pr)
	if err != nil {
		pr.Close()
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	err = c.send(req, "POST", p, nil)
	pr.Close()
	return err
}
