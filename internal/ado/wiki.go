package ado

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// WikiPermission is what a 403 on these calls is asking for. Work items and the
// wiki are separate scopes, so a token that raises items happily can still be
// refused here — worth saying which one is missing.
const WikiPermission = "Wiki (Read & Write), and Contribute on the wiki's backing repository"

// Wiki types, as the API reports them.
const (
	ProjectWiki = "projectWiki" // provisioned for the project
	CodeWiki    = "codeWiki"    // published from a folder in a Git repo
)

// Wiki is one wiki in the project.
//
// Note Name is NOT derivable: a project wiki's backing *repository* is called
// "<Project>.wiki", but the wiki resource itself is named whatever it was
// created as. Guessing it is how you end up writing to a wiki that is not there,
// which is why nothing here constructs an identifier.
type Wiki struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	RepositoryID string `json:"repositoryId"`
	MappedPath   string `json:"mappedPath"`
	Versions     []struct {
		Version string `json:"version"`
	} `json:"versions"`
}

// Branch is the version a code wiki is published from. Empty for a project
// wiki, which has only the one branch the service manages.
func (w Wiki) Branch() string {
	if w.Type != CodeWiki || len(w.Versions) == 0 {
		return ""
	}
	return w.Versions[0].Version
}

// Describe names the wiki and what backs it, so a permission failure says where
// to go: the two types are governed by different repositories.
func (w Wiki) Describe() string {
	switch w.Type {
	case CodeWiki:
		s := fmt.Sprintf("%s (code wiki, repo %s", w.Name, w.RepositoryID)
		if b := w.Branch(); b != "" {
			s += ", branch " + b
		}
		if w.MappedPath != "" && w.MappedPath != "/" {
			s += ", under " + w.MappedPath
		}
		return s + ")"
	case ProjectWiki:
		return w.Name + " (project wiki)"
	default:
		return w.Name
	}
}

// Wikis lists the project's wikis. Read-only, so it needs only the wiki read
// scope — which makes it a cheap pre-check that the token can see anything at
// all before a write is attempted.
func (c *Client) Wikis() ([]Wiki, error) {
	var body struct {
		Value []Wiki `json:"value"`
	}
	_, err := c.doWith(reqOpts{
		method:   http.MethodGet,
		endpoint: c.wikisEndpoint(),
		out:      &body,
		needs:    WikiPermission,
	})
	return body.Value, err
}

func (c *Client) wikisEndpoint() string {
	return c.projectURL("wiki/wikis") + "?api-version=" + apiVersion
}

// ResolveWiki finds the wiki to write to. An empty want picks the project wiki,
// or the only wiki when there is exactly one of any kind.
//
// It resolves rather than assumes on purpose: a project can have a provisioned
// wiki, any number of code wikis, or none at all, and the caller should not have
// to know which before it can publish.
func (c *Client) ResolveWiki(want string) (Wiki, error) {
	all, err := c.Wikis()
	if err != nil {
		return Wiki{}, err
	}

	// A named wiki is used whether or not discovery saw it. The list reflects
	// what this identity is allowed to enumerate, which is not always what it is
	// allowed to write; refusing an explicit instruction because a discovery call
	// came back thin turns a working configuration into a dead end.
	if want != "" && len(all) == 0 {
		return Wiki{ID: want, Name: want}, nil
	}

	if len(all) == 0 {
		// An empty list is not proof there is no wiki. Azure DevOps filters out
		// what the caller cannot see rather than refusing, and answers a write to
		// an invisible wiki with 404 rather than 403 — so "none" and "none you
		// are allowed to see" are indistinguishable here, and saying only the
		// first sends people off to create a wiki they already have.
		return Wiki{}, fmt.Errorf("ado: %s returned no wiki this identity can see — either the project has "+
			"none (create one: Overview > Wiki), or the identity cannot read it (from the wiki page: "+
			"... > Wiki security, grant Read and Contribute). Name it with -wiki to skip discovery entirely",
			c.wikisEndpoint())
	}

	if want != "" {
		for _, w := range all {
			if strings.EqualFold(w.Name, want) || strings.EqualFold(w.ID, want) {
				return w, nil
			}
		}
		return Wiki{}, fmt.Errorf("ado: no wiki named %q in %s — it has: %s",
			want, c.Project, strings.Join(names(all), ", "))
	}

	for _, w := range all {
		if w.Type == ProjectWiki {
			return w, nil
		}
	}
	if len(all) == 1 {
		return all[0], nil
	}
	return Wiki{}, fmt.Errorf("ado: %s has no project wiki and %d code wikis — name one with -wiki: %s",
		c.Project, len(all), strings.Join(names(all), ", "))
}

func names(all []Wiki) []string {
	out := make([]string, 0, len(all))
	for _, w := range all {
		out = append(out, w.Name)
	}
	sort.Strings(out)
	return out
}

