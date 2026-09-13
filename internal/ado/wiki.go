package ado

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// The wiki is written through the **Git** API, not the Wiki API.
//
// An Azure DevOps wiki is a Git repository of markdown files, and both routes
// reach the same pages. The difference is the scope: the Wiki API needs
// vso.wiki_write, which a pipeline's System.AccessToken does not appear to
// carry, while the Git API needs vso.code_write, which it plainly does — it
// clones the repository every run. Going through Git is what lets the build
// service identity publish without a PAT.
//
// The symptom that sends you here is worth recording: an identity that cannot
// use the Wiki API is not refused, it is shown an empty world. Listing wikis
// returns nothing and writing a page returns 404, so a scope problem reads as a
// missing wiki.

// WikiPermission is what a failure on these calls is asking for.
const WikiPermission = "Contribute on the wiki's repository"

// Wiki is the Git repository behind a wiki, and the branch its pages live on.
type Wiki struct {
	ID     string // repository id
	Name   string // repository name, e.g. "<Project>.wiki"
	Branch string // e.g. "wikiMaster" for a project wiki
}

// DefaultWikiRepo is the repository a project wiki is stored in. Unlike the wiki
// *resource* name, which is whatever the wiki was created as, the repository
// name is derived from the project and is safe to construct.
func DefaultWikiRepo(project string) string { return project + ".wiki" }

func (w Wiki) Describe() string {
	return fmt.Sprintf("%s (git, branch %s)", w.Name, w.Branch)
}

// ResolveWiki looks up the wiki's repository. An empty want uses the project
// wiki's repository. The branch comes from the repository itself rather than
// assuming "wikiMaster", since a code wiki lives on an ordinary branch.
func (c *Client) ResolveWiki(want string) (Wiki, error) {
	if want == "" {
		want = DefaultWikiRepo(c.Project)
	}
	var repo struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		DefaultBranch string `json:"defaultBranch"`
	}
	_, err := c.doWith(reqOpts{
		method:   http.MethodGet,
		endpoint: c.gitURL("repositories/" + url.PathEscape(want)),
		out:      &repo,
		needs:    WikiPermission,
		notFound: "the wiki's repository name — and try its id instead, which resolves when the name does not",
	})
	if err != nil {
		return Wiki{}, fmt.Errorf("%w (looked for repository %q; a project wiki's is \"<project>.wiki\", a code "+
			"wiki's is the repository it was published from, and -wiki also accepts the repository id)", err, want)
	}
	branch := strings.TrimPrefix(repo.DefaultBranch, "refs/heads/")
	if branch == "" {
		branch = "wikiMaster"
	}
	return Wiki{ID: repo.ID, Name: repo.Name, Branch: branch}, nil
}

// WikiPage is one page's current state.
type WikiPage struct {
	Content string
	// ETag is unused on this route: Git guards concurrency with the branch tip,
	// which Put reads for itself immediately before pushing. Kept so the caller's
	// contract is the same either way.
	ETag   string
	Exists bool
}

