package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/gh/repos/o/r":
			fmt.Fprint(w, `{"description":" Zelda 3 ","homepage":"zelda.example","topics":["snes"]}`)
		case "/gitea/repos/o/r":
			fmt.Fprint(w, `{"description":"Gitea one","website":"https://g.example","topics":["a"]}`)
		case "/gl/projects/o%2Fr":
			fmt.Fprint(w, `{"description":"GitLab one","tag_list":["old"]}`)
		case "/gh/repos/o/bad":
			fmt.Fprint(w, `{"description":"x","homepage":"javascript:alert(1)"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient()
	for _, tc := range []struct {
		typ, base, name, want string
	}{
		{"github", "/gh", "r", "{Zelda 3 https://zelda.example [snes]}"},
		{"gitea", "/gitea", "r", "{Gitea one https://g.example [a]}"},
		{"gitlab", "/gl", "r", "{GitLab one  [old]}"},
		{"github", "/gh", "bad", "{x  []}"},
	} {
		p, _ := c.New(tc.typ, "h", srv.URL+tc.base, "")
		info, err := c.Info(context.Background(), p, "o", tc.name)
		if err != nil || fmt.Sprint(info) != tc.want {
			t.Errorf("%s: %v %v, want %s", tc.typ, info, err, tc.want)
		}
	}
	p, _ := c.New("github", "h", srv.URL+"/gh", "")
	if _, err := c.Info(context.Background(), p, "o", "missing"); err == nil {
		t.Error("missing repo should fail")
	}
}
