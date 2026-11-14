package readme

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestExtract(t *testing.T) {
	for _, tc := range []struct {
		name, in, title, summary string
		links                    []string
	}{
		{"markdown", "# Zelda3\n[![Build](https://x/badge.svg)](https://x)\n\nA reimplementation of Zelda 3.\n\nOur discord server is: https://discord.gg/AJJbJAzNNJ\n",
			"Zelda3", "A reimplementation of Zelda 3.", []string{"discord.gg/AJJbJAzNNJ"}},
		{"centered html", `<div align="center"><img src="logo.png" alt="Logo">
<p align="center"><a href="https://site.dev">Official Website</a> • <a href="https://discord.gg/6NpMhefCK9">Discord</a></p></div>

# Overview

Dusklight is a reverse-engineered **reimplementation** of [Twilight Princess](https://example.org).
`, "", "Dusklight is a reverse-engineered reimplementation of Twilight Princess.", []string{"discord.gg/6NpMhefCK9"}},
		{"html heading and alt title", "<h1 align=\"center\">My&nbsp;Tool</h1>\n\n```\nnot this paragraph at all please\n```\n\nIt does useful things for people.\n\nJoin https://matrix.to/#/#room:example.org or t.me/mygroup.",
			"My Tool", "It does useful things for people.", []string{"matrix.to/#/#room:example.org", "t.me/mygroup"}},
		{"setext", "Project Name\n============\n\n- list item that is not a summary at all\n\nThe real summary paragraph,\nwrapped over two lines.\n",
			"Project Name", "The real summary paragraph, wrapped over two lines.", nil},
		{"image alt title", "![Ship of Harkinian](docs/title.png)\n\n* [Discord](https://discord.com/invite/harbourmasters)\n",
			"Ship of Harkinian", "", []string{"discord.com/invite/harbourmasters"}},
		{"empty", "", "", "", nil},
	} {
		got := Extract(tc.in)
		if got.Title != tc.title || got.Summary != tc.summary || !reflect.DeepEqual(got.Links, tc.links) {
			t.Errorf("%s:\n got  %q / %q / %q\n want %q / %q / %q", tc.name, got.Title, got.Summary, got.Links, tc.title, tc.summary, tc.links)
		}
	}
}

func TestShortenAndCap(t *testing.T) {
	long := ""
	for range 100 {
		long += "word "
	}
	if s := Extract("# T\n\n" + long).Summary; len([]rune(s)) > 201 || s[len(s)-3:] != "…" {
		t.Errorf("summary not shortened: %q", s)
	}
	huge := make([]byte, MaxText*2)
	for i := range huge {
		huge[i] = 'a'
	}
	if n := len(Extract(string(huge)).Text); n > MaxText {
		t.Errorf("text not capped: %d", n)
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	if Find(dir) != "" {
		t.Error("found README in empty dir")
	}
	os.WriteFile(filepath.Join(dir, "README.rst"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "readme.MD"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(dir, "README"), 0o755)
	if got := filepath.Base(Find(dir)); got != "readme.MD" {
		t.Errorf("preferred %s", got)
	}
	// A symlinked README pointing elsewhere is ignored.
	link := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret"), []byte("secret"), 0o644)
	os.Symlink(filepath.Join(dir, "secret"), filepath.Join(link, "README.md"))
	if Find(link) != "" {
		t.Error("followed symlink")
	}
	if _, ok := Read(link); ok {
		t.Error("read symlink")
	}
}