// Page reads a wiki page's markdown from the repository.
func (c *Client) Page(w Wiki, path string) (WikiPage, error) {
	var item struct {
		Content string `json:"content"`
	}
	resp, err := c.doWith(reqOpts{
		method: http.MethodGet,
		endpoint: c.gitURL("repositories/"+url.PathEscape(w.ID)+"/items") +
			"&path=" + url.QueryEscape(filePath(path)) +
			"&versionDescriptor.versionType=branch&versionDescriptor.version=" + url.QueryEscape(w.Branch) +
			"&includeContent=true",
		out:      &item,
		needs:    WikiPermission,
		allow404: true,
	})
	if err != nil {
		return WikiPage{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return WikiPage{}, nil
	}
	return WikiPage{Content: item.Content, Exists: true}, nil
}

// Put commits the page onto the wiki branch.
//
// One commit, whatever it takes: the page itself, plus any parent page that does
// not exist yet, so a nested page does not land under a heading the wiki has
// nothing to show for. Git needs no parent to create a nested path — this is
// about how the page reads, not whether the write succeeds.
func (c *Client) Put(w Wiki, path, content, _ string) (created bool, err error) {
	tip, err := c.branchTip(w)
	if err != nil {
		return false, err
	}

	var changes []gitChange
	for _, parent := range ancestors(path) {
		existing, err := c.Page(w, parent)
		if err != nil {
			return false, err
		}
		if !existing.Exists {
			changes = append(changes, newChange("add", parent, parentStub(parent)))
		}
	}

	existing, err := c.Page(w, path)
	if err != nil {
		return false, err
	}
	created = !existing.Exists
	changeType := "edit"
	if created {
		changeType = "add"
	}
	changes = append(changes, newChange(changeType, path, content))

	push := gitPush{
		RefUpdates: []gitRefUpdate{{Name: "refs/heads/" + w.Branch, OldObjectID: tip}},
		Commits: []gitCommit{{
			Comment: "tf-snag: update " + normalisePath(path),
			Changes: changes,
		}},
	}
	_, err = c.doWith(reqOpts{
		method:      http.MethodPost,
		endpoint:    c.gitURL("repositories/" + url.PathEscape(w.ID) + "/pushes"),
		body:        push,
		contentType: "application/json",
		needs:       WikiPermission,
	})
	return created, err
}

// branchTip is the commit the push must be based on. Read immediately before
// pushing: a stale one is rejected rather than overwriting somebody's commit,
// which is the behaviour worth having.
func (c *Client) branchTip(w Wiki) (string, error) {
	var refs struct {
		Value []struct {
			ObjectID string `json:"objectId"`
		} `json:"value"`
	}
	_, err := c.doWith(reqOpts{
		method: http.MethodGet,
		endpoint: c.gitURL("repositories/"+url.PathEscape(w.ID)+"/refs") +
			"&filter=" + url.QueryEscape("heads/"+w.Branch),
		out:   &refs,
		needs: WikiPermission,
	})
	if err != nil {
		return "", err
	}
	if len(refs.Value) == 0 {
		return "", fmt.Errorf("ado: wiki repository %s has no branch %q", w.Name, w.Branch)
	}
	return refs.Value[0].ObjectID, nil
}

type gitPush struct {
	RefUpdates []gitRefUpdate `json:"refUpdates"`
	Commits    []gitCommit    `json:"commits"`
}

type gitRefUpdate struct {
	Name        string `json:"name"`
	OldObjectID string `json:"oldObjectId"`
}

type gitCommit struct {
	Comment string      `json:"comment"`
	Changes []gitChange `json:"changes"`
}

type gitChange struct {
	ChangeType string `json:"changeType"`
	Item       struct {
		Path string `json:"path"`
	} `json:"item"`
	NewContent struct {
		Content     string `json:"content"`
		ContentType string `json:"contentType"`
	} `json:"newContent"`
}

func newChange(changeType, page, content string) gitChange {
	var ch gitChange
	ch.ChangeType = changeType
	ch.Item.Path = filePath(page)
	ch.NewContent.Content = content
	ch.NewContent.ContentType = "rawtext"
	return ch
}

func (c *Client) gitURL(path string) string {
	return fmt.Sprintf("%s/%s/_apis/git/%s?api-version=%s",
		c.OrgURL, url.PathEscape(c.Project), path, apiVersion)
}

// filePath maps a wiki page path to the markdown file backing it: the wiki
// renders "/tf-snag/Exceptions" from "/tf-snag/Exceptions.md". Spaces are stored
// as dashes, which is how the wiki writes them itself.
func filePath(page string) string {
	p := normalisePath(page)
	if p == "/" {
		return "/"
	}
	return strings.ReplaceAll(p, " ", "-") + ".md"
}

// parentStub is deliberately thin. It exists so the page above a nested one is
// not blank, and says why it is there.
func parentStub(path string) string {
	name := path[strings.LastIndex(path, "/")+1:]
	return "# " + name + "\n\nCreated by tf-snag so its pages below this one have a parent.\n"
}

// ancestors lists the parent pages of a path, outermost first: /a/b/c -> /a, /a/b.
func ancestors(path string) []string {
	parts := strings.Split(strings.Trim(normalisePath(path), "/"), "/")
	if len(parts) < 2 {
		return nil
	}
	out := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		out = append(out, "/"+strings.Join(parts[:i], "/"))
	}
	return out
}

// normalisePath makes a page path absolute. Azure DevOps treats a leading slash
// as the wiki root; without one the page lands somewhere unintended.
func normalisePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}
