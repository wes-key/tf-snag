package ado

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// WikiPermission is what a 403 on these calls is asking for. Work items and the
// wiki are separate scopes, so a token that raises items happily can still be
// refused here — worth saying which one is missing.
const WikiPermission = "Wiki (Read & Write)"

// DefaultWiki is the project wiki's identifier. Azure DevOps names it after the
// project, and it is what `-wiki` falls back to.
func DefaultWiki(project string) string { return project + ".wiki" }

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
func (c *Client) Page(wiki, path string) (WikiPage, error) {
	var body struct {
		Content string `json:"content"`
	}
	resp, err := c.doWith(reqOpts{
		method:   http.MethodGet,
		endpoint: c.wikiURL(wiki, path, "includeContent=true"),
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
// empty one creates. Reports whether the page was created rather than updated,
// which is the only part of the outcome worth logging differently.
func (c *Client) Put(wiki, path, content, etag string) (created bool, err error) {
	headers := map[string]string{}
	if etag != "" {
		headers["If-Match"] = etag
	}
	_, err = c.doWith(reqOpts{
		method:      http.MethodPut,
		endpoint:    c.wikiURL(wiki, path, ""),
		body:        pageBody{Content: content},
		contentType: "application/json",
		headers:     headers,
		needs:       WikiPermission,
	})
	return etag == "", err
}

type pageBody struct {
	Content string `json:"content"`
}

// wikiURL builds a pages endpoint. The page path is a query parameter rather
// than part of the route, so "/tf-snag/Exceptions" stays one value instead of
// becoming route segments.
func (c *Client) wikiURL(wiki, path, extra string) string {
	q := "path=" + url.QueryEscape(normalisePath(path)) + "&api-version=" + apiVersion
	if extra != "" {
		q += "&" + extra
	}
	return fmt.Sprintf("%s/%s/_apis/wiki/wikis/%s/pages?%s",
		c.OrgURL, url.PathEscape(c.Project), url.PathEscape(wiki), q)
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