// WikiPage is one page's current state.
type WikiPage struct {
	Content string
	// ETag identifies the version just read. Azure DevOps requires it back as
	// If-Match on an update: without it the write is refused, and with a stale
	// one it is rejected rather than silently clobbering somebody's edit.
	ETag   string
	Exists bool
}

// Page reads a wiki page. A page that does not exist yet is not an error — it
// is the normal first run, and Put will create it.
func (c *Client) Page(w Wiki, path string) (WikiPage, error) {
	var body struct {
		Content string `json:"content"`
	}
	resp, err := c.doWith(reqOpts{
		method:   http.MethodGet,
		endpoint: c.wikiURL(w, path, "includeContent=true"),
		out:      &body,
		needs:    WikiPermission,
		allow404: true,
	})
	if err != nil {
		return WikiPage{}, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return WikiPage{}, nil
	}
	return WikiPage{Content: body.Content, ETag: resp.Header.Get("ETag"), Exists: true}, nil
}

// Put creates or replaces a page. Pass the ETag from Page for an update; an
// empty one creates.
//
// A nested page whose parent does not exist is answered with 404 — the API does
// not create intermediate pages the way the web UI does, and a register at
// /tf-snag/Exceptions is nested by design. So a 404 on a create is retried once
// after filling in the ancestors, rather than handed back as "the wiki does not
// exist", which is what it looks like.
func (c *Client) Put(w Wiki, path, content, etag string) (created bool, err error) {
	created = etag == ""
	resp, err := c.putPage(w, path, content, etag)
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound || !created {
		return created, err
	}
	parents := ancestors(path)
	if len(parents) == 0 {
		return created, err // top-level page: the 404 is about the wiki, not the path
	}
	if perr := c.createParents(w, parents); perr != nil {
		// Both, because they mean different things: if the parent write failed
		// the same way, the path was never the problem and the wiki itself is
		// unreachable. Reporting only the original hides that.
		return created, fmt.Errorf("%w (creating parent page %s also failed: %v)", err, parents[len(parents)-1], perr)
	}
	_, err = c.putPage(w, path, content, "")
	return created, err
}

func (c *Client) putPage(w Wiki, path, content, etag string) (*http.Response, error) {
	headers := map[string]string{}
	if etag != "" {
		headers["If-Match"] = etag
	}
	return c.doWith(reqOpts{
		method:      http.MethodPut,
		endpoint:    c.wikiURL(w, path, ""),
		body:        pageBody{Content: content},
		contentType: "application/json",
		headers:     headers,
		needs:       WikiPermission,
	})
	// Not allow404: the caller wants the error to keep, and doWith hands back the
	// response alongside it, which is enough to tell a missing parent from a
	// missing wiki.
}

// createParents fills in missing ancestor pages, outermost first. Existing ones
// are left exactly as they are: somebody may have written a real page there.
func (c *Client) createParents(w Wiki, parents []string) error {
	for _, p := range parents {
		existing, err := c.Page(w, p)
		if err != nil {
			return err
		}
		if existing.Exists {
			continue
		}
		if _, err := c.doWith(reqOpts{
			method:      http.MethodPut,
			endpoint:    c.wikiURL(w, p, ""),
			body:        pageBody{Content: parentStub(p)},
			contentType: "application/json",
			needs:       WikiPermission,
		}); err != nil {
			return err
		}
	}
	return nil
}

// parentStub is deliberately thin. It exists so a child page can, and says so,
// rather than looking like a page somebody started and abandoned.
func parentStub(path string) string {
	name := path[strings.LastIndex(path, "/")+1:]
	return "# " + name + "\n\nCreated by tf-snag so its pages below this one can exist.\n"
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

type pageBody struct {
	Content string `json:"content"`
}

// wikiURL builds a pages endpoint, addressing the wiki by id rather than name —
// a rename should not break a scheduled pipeline. The page path is a query
// parameter rather than part of the route, so "/tf-snag/Exceptions" stays one
// value instead of becoming route segments.
func (c *Client) wikiURL(w Wiki, path, extra string) string {
	q := "path=" + url.QueryEscape(normalisePath(path)) + "&api-version=" + apiVersion
	// A code wiki is a folder on a branch of an ordinary repo, and the branch has
	// to be named or the write lands wherever the service decides is default.
	if b := w.Branch(); b != "" {
		q += "&versionDescriptor.versionType=branch&versionDescriptor.version=" + url.QueryEscape(b)
	}
	if extra != "" {
		q += "&" + extra
	}
	return fmt.Sprintf("%s/%s/_apis/wiki/wikis/%s/pages?%s",
		c.OrgURL, url.PathEscape(c.Project), url.PathEscape(w.ID), q)
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
