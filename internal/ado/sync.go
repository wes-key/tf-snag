package ado

import (
	"fmt"
	"html"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/wes-key/tf-snag/internal/plan"
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
	// Suppressed findings are tracked separately so pass 2 can tell "the drift
	// went away" from "someone added an ignore rule" — both stop the finding
	// being reported, but they are not the same news for whoever reads the item.
	ignored := map[string]bool{}
	for i := range r.Drift {
		if r.Drift[i].Suppressed {
			ignored[r.Drift[i].FindingID()] = true
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
			ignored[r.Deprecations[i].FindingID()] = true
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
		why, note := "no longer reported", "Closed by tf-snag: this finding is no longer reported."
		if ignored[id] {
			why = "now covered by an ignore rule"
			note = "Closed by tf-snag: this finding is now covered by an ignore rule, so it is no longer tracked here. " +
				"It is still detected — remove the rule to start tracking it again."
		}
		if opts.DryRun {
			fmt.Fprintf(w, "  would close #%d (%s) — %s\n", item.ID, item.Title, why)
			res.Closed = append(res.Closed, Change{ID: item.ID, URL: item.URL, Title: item.Title})
			continue
		}
		changed, err := c.Close(item, opts.ClosedState, note+runSuffix(opts))
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

	// A closed item does not track a live finding. This is the unignore case:
	// adding an ignore rule closed the item, and removing the rule has to open a
	// fresh one rather than link to the closed one and leave live drift with
	// nothing actionable against it. The old item stays as the record of that
	// period. Only the state tf-snag itself closes with counts — an item someone
	// closed by hand into another state is treated as theirs to manage.
	if item, ok := existing[f.id]; ok && !strings.EqualFold(item.State, opts.ClosedState) {
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

// The work item description is HTML with inline styles — Azure DevOps keeps
// those, unlike Teams, so the run tab's design carries over almost intact:
// coloured badges, a ruled header, a red/green attribute diff.
//
// Two constraints shape the choices below. There is no stylesheet to hang
// classes off, so every rule is inline. And the description renders under
// whichever theme the reader has, so tinted panel backgrounds are avoided —
// solid-filled badges with white text read correctly on light and dark alike,
// which is the same reason they are the one element worth filling.
const (
	colDelete = "#c50f1f" // delete, replace
	colUpdate = "#ca5010" // update
	colCreate = "#107c10" // create
	colNew    = "#2a5bd7" // new since the baseline
	colMuted  = "#767676" // secondary text, readable either theme
	colRule   = "#c8c8c8" // table and divider lines
)

// badge is the run tab's pill: solid fill, white text, rounded. The one thing
// Teams could not render and Azure DevOps can.
func badge(text, fill string) string {
	return fmt.Sprintf(
		`<span style="background:%s;color:#ffffff;padding:2px 9px;border-radius:10px;`+
			`font-size:11px;font-weight:600;white-space:nowrap">%s</span>`, fill, esc(text))
}

// mono renders an address or value the way the tab's <code> does.
func mono(s string) string {
	return fmt.Sprintf(`<span style="font-family:Consolas,Menlo,monospace;font-size:12px">%s</span>`, esc(s))
}

// muted is a secondary line — a module path, a source location.
func muted(s string) string {
	return fmt.Sprintf(`<span style="color:%s;font-size:12px">%s</span>`, colMuted, esc(s))
}

// header is the coloured rule and title every item opens with, mirroring the
// tab's tinted banner without relying on a background that has to survive both
// themes.
func header(glyph, title, fill, sub string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<div style="border-left:4px solid %s;padding:2px 0 2px 10px;margin:0 0 12px 0">`, fill)
	fmt.Fprintf(&b, `<span style="color:%s;font-weight:700;font-size:15px">%s</span> `, fill, esc(glyph))
	fmt.Fprintf(&b, `<span style="font-weight:700;font-size:15px">%s</span>`, esc(title))
	if sub != "" {
		fmt.Fprintf(&b, `<br/>%s`, muted(sub))
	}
	b.WriteString(`</div>`)
	return b.String()
}

func driftColour(action string) string {
	switch action {
	case "create":
		return colCreate
	case "delete", "replace":
		return colDelete
	default:
		return colUpdate
	}
}

func driftGlyph(action string) string {
	switch action {
	case "create":
		return "+"
	case "delete":
		return "−"
	case "replace":
		return "±"
	default:
		return "~"
	}
}

// driftBody is the work item description for a drifted resource.
func driftBody(rr report.ResourceReport, opts Options) string {
	fill := driftColour(rr.Action)
	var b strings.Builder

	b.WriteString(header(driftGlyph(rr.Action), rr.Address, fill, whereLine(rr)))

	// Badge row: what changed, and whether it is new — the tab's Change and
	// First seen columns.
	b.WriteString(`<div style="margin:0 0 12px 0">`)
	b.WriteString(badge(titleCase(rr.Action), fill))
	b.WriteString(provenanceBadge(rr.BaselineState, rr.Unsuppressed, rr.FirstSeen))
	b.WriteString(`</div>`)

	if len(rr.Attrs) > 0 {
		b.WriteString(attrTable(rr.Attrs))
	} else if s := rr.SummaryLine(); s != "" {
		fmt.Fprintf(&b, `<p>%s</p>`, muted(s))
	}

	b.WriteString(footer(opts))
	return b.String()
}

// attrTable is the tab's Attribute / From / To diff, old in red and new in
// green.
func attrTable(attrs []plan.AttrDiff) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<table style="border-collapse:collapse;width:100%%;font-size:12px">`)
	fmt.Fprintf(&b, `<tr>`+
		`<th style="text-align:left;padding:4px 10px;border-bottom:1px solid %[1]s;color:%[2]s">Attribute</th>`+
		`<th style="text-align:left;padding:4px 10px;border-bottom:1px solid %[1]s;color:%[2]s">From</th>`+
		`<th style="text-align:left;padding:4px 10px;border-bottom:1px solid %[1]s;color:%[2]s">To</th></tr>`,
		colRule, colMuted)
	for _, a := range attrs {
		fmt.Fprintf(&b,
			`<tr>`+
				`<td style="padding:4px 10px;border-bottom:1px solid %[1]s">%[2]s</td>`+
				`<td style="padding:4px 10px;border-bottom:1px solid %[1]s;color:%[3]s">%[4]s</td>`+
				`<td style="padding:4px 10px;border-bottom:1px solid %[1]s;color:%[5]s">%[6]s</td></tr>`,
			colRule, mono(a.Path),
			colDelete, mono(clip(render(a.Old), 200)),
			colCreate, mono(clip(render(a.New), 200)))
	}
	b.WriteString(`</table>`)
	return b.String()
}

func deprBody(d report.Deprecation, opts Options) string {
	fill := colUpdate
	glyph := "⚠"
	if strings.EqualFold(d.Severity, "error") {
		fill, glyph = colDelete, "✖"
	}

	var b strings.Builder
	b.WriteString(header(glyph, d.Summary, fill, ""))

	b.WriteString(`<div style="margin:0 0 12px 0">`)
	b.WriteString(badge(titleCase(orDefault(d.Severity, "warning")), fill))
	b.WriteString(provenanceBadge(d.BaselineState, d.Unsuppressed, d.FirstSeen))
	b.WriteString(`</div>`)

	if d.Detail != "" {
		fmt.Fprintf(&b, `<p>%s</p>`, esc(d.Detail))
	}
	if len(d.Sites) > 0 {
		fmt.Fprintf(&b, `<p style="margin-bottom:4px;color:%s;font-size:12px">Reported at</p><ul style="margin-top:0">`, colMuted)
		for _, s := range d.Sites {
			line := mono(orDefault(s.Address, location(s.File, s.Line)))
			if s.Address != "" {
				if loc := location(s.File, s.Line); loc != "" {
					line += "&nbsp;&nbsp;" + muted(loc)
				}
			}
			fmt.Fprintf(&b, `<li>%s</li>`, line)
		}
		b.WriteString(`</ul>`)
	}

	b.WriteString(footer(opts))
	return b.String()
}

// whereLine locates the resource under the title. The tab shows the module or
// the file, whichever it has; a work item shows both when it can, because it is
// read on its own weeks later and the file path is where the reader goes to fix
// the thing.
func whereLine(rr report.ResourceReport) string {
	parts := make([]string, 0, 2)
	if rr.Module != "" {
		parts = append(parts, rr.Module)
	}
	if loc := location(rr.File, rr.Line); loc != "" {
		parts = append(parts, loc)
	}
	return strings.Join(parts, "  ·  ")
}

// provenanceBadge says why this item exists now. An unignored finding gets its
// own badge and keeps its real age beside it: the item is new, the finding is
// not, and conflating the two would misreport how long the drift has been there.
func provenanceBadge(state string, unsuppressed bool, ts string) string {
	age := firstSeen(ts)
	switch {
	case unsuppressed:
		s := "&nbsp;" + badge("No longer ignored", colNew)
		if age != "" {
			s += "&nbsp;" + muted(age)
		}
		return s
	case state == "new":
		return "&nbsp;" + badge("New", colNew)
	case age != "":
		return "&nbsp;" + muted(age)
	}
	return ""
}

// firstSeen phrases a carried-over finding's age. Empty without a -baseline.
func firstSeen(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	switch days := int(time.Since(t).Hours() / 24); {
	case days <= 0:
		return "first detected today"
	case days == 1:
		return "first detected yesterday"
	default:
		return fmt.Sprintf("first detected %s, %d days ago", t.Format("2006-01-02"), days)
	}
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
	tail := "Raised automatically by tf-snag"
	if len(bits) > 0 {
		tail += " — " + strings.Join(bits, " · ")
	}
	// The rule separates the provenance from the finding, as the tab separates
	// its sections.
	return fmt.Sprintf(
		`<p style="margin-top:16px;padding-top:8px;border-top:1px solid %s;color:%s;font-size:11px">%s</p>`,
		colRule, colMuted, tail)
}

func titleCase(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
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
