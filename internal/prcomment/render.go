// Package prcomment renders a run's findings as a pull request comment and keeps
// one thread per pull request up to date with them.
//
// The point is where a reviewer reads. The tf-snag tab is complete and themed,
// but it is a build tab: approving a change does not take anybody past it, so a
// retirement the pull request introduces gets approved unread. This puts the
// same findings in the conversation the reviewer is already in.
package prcomment

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/wes-key/tf-snag/internal/report"
)

// Marker identifies tf-snag's own thread on a pull request. It is an HTML
// comment, so it is invisible in the rendered comment but survives a round trip
// through the API - which is what lets a later run find the thread it wrote
// rather than posting a second one.
const Marker = "<!-- tf-snag:pr-comment -->"

// maxPerSection caps each list. A pull request that drifts the whole estate
// would otherwise post a comment nobody scrolls, and the tab carries the full
// report either way.
const maxPerSection = 10

// maxDetail caps the lines under one finding - attribute changes, deprecation
// sites, retired resources.
const maxDetail = 3

// Options carry the context the report itself does not know about.
type Options struct {
	// Context is the line under the heading, e.g. the pipeline and branch.
	Context string
	// RunURL is the pipeline run, linked from the footer so a reader can reach
	// the full tf-snag tab.
	RunURL string
}

// Render builds the comment body: a verdict, the counts, then each kind of
// finding. Ignored findings are counted, never listed - somebody has already
// decided not to act on them, and a reviewer reading a pull request has not
// asked to review that decision.
func Render(rep *report.Report, opts Options) string {
	drift := unsuppressedDrift(rep)
	deprecations := unsuppressedDeprecations(rep)
	retirements := unsuppressedRetirements(rep)

	var b strings.Builder
	b.WriteString(Marker)
	fmt.Fprintf(&b, "\n## %s\n", verdict(drift, deprecations, retirements))
	if opts.Context != "" {
		fmt.Fprintf(&b, "\n%s\n", opts.Context)
	}
	if n := countIgnored(rep); n > 0 {
		fmt.Fprintf(&b, "\n%s suppressed by an ignore rule, not listed below.\n",
			plural(n, "finding", "findings"))
	}

	writeDrift(&b, drift)
	writeDeprecations(&b, deprecations)
	writeRetirements(&b, retirements)

	b.WriteString("\n---\n")
	if opts.RunURL != "" {
		fmt.Fprintf(&b, "\nFull report on the **tf-snag** tab of [this run](%s).\n", opts.RunURL)
	} else {
		b.WriteString("\nFull report on the **tf-snag** tab of this run.\n")
	}
	return b.String()
}

// verdict leads with what the reviewer has to decide about.
func verdict(drift []report.ResourceReport, deprecations []report.Deprecation, retirements []report.Retirement) string {
	var parts []string
	if n := len(drift); n > 0 {
		parts = append(parts, plural(n, "resource changed outside Terraform", "resources changed outside Terraform"))
	}
	if n := len(deprecations); n > 0 {
		parts = append(parts, plural(n, "deprecation", "deprecations"))
	}
	if n := len(retirements); n > 0 {
		parts = append(parts, plural(n, "retirement", "retirements"))
	}
	if len(parts) == 0 {
		return "tf-snag: nothing to flag"
	}
	return "tf-snag: " + strings.Join(parts, ", ")
}

func writeDrift(b *strings.Builder, items []report.ResourceReport) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### Changed outside Terraform (%d)\n\n", len(items))
	for i, d := range items {
		if i == maxPerSection {
			fmt.Fprintf(b, "\n…and %d more.\n", len(items)-maxPerSection)
			return
		}
		fmt.Fprintf(b, "- **`%s`** — %s%s\n", d.Address, strings.ToLower(d.Action),
			badges(d.BaselineState, d.Unsuppressed, d.FirstSeen))
		for j, a := range d.Attrs {
			if j == maxDetail {
				fmt.Fprintf(b, "  - …and %s\n", plural(len(d.Attrs)-maxDetail, "more attribute", "more attributes"))
				break
			}
			fmt.Fprintf(b, "  - `%s`: `%s` → `%s`\n", a.Path, short(value(a.Old)), short(value(a.New)))
		}
	}
}

func writeDeprecations(b *strings.Builder, items []report.Deprecation) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### Deprecations (%d)\n\n", len(items))
	for i, d := range items {
		if i == maxPerSection {
			fmt.Fprintf(b, "\n…and %d more.\n", len(items)-maxPerSection)
			return
		}
		fmt.Fprintf(b, "- **%s**%s\n", d.Summary, badges(d.BaselineState, d.Unsuppressed, d.FirstSeen))
		if d.Detail != "" {
			fmt.Fprintf(b, "  - %s\n", oneLine(d.Detail))
		}
		for j, s := range d.Sites {
			if j == maxDetail {
				fmt.Fprintf(b, "  - …and %s\n", plural(len(d.Sites)-maxDetail, "more location", "more locations"))
				break
			}
			fmt.Fprintf(b, "  - `%s`%s\n", s.Address, location(s))
		}
	}
}

