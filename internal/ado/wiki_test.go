package ado

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// wikisServer serves a Wikis - List response and records what was asked for.
func wikisServer(t *testing.T, wikis []map[string]any) (*Client, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"value": wikis, "count": len(wikis)})
	}))
	t.Cleanup(srv.Close)
	return &Client{OrgURL: srv.URL, Project: "proj", Token: "t"}, &seen
}

func TestResolveWikiPrefersTheProjectWiki(t *testing.T) {
	c, _ := wikisServer(t, []map[string]any{
		{"id": "code-1", "name": "Docs", "type": CodeWiki, "repositoryId": "r1",
			"versions": []map[string]string{{"version": "main"}}},
		{"id": "proj-1", "name": "AnythingAtAll", "type": ProjectWiki},
	})
	got, err := c.ResolveWiki("")
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately not named "<project>.wiki": the name is whatever it was
	// created as, which is why this resolves rather than constructs.
	if got.ID != "proj-1" {
		t.Errorf("picked %+v, want the project wiki", got)
	}
}

func TestResolveWikiFallsBackToTheOnlyCodeWiki(t *testing.T) {
	c, _ := wikisServer(t, []map[string]any{
		{"id": "code-1", "name": "Docs", "type": CodeWiki, "repositoryId": "r1", "mappedPath": "/docs",
			"versions": []map[string]string{{"version": "main"}}},
	})
	got, err := c.ResolveWiki("")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "code-1" || got.Branch() != "main" {
		t.Errorf("got %+v, want the single code wiki on branch main", got)
	}
	if !strings.Contains(got.Describe(), "code wiki") || !strings.Contains(got.Describe(), "/docs") {
		t.Errorf("Describe() = %q, should say where to grant permissions", got.Describe())
	}
}

func TestResolveWikiByNameOrID(t *testing.T) {
	wikis := []map[string]any{
		{"id": "a-1", "name": "Alpha", "type": CodeWiki, "repositoryId": "r1"},
		{"id": "b-2", "name": "Beta", "type": CodeWiki, "repositoryId": "r2"},
	}
	c, _ := wikisServer(t, wikis)
	for _, want := range []string{"Beta", "beta", "b-2"} {
		got, err := c.ResolveWiki(want)
		if err != nil {
			t.Fatalf("%q: %v", want, err)
		}
		if got.ID != "b-2" {
			t.Errorf("%q resolved to %+v", want, got)
		}
	}
}

// The failure modes are the point: each should say what to do next.
func TestResolveWikiErrorsAreActionable(t *testing.T) {
	none, _ := wikisServer(t, nil)
	if _, err := none.ResolveWiki(""); err == nil || !strings.Contains(err.Error(), "has no wiki") {
		t.Errorf("no wikis: %v", err)
	}

	ambiguous, _ := wikisServer(t, []map[string]any{
		{"id": "a-1", "name": "Alpha", "type": CodeWiki},
		{"id": "b-2", "name": "Beta", "type": CodeWiki},
	})
	_, err := ambiguous.ResolveWiki("")
	if err == nil || !strings.Contains(err.Error(), "Alpha, Beta") {
		t.Errorf("ambiguous should list the choices, got: %v", err)
	}

	_, err = ambiguous.ResolveWiki("Gamma")
	if err == nil || !strings.Contains(err.Error(), "Alpha, Beta") {
		t.Errorf("unknown name should list what exists, got: %v", err)
	}
}

// A code wiki is a folder on a branch of an ordinary repo; without naming the
// branch the write lands wherever the service defaults to.
func TestWikiURLPinsTheBranchForCodeWikisOnly(t *testing.T) {
	c := &Client{OrgURL: "https://dev.azure.com/org", Project: "proj"}

	code := Wiki{ID: "w1", Type: CodeWiki, Versions: []struct {
		Version string `json:"version"`
	}{{Version: "release/1.0"}}}
	got := c.wikiURL(code, "/tf-snag/Exceptions", "")
	if !strings.Contains(got, "versionDescriptor.versionType=branch") ||
		!strings.Contains(got, "versionDescriptor.version=release%2F1.0") {
		t.Errorf("code wiki URL should pin the branch: %s", got)
	}
	if !strings.Contains(got, "path=%2Ftf-snag%2FExceptions") {
		t.Errorf("page path should be an escaped query parameter: %s", got)
	}

	project := Wiki{ID: "w2", Type: ProjectWiki}
	if got := c.wikiURL(project, "/p", ""); strings.Contains(got, "versionDescriptor") {
		t.Errorf("project wiki needs no branch: %s", got)
	}
}

func TestNormalisePath(t *testing.T) {
	for in, want := range map[string]string{
		"/tf-snag/Exceptions": "/tf-snag/Exceptions",
		"tf-snag/Exceptions":  "/tf-snag/Exceptions",
		"/tf-snag/":           "/tf-snag",
		"":                    "/",
		"  /a  ":              "/a",
	} {
		if got := normalisePath(in); got != want {
			t.Errorf("normalisePath(%q) = %q, want %q", in, got, want)
		}
	}
}
