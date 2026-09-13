package wiki

import (
	"fmt"
	"io"
	"strings"

	"github.com/wes-key/tf-snag/internal/ado"
)

// Publisher is the subset of *ado.Client this package needs, so the publish
// logic can be tested without an Azure DevOps instance.
type Publisher interface {
	Page(wiki, path string) (Page, error)
	Put(wiki, path, content, etag string) (created bool, err error)
}

// Page mirrors ado.WikiPage. Declared here so the interface above does not drag
// the ado package into every caller.
type Page struct {
	Content string
	ETag    string
	Exists  bool
}

// Client adapts *ado.Client to Publisher. The indirection is what lets Publish
// be tested against a fake instead of an Azure DevOps instance.
type Client struct{ *ado.Client }

func (c Client) Page(wikiID, path string) (Page, error) {
	p, err := c.Client.Page(wikiID, path)
	return Page{Content: p.Content, ETag: p.ETag, Exists: p.Exists}, err
}

// Result says what happened, for the caller's log.
type Result struct {
	Created   bool
	Updated   bool
	Unchanged bool
}

// Publish writes content to the page unless it is already exactly that.
//
// The comparison is the point. The register is regenerated every run, and a
// scheduled check that rewrites an identical page adds a wiki revision a day —
// which buries the revisions that mean something. The page has to be read for
// its ETag regardless, so the check is free.
//
// dryRun reports what would happen and writes nothing.
func Publish(p Publisher, wikiID, path, content string, dryRun bool, log io.Writer) (Result, error) {
	cur, err := p.Page(wikiID, path)
	if err != nil {
		return Result{}, fmt.Errorf("reading %s%s: %w", wikiID, path, err)
	}

	if cur.Exists && sameContent(cur.Content, content) {
		fmt.Fprintf(log, "wiki: %s%s is already up to date\n", wikiID, path)
		return Result{Unchanged: true}, nil
	}

	verb := "updating"
	if !cur.Exists {
		verb = "creating"
	}
	if dryRun {
		fmt.Fprintf(log, "wiki: would be %s %s%s (%d bytes)\n", verb, wikiID, path, len(content))
		return Result{Created: !cur.Exists, Updated: cur.Exists}, nil
	}

	fmt.Fprintf(log, "wiki: %s %s%s\n", verb, wikiID, path)
	if _, err := p.Put(wikiID, path, content, cur.ETag); err != nil {
		return Result{}, fmt.Errorf("writing %s%s: %w", wikiID, path, err)
	}
	return Result{Created: !cur.Exists, Updated: cur.Exists}, nil
}

// sameContent ignores the generated-at footer, which changes every run and
// would otherwise make every page differ from the last one.
func sameContent(a, b string) bool {
	return strings.TrimSpace(withoutFooter(a)) == strings.TrimSpace(withoutFooter(b))
}

func withoutFooter(s string) string {
	if i := strings.LastIndex(s, "\n---\n"); i >= 0 {
		return s[:i]
	}
	return s
}