func writeRetirements(b *strings.Builder, items []report.Retirement) {
	if len(items) == 0 {
		return
	}
	// Most urgent first: a retirement whose date has passed is a different
	// conversation from one three years out.
	sorted := append([]report.Retirement(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Days < sorted[j].Days })

	fmt.Fprintf(b, "\n### Retirements (%d)\n\n", len(sorted))
	for i, r := range sorted {
		if i == maxPerSection {
			fmt.Fprintf(b, "\n…and %d more.\n", len(sorted)-maxPerSection)
			return
		}
		title := r.Title
		if r.URL != "" {
			title = fmt.Sprintf("[%s](%s)", r.Title, r.URL)
		}
		fmt.Fprintf(b, "- **%s** — %s%s\n", title, deadline(r), badges(r.BaselineState, r.Unsuppressed, ""))
		for j, in := range r.Instances {
			if j == maxDetail {
				fmt.Fprintf(b, "  - …and %s\n", plural(len(r.Instances)-maxDetail, "more resource", "more resources"))
				break
			}
			fmt.Fprintf(b, "  - `%s`\n", in.Address)
		}
	}
}

// deadline says how long is left in words a reader can act on, and marks the
// ones -retirements-fail-within excludes from the gate, so a warning is not
// mistaken for a blocker.
func deadline(r report.Retirement) string {
	var when string
	switch {
	case r.Days < 0:
		when = fmt.Sprintf("**retired %s ago** (%s)", span(-r.Days), r.RetiresOn)
	case r.Days == 0:
		when = fmt.Sprintf("**retires today** (%s)", r.RetiresOn)
	default:
		when = fmt.Sprintf("retires in %s (%s)", span(r.Days), r.RetiresOn)
	}
	if r.BeyondFailWindow {
		when += ", outside the fail window"
	}
	return when
}

// span puts a day count in the largest unit that still reads precisely: "14
// days" is useful, "412 days" is not.
func span(days int) string {
	switch {
	case days < 60:
		return plural(days, "day", "days")
	case days < 730:
		return plural(days/30, "month", "months")
	default:
		return plural(days/365, "year", "years")
	}
}

// badges mark what a reviewer most needs: a finding this change introduces, or
// one that has just come out from under an ignore rule.
func badges(state string, unsuppressed bool, firstSeen string) string {
	switch {
	case unsuppressed:
		return " — **no longer ignored**"
	case state == "new":
		return " — **new**"
	}
	if d, ok := dateOnly(firstSeen); ok {
		return " — first seen " + d
	}
	return ""
}

// dateOnly trims an RFC 3339 timestamp to its date. A reviewer cares which day a
// finding appeared, never which second.
func dateOnly(ts string) (string, bool) {
	if len(ts) >= 10 && ts[4] == '-' && ts[7] == '-' {
		return ts[:10], true
	}
	return "", false
}

func location(s report.DeprecationSite) string {
	switch {
	case s.File == "":
		return ""
	case s.Line > 0:
		return fmt.Sprintf(" (`%s:%d`)", s.File, s.Line)
	default:
		return fmt.Sprintf(" (`%s`)", s.File)
	}
}

// short keeps a long attribute value from turning one finding into a wall of
// JSON. The tab and the report carry the whole thing.
func short(v string) string {
	v = oneLine(v)
	const max = 60
	if len([]rune(v)) <= max {
		return v
	}
	return string([]rune(v)[:max]) + "…"
}

// value renders an attribute value the way the rest of the report does: JSON, so
// a string is quoted and a null is visible rather than blank.
func value(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// --- selection ---------------------------------------------------------------

func unsuppressedDrift(rep *report.Report) []report.ResourceReport {
	var out []report.ResourceReport
	for _, d := range rep.Drift {
		if !d.Suppressed {
			out = append(out, d)
		}
	}
	return out
}

func unsuppressedDeprecations(rep *report.Report) []report.Deprecation {
	var out []report.Deprecation
	for _, d := range rep.Deprecations {
		if !d.Suppressed {
			out = append(out, d)
		}
	}
	return out
}

func unsuppressedRetirements(rep *report.Report) []report.Retirement {
	var out []report.Retirement
	for _, r := range rep.Retirements {
		if !r.Suppressed {
			out = append(out, r)
		}
	}
	return out
}

func countIgnored(rep *report.Report) int {
	n := 0
	for _, d := range rep.Drift {
		if d.Suppressed {
			n++
		}
	}
	for _, d := range rep.Deprecations {
		if d.Suppressed {
			n++
		}
	}
	for _, r := range rep.Retirements {
		if r.Suppressed {
			n++
		}
	}
	return n
}
