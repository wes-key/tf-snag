package ado

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gitWikiServer fakes the Azure DevOps Git API for a wiki repository. files maps
// a repo path ("/tf-snag/Exceptions.md") to its markdown.
type gitWikiServer struct {
	files  map[string]string
	pushes []gitPush
	repo   string // name echoed back; empty means "no such repository"
	branch string
}

func newGitWikiServer(t *testing.T, s *gitWikiServer) *Client {
	t.Helper()
	if s.branch == "" {
		s.branch = "wikiMaster"
	}
	if s.files == nil {
		s.files = map[string]string{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case strings.HasSuffix(r.URL.Path, "/pushes"):
			var p gitPush
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &p)
			s.pushes = append(s.pushes, p)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{}`))

		case strings.HasSuffix(r.URL.Path, "/refs"):
			json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]string{{"name": "refs/heads/" + s.branch, "objectId": "tip-sha"}},
			})

		case strings.HasSuffix(r.URL.Path, "/items"):
			content, ok := s.files[q.Get("path")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"content": content})

		default: // repository lookup
			if s.repo == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{
				"id": "repo-id", "name": s.repo, "defaultBranch": "refs/heads/" + s.branch,
			})
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{OrgURL: srv.URL, Project: "proj", Token: "t"}
}

func TestResolveWikiDefaultsToTheProjectWikiRepo(t *testing.T) {
	fake := &gitWikiServer{repo: "proj.wiki"}
	c := newGitWikiServer(t, fake)

	got, err := c.ResolveWiki("")
	if err != nil {
		t.Fatal(err)
	}
	// The repository name IS derivable from the project, unlike the wiki
	// resource's name.
	if got.Name != "proj.wiki" || got.ID != "repo-id" {
		t.Errorf("got %+v", got)
	}
	// Read from the repo, not assumed: a code wiki lives on an ordinary branch.
	if got.Branch != "wikiMaster" {
		t.Errorf("branch = %q", got.Branch)
	}
}

func TestResolveWikiReadsACodeWikiBranch(t *testing.T) {
	c := newGitWikiServer(t, &gitWikiServer{repo: "docs", branch: "main"})
	got, err := c.ResolveWiki("docs")
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "main" {
		t.Errorf("branch = %q, want main", got.Branch)
	}
}

func TestResolveWikiExplainsAMissingRepository(t *testing.T) {
	c := newGitWikiServer(t, &gitWikiServer{}) // no repo
	_, err := c.ResolveWiki("nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "wiki's repository") {
		t.Errorf("error should say what the repository is called: %v", err)
	}
}

func TestPageReportsAbsence(t *testing.T) {
	c := newGitWikiServer(t, &gitWikiServer{repo: "proj.wiki"})
	got, err := c.Page(Wiki{ID: "repo-id", Branch: "wikiMaster"}, "/tf-snag/Exceptions")
	if err != nil {
		t.Fatal(err)
	}
	if got.Exists {
		t.Error("a page that is not in the repo should read as absent")
	}
}

// The page and any missing parent go in one commit: a nested page should not
// land under a heading the wiki has nothing to show for.
func TestPutCommitsPageAndMissingParentTogether(t *testing.T) {
	fake := &gitWikiServer{repo: "proj.wiki"}
	c := newGitWikiServer(t, fake)

	created, err := c.Put(Wiki{ID: "repo-id", Name: "proj.wiki", Branch: "wikiMaster"},
		"/tf-snag/Exceptions", "register", "")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("a page that did not exist should report as created")
	}
	if len(fake.pushes) != 1 {
		t.Fatalf("pushes = %d, want 1", len(fake.pushes))
	}
	p := fake.pushes[0]
	if p.RefUpdates[0].Name != "refs/heads/wikiMaster" || p.RefUpdates[0].OldObjectID != "tip-sha" {
		t.Errorf("refUpdate = %+v — the push must be based on the branch tip", p.RefUpdates[0])
	}
	changes := p.Commits[0].Changes
	if len(changes) != 2 {
		t.Fatalf("changes = %d, want the parent and the page: %+v", len(changes), changes)
	}
	if changes[0].Item.Path != "/tf-snag.md" || changes[0].ChangeType != "add" {
		t.Errorf("parent change = %+v", changes[0])
	}
	if changes[1].Item.Path != "/tf-snag/Exceptions.md" || changes[1].ChangeType != "add" {
		t.Errorf("page change = %+v", changes[1])
	}
	if changes[1].NewContent.Content != "register" || changes[1].NewContent.ContentType != "rawtext" {
		t.Errorf("page content = %+v", changes[1].NewContent)
	}
}

func TestPutEditsAnExistingPageAndLeavesTheParentAlone(t *testing.T) {
	fake := &gitWikiServer{repo: "proj.wiki", files: map[string]string{
		"/tf-snag.md":            "# tf-snag",
		"/tf-snag/Exceptions.md": "old",
	}}
	c := newGitWikiServer(t, fake)

	created, err := c.Put(Wiki{ID: "repo-id", Branch: "wikiMaster"}, "/tf-snag/Exceptions", "new", "")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("an existing page is edited, not created")
	}
	changes := fake.pushes[0].Commits[0].Changes
	if len(changes) != 1 {
		t.Fatalf("an existing parent must not be rewritten: %+v", changes)
	}
	if changes[0].ChangeType != "edit" {
		t.Errorf("changeType = %q, want edit", changes[0].ChangeType)
	}
}

func TestFilePathMapsPagesToMarkdown(t *testing.T) {
	for in, want := range map[string]string{
		"/tf-snag/Exceptions": "/tf-snag/Exceptions.md",
		"tf-snag/Exceptions":  "/tf-snag/Exceptions.md",
		"/Drift Exceptions":   "/Drift-Exceptions.md", // the wiki stores spaces as dashes
		"/a/b/c":              "/a/b/c.md",
	} {
		if got := filePath(in); got != want {
			t.Errorf("filePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAncestors(t *testing.T) {
	for in, want := range map[string][]string{
		"/tf-snag/Exceptions": {"/tf-snag"},
		"/a/b/c":              {"/a", "/a/b"},
		"/Exceptions":         nil,
		"tf-snag/Exceptions":  {"/tf-snag"},
	} {
		got := ancestors(in)
		if len(got) != len(want) {
			t.Errorf("ancestors(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("ancestors(%q) = %v, want %v", in, got, want)
				break
			}
		}
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
