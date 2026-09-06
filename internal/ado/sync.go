package ado

import (
	"fmt"
	"html"
	"io"
	"sort"
	"strings"

	"github.com/wes-key/tf-snag/internal/report"
)

// Options configure one work-item pass.
type Options struct {
	Type        string // work item type, e.g. Task
	AreaPath    string // optional
	ClosedState string // state to move a resolved finding to
	Close       bool   // opt in to closing items whose finding has gone
	DryRun      bool   // report what would happen, change nothing
	RunURL      string // this run, linked from the item body
	Context     string // pipeline / branch / run, for the item body
}

// Result is what a pass did, for the caller to report.
type Result struct {
	Created  []Change
	Closed   []Change
	Existing int // findings already tracked, left alone
	Skipped  int // findings not eligible (not new)
}

// Change is one work item created or closed.
type Change struct {
	ID    int
	URL   string
	Title string
}

// Sync raises work items for eligible findings and, when Options.Close is set,
// closes items whose finding is no longer reported. It also stamps WorkItem /
// WorkItemURL onto the report so every downstream format can show the reference
// beside the finding.
//
// eligible decides which findings warrant an item — new-only, everything, and
// so on — and is applied to creation only. An item that already exists is always
// linked back, whatever the finding's state, so a long-standing finding keeps
// pointing at the item raised for it on day one.
func (c *Client) Sync(r *report.Report, opts Options, eligible func(FindingKind, string) bool, w io.Writer) (Result, error) {
	var res Result

	existing, err := c.Existing()
	if err != nil {
		return res, err
	}

	// Pass 1: link and create. Suppressed findings are skipped entirely — an
	// ignored finding is one someone has already decided not to act on, so
	// raising work for it would be perverse.
	seen := map[string]bool{}
	for i := range r.Drift {
		if r.Drift[i].Suppressed {
			continue
		}
		f := finding{
			kind:  KindDrift,
			id:    r.Drift[i].FindingID(),
			title: driftTitle(r.Drift[i]),
			body:  driftBody(r.Drift[i], opts),
			tags:  []string{"drift"},
		}
		seen[f.id] = true
		item, err := c.linkOrCreate(f, existing, opts, eligible, &res, w)
		if err != nil {
			return res, err
		}
		r.Drift[i].WorkItem, r.Drift[i].WorkItemURL = item.ID, item.URL
	}
	for i := range r.Deprecations {
		if r.Deprecations[i].Suppressed {
			continue
		}
		f := finding{
			kind:  KindDeprecation,
			id:    r.Deprecations[i].FindingID(),
			title: deprTitle(r.Deprecations[i]),
			body:  deprBody(r.Deprecations[i], opts),
			tags:  []string{"deprecation"},
		}
		seen[f.id] = true
		item, err := c.linkOrCreate(f, existing, opts, eligible, &res, w)
		if err != nil {
			return res, err
		}
		r.Deprecations[i].WorkItem, r.Deprecations[i].WorkItemURL = item.ID, item.URL
	}

	if !opts.Close {
		return res, nil
	}

	// Pass 2: close items whose finding is gone. Iterated in id order so a run's
	// output is stable and diffable.
	gone := make([]string, 0, len(existing))
	for id := range existing {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	sort.Slice(gone, func(i, j int) bool { return existing[gone[i]].ID < existing[gone[j]].ID })

	for _, id := range gone {
		item := existing[id]
		if strings.EqualFold(item.State, opts.ClosedState) {
			continue // already closed on a previous run
		}
		if opts.DryRun {
			fmt.Fprintf(w, "  would close #%d (%s) — no longer reported\n", item.ID, item.Title)
			res.Closed = append(res.Closed, Change{ID: item.ID, URL: item.URL, Title: item.Title})
			continue
		}
		changed, err := c.Close(item, opts.ClosedState,
			"Closed by tf-snag: this finding is no longer reported."+runSuffix(opts))
		if err != nil {
			return res, err
		}
		if changed {
			res.Closed = append(res.Closed, Change{ID: item.ID, URL: item.URL, Title: item.Title})
		}
	}
	return res, nil
}

// FindingKind distinguishes what a finding is, so a caller's eligibility rule
// can treat drift and deprecations differently.
type FindingKind string

const (
	KindDrift       FindingKind = "drift"
	KindDeprecation FindingKind = "deprecation"
)

type finding struct {
	kind  FindingKind
	id    string
	title string
	body  string
	tags  []string
}

// linkOrCreate returns the item tracking f, raising one if it is missing and the
// finding is eligible. A zero WorkItem means nothing tracks it and nothing was
// raised.
func (c *Client) linkOrCreate(f finding, existing map[string]WorkItem, opts Options,
	eligible func(FindingKind, string) bool, res *Result, w io.Writer) (WorkItem, error) {

	if item, ok := existing[f.id]; ok {
		res.Existing++
		return item, nil
	}
	if eligible != nil && !eligible(f.kind, f.id) {
		res.Skipped++
		return WorkItem{}, nil
	}
	if opts.DryRun {
		fmt.Fprintf(w, "  would create %s: %s\n", opts.Type, f.title)
		res.Created = append(res.Created, Change{Title: f.title})
		return WorkItem{}, nil
	}

	item, err := c.Create(NewItem{
		FindingID:   f.id,
		Type:        opts.Type,
		Title:       f.title,
		Description: f.body,
		Tags:        f.tags,
		AreaPath:    opts.AreaPath,
	})
	if err != nil {
		return WorkItem{}, err
	}
	res.Created = append(res.Created, Change{ID: item.ID, URL: item.URL, Title: item.Title})
	return item, nil
}

// --- item content ------------------------------------------------------------

// titleMax keeps a title inside the 255 characters Azure DevOps allows, with
// room for the prefix.
const titleMax = 200

func driftTitle(rr report.ResourceReport) string {
	return clip(fmt.Sprintf("Terraform drift: %s changed outside Terraform", rr.Address), titleMax)
}

func deprTitle(d report.Deprecation) string {
	return clip("Terraform deprecation: "+d.Summary, titleMax)
}

// driftBody is the work item description. HTML, because that is what the
// System.Description field renders.
func driftBody(rr report.ResourceReport, opts Options) string {
	var b strings.Builder
	b.WriteString("<p>tf-snag found this resource changed outside Terraform.</p>")
	fmt.Fprintf(&b, "<p><b>Resource:</b> <code>%s</code><br/>", esc(rr.Address))
	if rr.Module != "" {
		fmt.Fprintf(&b, "<b>Module:</b> <code>%s</code><br/>", esc(rr.Module))
	}
	if rr.File != "" {
		fmt.Fprintf(&b, "<b>Declared in:</b> <code>%s</code><br/>", esc(location(rr.File, rr.Line)))
	}
	fmt.Fprintf(&b, "<b>Change:</b> %s</p>", esc(rr.Action))

	if len(rr.Attrs) > 0 {
		b.WriteString("<table><tr><th>Attribute</th><th>From</th><th>To</th></tr>")
		for _, a := range rr.Attrs {
			fmt.Fprintf(&b, "<tr><td><code>%s</code></td><td><code>%s</code></td><td><code>%s</code></td></tr>",
				esc(a.Path), esc(clip(render(a.Old), 200)), esc(clip(render(a.New), 200)))
		}
		b.WriteString("</table>")
	}
	b.WriteString(footer(opts))
	return b.String()
}

func deprBody(d report.Deprecation, opts Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<p>%s</p>", esc(d.Summary))
	if d.Detail != "" {
		fmt.Fprintf(&b, "<p>%s</p>", esc(d.Detail))
	}
	if len(d.Sites) > 0 {
		b.WriteString("<p><b>Reported at:</b></p><ul>")
		for _, s := range d.Sites {
			label := s.Address
			if loc := location(s.File, s.Line); loc != "" {
				if label == "" {
					label = loc
				} else {
					label += " (" + loc + ")"
				}
			}
			fmt.Fprintf(&b, "<li><code>%s</code></li>", esc(label))
		}
		b.WriteString("</ul>")
	}
	b.WriteString(footer(opts))
	return b.String()
}

// footer records where the item came from, so someone triaging it a month later
// can find the run without asking.
func footer(opts Options) string {
	var bits []string
	if opts.Context != "" {
		bits = append(bits, esc(opts.Context))
	}
	if opts.RunURL != "" {
		bits = append(bits, fmt.Sprintf(`<a href="%s">view the run</a>`, esc(opts.RunURL)))
	}
	if len(bits) == 0 {
		return "<p><i>Raised automatically by tf-snag.</i></p>"
	}
	return "<p><i>Raised automatically by tf-snag — " + strings.Join(bits, " · ") + "</i></p>"
}

func runSuffix(opts Options) string {
	if opts.RunURL == "" {
		return ""
	}
	return " " + opts.RunURL
}

func location(file string, line int) string {
	switch {
	case file == "":
		return ""
	case line > 0:
		return fmt.Sprintf("%s:%d", file, line)
	default:
		return file
	}
}

func esc(s string) string { return html.EscapeString(s) }

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// render formats an attribute value the way the other reports do.
func render(v any) string {
	if v == nil {
		return "null"
	}
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%v", v)
}
