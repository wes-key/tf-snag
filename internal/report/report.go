// Package report turns a parsed plan into tf-snag's summary: resources changed
// outside Terraform (drift) and, secondarily, the pending changes a plan would
// apply from configuration.
package report

import (
	"crypto/sha1"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wes-key/tf-snag/internal/plan"
	"github.com/wes-key/tf-snag/internal/retire"
)

// ReportSchema is the version of the JSON emitted by WriteJSON. Consumers (the
// tf-snag Azure DevOps extension) key off it and should refuse a version they do
// not recognise. Bump it on any breaking change to the JSON shape.
//
// v2: `deprecations` is now carried (schema 1 dropped it); every drift and
// deprecation may carry `suppressed`/`suppress_*` (ignore rules) and, when
// `-baseline` was given, `baseline_state` + `first_seen` (provenance).
//
// v3: `retirements` and `retirement_catalogue`, from `-check retirements`.
const ReportSchema = 3

type Report struct {
	Schema           int              `json:"schema"`
	TerraformVersion string           `json:"terraform_version"`
	Drift            []ResourceReport `json:"drift"`
	Pending          []ResourceReport `json:"pending"`
	// Deprecations is populated only when `-check` asks for it.
	Deprecations []Deprecation `json:"deprecations,omitempty"`
	// Retirements, and the catalogue they came from, are populated only when
	// `-check retirements` asks for them — see retirement.go.
	Retirements []Retirement   `json:"retirements,omitempty"`
	Catalogue   *CatalogueInfo `json:"retirement_catalogue,omitempty"`

	// RetirementGateDays narrows which retirements trip -exit-code
	// (-retirements-fail-within). 0 gates on every un-suppressed one.
	RetirementGateDays int `json:"-"`
}

// Deprecation is one deprecation notice from a `terraform plan -json` log,
// collapsed across every source location that trips it.
type Deprecation struct {
	Severity string            `json:"severity"`
	Summary  string            `json:"summary"`
	Detail   string            `json:"detail,omitempty"`
	Sites    []DeprecationSite `json:"sites,omitempty"`

	Suppressed     bool   `json:"suppressed,omitempty"`
	SuppressReason string `json:"suppress_reason,omitempty"`
	SuppressSrc    string `json:"suppress_source,omitempty"` // where the rule came from
	SuppressKind   string `json:"-"`                         // "inSource" | "external" — SARIF only

	// Set by PriorResults.StampReport when -baseline was given.
	BaselineState string `json:"baseline_state,omitempty"` // "new" | "updated"
	FirstSeen     string `json:"first_seen,omitempty"`     // RFC3339, first detection time
	FirstRunURL   string `json:"first_run_url,omitempty"`  // the CI run that first surfaced it
	// Unsuppressed marks a finding an ignore rule used to cover and no longer
	// does. It is deliberately not the same as BaselineState "new": the finding
	// was detected long before, and FirstSeen still says when. What changed is
	// that it became actionable, which is what the notify and work item gates
	// care about.
	Unsuppressed bool `json:"unsuppressed,omitempty"`

	// Set by the work-item pass when -ado-url is given.
	WorkItem    int    `json:"work_item,omitempty"` // ADO work item tracking this finding
	WorkItemURL string `json:"work_item_url,omitempty"`
}

// DeprecationSite is one place a deprecation fires — a resource address and,
// when the plan log carried a range, the .tf file and line.
type DeprecationSite struct {
	Address string `json:"address,omitempty"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
}

// key collapses the instances Terraform emits for one source location (a
// count/for_each block warns once per instance, all at the same range).
func (s DeprecationSite) key() string {
	if s.File != "" {
		return strings.ToLower(s.File) + "\x00" + strconv.Itoa(s.Line)
	}
	return "\x00\x00" + strings.ToLower(s.Address)
}

// less orders sites: located ones first, then by file, line, address.
func (s DeprecationSite) less(o DeprecationSite) bool {
	if (s.File == "") != (o.File == "") {
		return s.File != ""
	}
	if s.File != o.File {
		return s.File < o.File
	}
	if s.Line != o.Line {
		return s.Line < o.Line
	}
	return s.Address < o.Address
}

func (d Deprecation) firstSite() DeprecationSite {
	if len(d.Sites) == 0 {
		return DeprecationSite{}
	}
	return d.Sites[0]
}

type ResourceReport struct {
	Address  string          `json:"address"`
	Type     string          `json:"type,omitempty"`
	Module   string          `json:"module,omitempty"`
	Action   string          `json:"action"`
	Identity string          `json:"identity,omitempty"`
	Attrs    []plan.AttrDiff `json:"attributes,omitempty"`

	Suppressed     bool   `json:"suppressed,omitempty"`
	SuppressReason string `json:"suppress_reason,omitempty"`
	SuppressSrc    string `json:"suppress_source,omitempty"` // where the rule came from
	SuppressKind   string `json:"-"`                         // "inSource" | "external" — SARIF only

	// Set by AttachSourceLocations when -source was given: the .tf that declares
	// this resource (repo-relative, forward slashes) so consumers can link to it.
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`

	// Set by PriorResults.StampReport when -baseline was given.
	BaselineState string `json:"baseline_state,omitempty"` // "new" | "updated"
	FirstSeen     string `json:"first_seen,omitempty"`     // RFC3339, first detection time
	FirstRunURL   string `json:"first_run_url,omitempty"`  // the CI run that first surfaced it
	// Unsuppressed marks a finding an ignore rule used to cover and no longer
	// does. It is deliberately not the same as BaselineState "new": the finding
	// was detected long before, and FirstSeen still says when. What changed is
	// that it became actionable, which is what the notify and work item gates
	// care about.
	Unsuppressed bool `json:"unsuppressed,omitempty"`

	// Set by the work-item pass when -ado-url is given.
	WorkItem    int    `json:"work_item,omitempty"` // ADO work item tracking this finding
	WorkItemURL string `json:"work_item_url,omitempty"`
}

// AttachSourceLocations fills File/Line on each drift resource from src (keyed
// "<type>.<name>", the map produced by the -source index in main.go) — the same
// lookup WriteSARIF does for physicalLocation, so `-format json` can link a
// finding to the .tf that declares it. A nil src is a no-op.
func (r *Report) AttachSourceLocations(src map[string]SourceLoc) {
	if src == nil {
		return
	}
	for i := range r.Drift {
		if s, ok := src[r.Drift[i].Type+"."+resourceName(r.Drift[i].Address)]; ok {
			r.Drift[i].File, r.Drift[i].Line = s.File, s.Line
		}
	}
}

// SourceLoc is where a resource is declared in the Terraform source, used to
// give SARIF results a real, clickable location. Populated by the caller (it
// needs the .tf files, which the plan JSON does not carry).
type SourceLoc struct {
	File string // repo-relative, forward slashes
	Line int
}

// Build splits a plan into drift vs pending changes. no-op and read resources
// are dropped, as are pending changes that only reconcile a resource already
// listed as drift (Terraform proposing to undo the drift is not a change from
// configuration). An in-place drift whose only attribute differences are
// empty-ish (e.g. `tags: null -> {}`, a provider refresh round-trip) is dropped
// too — a clean plan confirms it is not real drift.
//
// Attribute diffs are attached only when both a before and an after state
// exist (an in-place change). A resource that was created or destroyed outside
// Terraform has one nil side — diffing it just lists every attribute against
// nil, which is noise — so instead we record an Identity (name/id/arn) for the
// one-line summary the writers print.
func Build(p *plan.Plan) *Report {
	r := &Report{Schema: ReportSchema, TerraformVersion: p.TerraformVersion}

	drifted := make(map[string]bool, len(p.ResourceDrift))
	for _, rc := range p.ResourceDrift {
		rr := ResourceReport{
			Address: rc.Address,
			Type:    rc.Type,
			Module:  rc.ModuleAddress,
			Action:  rc.Change.Action(),
		}
		switch {
		case rc.Change.After == nil:
			rr.Identity = identityOf(rc.Change.Before)
		case rc.Change.Before == nil:
			rr.Identity = identityOf(rc.Change.After)
		default:
			rr.Attrs = plan.DiffAttrs(rc.Change.Before, rc.Change.After)
			if len(rr.Attrs) == 0 {
				// The only differences were empty-ish (e.g. tags: null -> {}) —
				// a provider round-trip on refresh, which a clean plan confirms
				// is not drift. Drop it, and leave the address off `drifted` so
				// a genuine pending change for it still shows.
				continue
			}
		}
		drifted[rc.Address] = true
		r.Drift = append(r.Drift, rr)
	}

	for _, rc := range p.ResourceChanges {
		act := rc.Change.Action()
		if act == "no-op" || act == "read" {
			continue
		}
		if drifted[rc.Address] {
			continue
		}
		rr := ResourceReport{Address: rc.Address, Module: rc.ModuleAddress, Action: act}
		if act == "update" || act == "replace" {
			rr.Attrs = plan.DiffAttrs(rc.Change.Before, rc.Change.After)
		}
		r.Pending = append(r.Pending, rr)
	}

	return r
}

// HasDrift reports whether any resource changed outside Terraform.
func (r *Report) HasDrift() bool { return len(r.Drift) > 0 }

// HasDeprecations reports whether the plan raised any deprecation warning.
func (r *Report) HasDeprecations() bool { return len(r.Deprecations) > 0 }

// HasGatingFindings reports whether any un-suppressed drift or deprecation was
// found — the set the -exit-code gate cares about.
func (r *Report) HasGatingFindings() bool {
	for i := range r.Drift {
		if !r.Drift[i].Suppressed {
			return true
		}
	}
	for i := range r.Deprecations {
		if !r.Deprecations[i].Suppressed {
			return true
		}
	}
	if gating, _, _ := r.gatingRetirements(); len(gating) > 0 {
		return true
	}
	return false
}

// HasNewFindings reports whether any un-suppressed finding was absent from the
// previous run. Meaningful only after PriorResults.StampReport: with no
// -baseline nothing is stamped and this is always false, so callers must treat
// "not baselined" as "cannot tell" rather than as "nothing new" — see
// IsBaselined.
func (r *Report) HasNewFindings() bool {
	for i := range r.Drift {
		if !r.Drift[i].Suppressed && r.Drift[i].BaselineState == "new" {
			return true
		}
	}
	for i := range r.Deprecations {
		if !r.Deprecations[i].Suppressed && r.Deprecations[i].BaselineState == "new" {
			return true
		}
	}
	for i := range r.Retirements {
		if !r.Retirements[i].Suppressed && r.Retirements[i].BaselineState == "new" {
			return true
		}
	}
	return false
}

// IsBaselined reports whether this report has been diffed against a previous
// run, i.e. whether baseline_state / first_seen mean anything.
func (r *Report) IsBaselined() bool {
	for i := range r.Drift {
		if r.Drift[i].BaselineState != "" {
			return true
		}
	}
	for i := range r.Deprecations {
		if r.Deprecations[i].BaselineState != "" {
			return true
		}
	}
	for i := range r.Retirements {
		if r.Retirements[i].BaselineState != "" {
			return true
		}
	}
	return false
}

// gatingDrift / gatingDeprecations return the findings that count toward the
// gate; suppressed ones are split out for the "ignored" sections.
func (r *Report) gatingDrift() (gating, ignored []ResourceReport) {
	for _, rr := range r.Drift {
		if rr.Suppressed {
			ignored = append(ignored, rr)
		} else {
			gating = append(gating, rr)
		}
	}
	return
}

func (r *Report) gatingDeprecations() (gating, ignored []Deprecation) {
	for _, d := range r.Deprecations {
		if d.Suppressed {
			ignored = append(ignored, d)
		} else {
			gating = append(gating, d)
		}
	}
	return
}

// Deprecations filters plan diagnostics to deprecation notices and collapses
// them: one entry per distinct summary+detail, carrying every source location
// that raised it. Terraform re-emits a notice per resource instance / module
// expansion, and the same deprecated argument commonly appears across several
// resources — the Scans tab de-duplicates nothing, so this is where it happens.
// Stable order: by first site (located ones first), then summary.
func Deprecations(diags []plan.Diagnostic) []Deprecation {
	byKey := map[string]*Deprecation{}
	siteSeen := map[string]map[string]bool{}
	var order []string

	for _, d := range diags {
		if !isDeprecation(d) {
			continue
		}
		gk := deprecationKey(d.Summary, d.Detail)
		dep := byKey[gk]
		if dep == nil {
			dep = &Deprecation{Severity: d.Severity, Summary: d.Summary, Detail: d.Detail}
			byKey[gk] = dep
			siteSeen[gk] = map[string]bool{}
			order = append(order, gk)
		}
		if d.Severity == "error" {
			dep.Severity = "error"
		}
		site := DeprecationSite{Address: d.Address, File: d.Filename, Line: d.Line}
		if sk := site.key(); !siteSeen[gk][sk] {
			siteSeen[gk][sk] = true
			dep.Sites = append(dep.Sites, site)
		}
	}

	if len(order) == 0 {
		return nil
	}
	out := make([]Deprecation, 0, len(order))
	for _, gk := range order {
		dep := byKey[gk]
		sort.SliceStable(dep.Sites, func(i, j int) bool { return dep.Sites[i].less(dep.Sites[j]) })
		out = append(out, *dep)
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := out[i].firstSite(), out[j].firstSite()
		switch {
		case si.less(sj):
			return true
		case sj.less(si):
			return false
		default:
			return out[i].Summary < out[j].Summary
		}
	})
	return out
}

// isDeprecation picks the plan diagnostics that are deprecation notices. There
// is no machine-readable marker, so this matches on the word Terraform and
// providers use in the summary or detail.
func isDeprecation(d plan.Diagnostic) bool {
	if d.Severity != "warning" && d.Severity != "error" {
		return false
	}
	return strings.Contains(strings.ToLower(d.Summary+" "+d.Detail), "deprecat")
}

// deprecationKey identifies "the same deprecation" regardless of where it
// fires — its summary plus detail. Used to group notices and as the stable
// SARIF partialFingerprint.
func deprecationKey(summary, detail string) string {
	return strings.ToLower(summary) + "\x00" + strings.ToLower(detail)
}

func (d Deprecation) key() string { return deprecationKey(d.Summary, d.Detail) }

// identityOf pulls a human identifier out of a resource's state, preferring the
// short name over the long id/arn.
func identityOf(attrs map[string]any) string {
	for _, k := range []string{"name", "id", "arn"} {
		if s, ok := attrs[k].(string); ok && s != "" {
			return k + "=" + s
		}
	}
	return ""
}

// SummaryLine is the one-line description used when a drifted resource has no
// attribute diff worth showing — created or destroyed outside Terraform, where
// diffing against a nil side would just list every attribute.
func (rr ResourceReport) SummaryLine() string { return rr.summaryLine() }

// summaryLine is the one-line description of a drifted resource, used instead of
// an attribute dump for create/delete and as the JUnit failure body.
func (rr ResourceReport) summaryLine() string {
	switch rr.Action {
	case "delete":
		return "deleted outside Terraform" + parenID(rr.Identity)
	case "create":
		return "created outside Terraform" + parenID(rr.Identity)
	default:
		if len(rr.Attrs) == 0 {
			return rr.Action + " (no visible attribute changes)"
		}
		return fmt.Sprintf("%s: %d attribute(s) changed", rr.Action, len(rr.Attrs))
	}
}

func parenID(id string) string {
	if id == "" {
		return ""
	}
	return " (" + clip(id, 80) + ")"
}

// driftBreakdown is the "1 deleted, 2 updated" tail on the drift header.
func driftBreakdown(rows []ResourceReport) string {
	var del, cre, upd, other int
	for _, rr := range rows {
		switch rr.Action {
		case "delete":
			del++
		case "create":
			cre++
		case "update", "replace":
			upd++
		default:
			other++
		}
	}
	var parts []string
	for _, p := range []struct {
		n int
		s string
	}{{del, "deleted"}, {cre, "created"}, {upd, "updated"}, {other, "other"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.s))
		}
	}
	return strings.Join(parts, ", ")
}

// clip shortens s to max runes, adding an ellipsis when it truncates.
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// --- JUnit --------------------------------------------------------------------

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

// WriteJUnit renders the drift list as a JUnit test suite: one failing test case
// per resource changed outside Terraform, its attribute diffs in the failure
// body. Suppressed drift is a skipped case under classname `tf-snag.ignored`
// (live drift is `tf-snag.drift`), so the Tests tab groups the two apart and
// shows Failed vs Skipped in the Outcome column. A clean report emits a single
// passing case so the tab shows green rather than "no results". Pending changes
// are context, not tests — they belong in the text/markdown/JSON output.
func (r *Report) WriteJUnit(w io.Writer) error {
	suite := junitSuite{Name: "tf-snag"}
	drift, ignoredDrift := r.gatingDrift()

	if len(drift) == 0 {
		suite.Cases = append(suite.Cases, junitCase{
			Name:      "no drift detected",
			Classname: "tf-snag",
		})
	}
	for _, rr := range drift {
		body := attrLines(rr.Attrs)
		if body == "" {
			body = rr.summaryLine()
		}
		suite.Cases = append(suite.Cases, junitCase{
			Name:      rr.Address + moduleSuffix(rr.Module),
			Classname: "tf-snag.drift",
			Failure: &junitFailure{
				Message: "changed outside Terraform (" + rr.Action + ")",
				Body:    body,
			},
		})
	}
	// One case per retirement, not per instance: the work is "stop using this",
	// however many resources are on it, and the instances go in the body.
	rets, deferredRets, ignoredRets := r.gatingRetirements()
	for _, rt := range rets {
		addrs := make([]string, len(rt.Instances))
		for i, in := range rt.Instances {
			addrs[i] = in.Address
		}
		body := strings.Join(addrs, "\n")
		if rt.Remediation != "" {
			body += "\n\n" + rt.Remediation
		}
		suite.Cases = append(suite.Cases, junitCase{
			Name:      rt.Title,
			Classname: "tf-snag.retirements",
			Failure: &junitFailure{
				Message: fmt.Sprintf("%s retires %s (%s)", rt.Type, rt.RetiresOn, rt.When()),
				Body:    body,
			},
		})
	}
	for _, rt := range deferredRets {
		suite.Cases = append(suite.Cases, junitCase{
			Name:      rt.Title,
			Classname: "tf-snag.retirements",
			Skipped: &junitSkipped{Message: fmt.Sprintf("%s retires %s (%s) — beyond the %d-day fail window",
				rt.Type, rt.RetiresOn, rt.When(), r.RetirementGateDays)},
		})
	}
	for _, rr := range ignoredDrift {
		suite.Cases = append(suite.Cases, junitCase{
			Name:      rr.Address + moduleSuffix(rr.Module),
			Classname: "tf-snag.ignored",
			Skipped:   &junitSkipped{Message: "ignored — " + reasonOr(rr.SuppressReason) + " [" + rr.SuppressSrc + "]"},
		})
	}
	for _, rt := range ignoredRets {
		suite.Cases = append(suite.Cases, junitCase{
			Name:      rt.Title,
			Classname: "tf-snag.ignored",
			Skipped:   &junitSkipped{Message: "ignored — " + reasonOr(rt.SuppressReason) + " [" + rt.SuppressSrc + "]"},
		})
	}
	suite.Tests = len(suite.Cases)
	suite.Failures = len(drift) + len(rets)
	suite.Skipped = len(ignoredDrift) + len(ignoredRets) + len(deferredRets)

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(junitSuites{Suites: []junitSuite{suite}}); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func attrLines(attrs []plan.AttrDiff) string {
	var b strings.Builder
	for i, a := range attrs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s: %s => %s", a.Path, render(a.Old), render(a.New))
	}
	return b.String()
}

// --- Markdown ---------------------------------------------------------------

// WriteMarkdown renders the report as Markdown for Azure DevOps'
// `##vso[task.uploadsummary]`, which shows it on the run's Summary tab under
// "Extensions". Drift and deprecation detail use pipe tables; if a future
// Extensions renderer shows those as literal text, fall back to nested lists.
// Still no raw HTML or `<details>` — those do render as literal text there.
func (r *Report) WriteMarkdown(w io.Writer) error {
	bw := &errWriter{w: w}
	add, chg, del := tally(r.Pending)
	drift, ignoredDrift := r.gatingDrift()
	depr, ignoredDepr := r.gatingDeprecations()

	// No leading "## tf-snag" — Azure DevOps already titles the summary
	// section from the attachment filename, so a heading here doubles it up.
	if len(drift) == 0 {
		bw.printf("🟢 **No drift detected**\n")
	} else {
		line := fmt.Sprintf("🔴 **%d resource%s changed outside Terraform**", len(drift), plural(len(drift)))
		if b := driftBreakdown(drift); b != "" {
			line += " — " + b
		}
		bw.printf("%s\n", line)
	}
	if len(depr) > 0 {
		bw.printf("🟡 **%d deprecation warning%s**\n", len(depr), plural(len(depr)))
	}
	if n := len(ignoredDrift) + len(ignoredDepr); n > 0 {
		bw.printf("🔕 **%d ignored**\n", n)
	}
	meta := fmt.Sprintf("pending: %d to add, %d to change, %d to destroy", add, chg, del)
	if r.TerraformVersion != "" {
		meta = "Terraform " + r.TerraformVersion + " · " + meta
	}
	bw.printf("\n_%s_\n", meta)

	if len(drift) > 0 {
		bw.printf("\n### Changed outside Terraform\n\n")
		bw.printf("| Resource | Attribute | Change |\n|---|---|---|\n")
		for _, rr := range drift {
			res := "`" + mdCell(rr.Address) + "`"
			if rr.Module != "" {
				res += " _(" + mdCell(rr.Module) + ")_"
			}
			if len(rr.Attrs) == 0 {
				bw.printf("| %s | — | %s |\n", res, mdCell(rr.summaryLine()))
				continue
			}
			for _, a := range rr.Attrs {
				bw.printf("| %s | `%s` | `%s` → `%s` |\n", res,
					mdCell(a.Path), mdCell(clip(render(a.Old), 100)), mdCell(clip(render(a.New), 100)))
			}
		}
	}

	if len(r.Pending) > 0 {
		bw.printf("\n### Pending changes from configuration\n\n")
		bw.printf("_%d to add, %d to change, %d to destroy_\n\n", add, chg, del)
		for _, rr := range r.Pending {
			bw.printf("- `%s` `%s`%s\n", sign(rr.Action), mdText(rr.Address), mdItalicModule(rr.Module))
		}
	}

	if len(depr) > 0 {
		bw.printf("\n### Deprecation warnings\n\n")
		bw.printf("| Deprecation | Resources |\n|---|---|\n")
		for _, d := range depr {
			dep := "**" + mdCell(d.Summary) + "**"
			if d.Detail != "" {
				dep += " — " + mdCell(firstSentence(strings.ReplaceAll(d.Detail, "\n", " "), 200))
			}
			sites := make([]string, len(d.Sites))
			for i, s := range d.Sites {
				sites[i] = mdSiteCell(s)
			}
			bw.printf("| %s | %s |\n", dep, strings.Join(sites, ", "))
		}
	}

	if rets := r.reportedRetirements(); len(rets) > 0 {
		bw.printf("\n### Retirements\n\n")
		bw.printf("| Retiring | Date | Resources |\n|---|---|---|\n")
		for _, rt := range rets {
			title := "**" + mdCell(rt.Title) + "**"
			if rt.URL != "" {
				title = "[" + title + "](" + rt.URL + ")"
			}
			addrs := make([]string, len(rt.Instances))
			for i, in := range rt.Instances {
				addrs[i] = "`" + mdCell(in.Address) + "`"
			}
			when := rt.When()
			if rt.Deferred(r.RetirementGateDays) {
				when += ", beyond the fail window"
			}
			bw.printf("| %s | %s _(%s)_ | %s |\n",
				title, rt.RetiresOn, mdCell(when), strings.Join(addrs, ", "))
		}
		if r.Catalogue != nil && r.Catalogue.Stale {
			bw.printf("\n_Retirement catalogue (%s) last updated %s, %d days ago: it may be missing newer notices._\n",
				mdText(r.Catalogue.Source), r.Catalogue.Updated, r.Catalogue.AgeDays)
		}
	}

	if len(ignoredDrift) > 0 || len(ignoredDepr) > 0 {
		bw.printf("\n### Ignored\n\n")
		bw.printf("| Item | Reason | Rule |\n|---|---|---|\n")
		for _, rr := range ignoredDrift {
			bw.printf("| `%s` | %s | `%s` |\n",
				mdCell(rr.Address), mdCell(reasonOr(rr.SuppressReason)), mdCell(rr.SuppressSrc))
		}
		for _, d := range ignoredDepr {
			bw.printf("| %s | %s | `%s` |\n",
				mdCell(d.Summary), mdCell(reasonOr(d.SuppressReason)), mdCell(d.SuppressSrc))
		}
	}

	return bw.err
}

// mdCell escapes a string for use inside a Markdown table cell.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return mdText(s)
}

// mdSiteCell renders a deprecation site for a table cell: `addr` (file:line).
func mdSiteCell(s DeprecationSite) string {
	loc := s.File
	if s.File != "" && s.Line > 0 {
		loc = fmt.Sprintf("%s:%d", s.File, s.Line)
	}
	switch {
	case s.Address != "" && loc != "":
		return "`" + mdCell(s.Address) + "` (" + mdCell(loc) + ")"
	case s.Address != "":
		return "`" + mdCell(s.Address) + "`"
	default:
		return "`" + mdCell(loc) + "`"
	}
}

// firstSentence trims s to its first sentence when that leaves a useful amount
// of text; otherwise it clips to max runes.
func firstSentence(s string, max int) string {
	if i := strings.Index(s, ". "); i >= 40 && i < max {
		return s[:i+1]
	}
	return clip(s, max)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func mdItalicModule(mod string) string {
	if mod == "" {
		return ""
	}
	return " _(" + mdText(mod) + ")_"
}

// mdText neutralises characters that would break out of an inline-code span or
// a list item in the Azure DevOps summary renderer.
func mdText(s string) string {
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// --- SARIF ----------------------------------------------------------------

// The SARIF SAST Scans Tab extension (sariftools.scans) renders a "Scans" tab
// from a build artifact named CodeAnalysisLogs containing *.sarif files. Its
// columns are Message, Rule, Severity, Path (file:line) and Baseline, so:
//   - the drift kind goes in the rule id / name  (Rule column)
//   - the action maps to a SARIF baselineState    (Baseline column)
//   - the .tf declaration, when known, is the path (Path column, links to code)
//   - the readable resource type leads the message (no column for it)

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	InformationURI string      `json:"informationUri,omitempty"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string    `json:"id"`
	Name             string    `json:"name,omitempty"`
	ShortDescription sarifText `json:"shortDescription"`
	DefaultConfig    struct {
		Level string `json:"level"`
	} `json:"defaultConfiguration"`
}

type sarifText struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID string `json:"ruleId"`
	// GUID is a deterministic per-finding id (see resultGUID) used to match a
	// result against the previous run when -baseline is given.
	GUID                string            `json:"guid,omitempty"`
	Level               string            `json:"level"`
	BaselineState       string            `json:"baselineState,omitempty"`
	Message             sarifText         `json:"message"`
	Locations           []sarifLocation   `json:"locations,omitempty"`
	RelatedLocations    []sarifLocation   `json:"relatedLocations,omitempty"`
	LogicalLocations    []sarifLogicalLoc `json:"logicalLocations,omitempty"`
	PartialFingerprints map[string]string `json:"partialFingerprints,omitempty"`
	// Provenance.firstDetectionTimeUtc — set only under -baseline: the time the
	// finding was first seen, forwarded from the previous run. The Scans tab
	// renders it as a "First Observed" date and an "Age" column.
	Provenance   *sarifProvenance   `json:"provenance,omitempty"`
	Suppressions []sarifSuppression `json:"suppressions,omitempty"`
	Properties   map[string]string  `json:"properties,omitempty"`
}

type sarifProvenance struct {
	FirstDetectionTimeUtc string `json:"firstDetectionTimeUtc,omitempty"`
	// Properties carries firstDetectionRunUrl — the CI run that first surfaced
	// this finding. SARIF has firstDetectionRunGuid, but a guid is not something
	// a reader can click, and the property bag is the sanctioned extension point.
	Properties map[string]string `json:"properties,omitempty"`
}

// provFirstRunURL is the property key under which a result's first-detection run
// URL travels between runs.
const provFirstRunURL = "firstDetectionRunUrl"

func (p *sarifProvenance) firstRunURL() string {
	if p == nil {
		return ""
	}
	return p.Properties[provFirstRunURL]
}

// newProvenance builds a provenance block, omitting the property bag when there
// is no run URL to record.
func newProvenance(firstSeen, runURL string) *sarifProvenance {
	p := &sarifProvenance{FirstDetectionTimeUtc: firstSeen}
	if runURL != "" {
		p.Properties = map[string]string{provFirstRunURL: runURL}
	}
	return p
}

// sarifSuppression marks a result the user chose to ignore. kind "inSource" is
// an inline `# tf-snag:ignore` comment; "external" is the ignore file. The Scans
// tab hides suppressed results by default (Suppression filter = unsuppressed).
type sarifSuppression struct {
	Kind          string `json:"kind"`
	Justification string `json:"justification,omitempty"`
}

func suppressionsFor(kind, reason string) []sarifSuppression {
	if kind == "" {
		kind = "external"
	}
	return []sarifSuppression{{Kind: kind, Justification: reason}}
}

type sarifLocation struct {
	PhysicalLocation struct {
		ArtifactLocation struct {
			URI string `json:"uri"`
		} `json:"artifactLocation"`
		Region *sarifRegion `json:"region,omitempty"`
	} `json:"physicalLocation"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

type sarifLogicalLoc struct {
	FullyQualifiedName string `json:"fullyQualifiedName"`
	Name               string `json:"name,omitempty"`
	Kind               string `json:"kind,omitempty"`
}

// Two rules -> two collapsible groups in the SARIF Scans tab. We do NOT split
// drift into per-kind rules — the kind (Deleted / Updated / Created) still leads
// each resource-drift message. The tab orders groups by result count (or
// alphabetically if the viewer re-sorts), not by this slice's order, so their
// vertical order is not ours to fix.
//
// No Name on either: the tab builds its group header as "<id>: <name>", and a
// name here only ever echoed the id. Left unset, the header is just the id.
var sarifRules = []sarifRule{
	func() sarifRule {
		ru := sarifRule{
			ID:               "resource-drift",
			ShortDescription: sarifText{Text: "Resource changed outside Terraform"},
		}
		ru.DefaultConfig.Level = "warning"
		return ru
	}(),
	func() sarifRule {
		ru := sarifRule{
			ID:               "deprecation",
			ShortDescription: sarifText{Text: "Deprecated syntax or feature in Terraform configuration"},
		}
		ru.DefaultConfig.Level = "warning"
		return ru
	}(),
	func() sarifRule {
		ru := sarifRule{
			ID:               "retirement",
			ShortDescription: sarifText{Text: "Resource uses a service or SKU the provider is retiring"},
		}
		ru.DefaultConfig.Level = "warning"
		return ru
	}(),
}

// WriteSARIF renders the report as a SARIF 2.1.0 log: one `resource-drift`
// result per resource changed outside Terraform, then one `deprecation` result
// per notice in r.Deprecations. src maps "<type>.<name>" to the .tf declaration
// (may be nil) for drift locations; deprecation locations come from the plan
// log itself. A clean report writes an empty results array.
//
// Every result carries a deterministic `guid` (see resultGUID) and a
// `partialFingerprints` entry. When prior is non-nil (from ParsePriorSARIF of
// the previous run's tf-snag.sarif), each result is stamped with a
// `baselineState` — `new`, or `updated` when it was also in the previous run —
// by matching on guid, and any prior result that has now gone is re-emitted as
// `absent`. When prior is nil, no `baselineState` is set and the Scans tab
// shows every row as "New".
func (r *Report) WriteSARIF(w io.Writer, src map[string]SourceLoc, prior *PriorResults) error {
	run := sarifRun{
		Tool: sarifTool{Driver: sarifDriver{
			Name:           "tf-snag",
			InformationURI: "https://github.com/wes-key/tf-snag",
			Rules:          sarifRules,
		}},
		Results: []sarifResult{},
	}

	for _, rr := range r.Drift {
		res := sarifResult{
			RuleID:  "resource-drift",
			GUID:    resultGUID("resource-drift", rr.Address),
			Level:   sarifLevel(rr.Action),
			Message: sarifText{Text: rr.sarifMessage()},
			LogicalLocations: []sarifLogicalLoc{{
				FullyQualifiedName: rr.Address,
				Name:               resourceName(rr.Address),
				Kind:               "resource",
			}},
			PartialFingerprints: map[string]string{"driftAddress/v1": rr.Address},
			Properties:          sarifProps(rr),
		}
		if rr.Suppressed {
			res.Suppressions = suppressionsFor(rr.SuppressKind, rr.SuppressReason)
		}

		var loc sarifLocation
		if s, ok := src[rr.Type+"."+resourceName(rr.Address)]; ok {
			loc.PhysicalLocation.ArtifactLocation.URI = s.File
			loc.PhysicalLocation.Region = &sarifRegion{StartLine: s.Line}
		} else {
			loc.PhysicalLocation.ArtifactLocation.URI = rr.Address
		}
		res.Locations = []sarifLocation{loc}

		prior.stamp(&res)
		run.Results = append(run.Results, res)
	}

	for _, d := range r.Deprecations {
		res := sarifResult{
			RuleID:              "deprecation",
			GUID:                resultGUID("deprecation", d.key()),
			Level:               sarifSeverityLevel(d.Severity),
			Message:             sarifText{Text: deprecationMessage(d)},
			PartialFingerprints: map[string]string{"deprecation/v1": d.key()},
			Properties:          deprecationProps(d),
		}
		if d.Suppressed {
			res.Suppressions = suppressionsFor(d.SuppressKind, d.SuppressReason)
		}
		for i, s := range d.Sites {
			if s.Address != "" {
				res.LogicalLocations = append(res.LogicalLocations, sarifLogicalLoc{
					FullyQualifiedName: s.Address,
					Name:               resourceName(s.Address),
					Kind:               "resource",
				})
			}
			loc := siteLocation(s)
			if i == 0 {
				res.Locations = []sarifLocation{loc}
			} else {
				res.RelatedLocations = append(res.RelatedLocations, loc)
			}
		}
		if len(res.Locations) == 0 {
			res.Locations = []sarifLocation{siteLocation(DeprecationSite{})}
		}

		prior.stamp(&res)
		run.Results = append(run.Results, res)
	}

	// One result per catalogue entry, with the affected resources as logical
	// locations: the same grouping the rest of the report uses, and it keeps a
	// finding's guid stable as instances come and go.
	for _, rt := range r.Retirements {
		res := sarifResult{
			RuleID:              "retirement",
			GUID:                resultGUID("retirement", rt.ID),
			Level:               retirementLevel(rt),
			Message:             sarifText{Text: retirementMessage(rt)},
			PartialFingerprints: map[string]string{"retirement/v1": rt.ID},
			Properties:          retirementProps(rt),
		}
		if rt.Suppressed {
			res.Suppressions = suppressionsFor(rt.SuppressKind, rt.SuppressReason)
		}
		for _, in := range rt.Instances {
			res.LogicalLocations = append(res.LogicalLocations, sarifLogicalLoc{
				FullyQualifiedName: in.Address,
				Name:               resourceName(in.Address),
				Kind:               "resource",
			})
			loc := sarifLocation{}
			if in.File != "" {
				loc.PhysicalLocation.ArtifactLocation.URI = in.File
				loc.PhysicalLocation.Region = &sarifRegion{StartLine: in.Line}
			} else {
				loc.PhysicalLocation.ArtifactLocation.URI = in.Address
			}
			if len(res.Locations) == 0 {
				res.Locations = []sarifLocation{loc}
			} else {
				res.RelatedLocations = append(res.RelatedLocations, loc)
			}
		}
		prior.stamp(&res)
		run.Results = append(run.Results, res)
	}

	run.Results = append(run.Results, prior.absent()...)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(sarifLog{
		Schema:  "https://json.schemastore.org/sarif-2.1.0.json",
		Version: "2.1.0",
		Runs:    []sarifRun{run},
	})
}

// driftVerb is the kind label at the front of a Scans row's message (there is
// no separate column for it once results share one rule). No glyph — the
// severity icon in column 0 (from level) already carries the visual weight.
func driftVerb(action string) string {
	switch action {
	case "delete":
		return "Deleted"
	case "create":
		return "Created"
	default:
		return "Updated"
	}
}

func sarifLevel(action string) string {
	switch action {
	case "delete", "create":
		return "error"
	default:
		return "warning"
	}
}

// sarifNS is a fixed namespace UUID for deriving result GUIDs (RFC 4122 v5).
// Any constant UUID works; it must never change or every finding's guid moves.
var sarifNS = [16]byte{
	0x7d, 0x3a, 0x2c, 0x91, 0x4b, 0x8e, 0x4f, 0x1a,
	0x9c, 0x6d, 0x2e, 0x5f, 0x0a, 0x1b, 0x3c, 0x4d,
}

// FindingID is a finding's stable identity across runs — the same value the
// SARIF result carries as its guid. It is what lets a tracker recognise an
// existing item for this finding rather than raising a duplicate every day, so
// it must stay derived from the address alone: anything that changes when the
// drift changes (attribute values, counts, wording) would break the match.
func (rr ResourceReport) FindingID() string {
	return resultGUID("resource-drift", rr.Address)
}

// FindingID is the deprecation's equivalent, keyed on summary + detail so all
// the sites tripping one notice share a single identity.
func (d Deprecation) FindingID() string {
	return resultGUID("deprecation", d.key())
}

// FindingID for a retirement is keyed on the catalogue id alone, so the item
// tracking "stop using Basic public IPs" survives instances being migrated one
// at a time — the work is done when the last one goes, not when the first does.
func (rt Retirement) FindingID() string {
	return resultGUID("retirement", rt.ID)
}

// resultGUID derives a deterministic RFC 4122 v5 UUID from the finding's kind
// and stable identity, so the same finding gets the same guid on every run.
// Sarif.Multitool uses it as the primary key when matching results between a
// run and its predecessor to compute baselineState.
func resultGUID(kind, identity string) string {
	h := sha1.New()
	h.Write(sarifNS[:])
	h.Write([]byte(kind + "\x00" + identity))
	b := h.Sum(nil)[:16]
	b[6] = b[6]&0x0f | 0x50 // version 5
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// --- baseline (new / updated / absent) ---------------------------------

// PriorResults is the previous run's SARIF results, indexed by identity, for
// baseline comparison. The zero value / a nil *PriorResults means "no baseline"
// — WriteSARIF then leaves baselineState unset.
type PriorResults struct {
	byKey map[string]*priorResult
	// RunURL is this run's URL. A finding absent from the baseline is being seen
	// for the first time here, so this is recorded as its first-detection run
	// and travels forward with it on every run after.
	RunURL string
}

type priorResult struct {
	ruleID     string
	level      string
	message    string
	loc        []sarifLocation
	logloc     []sarifLogicalLoc
	fp         map[string]string
	guid       string
	firstSeen  string // provenance.firstDetectionTimeUtc, "" if the prior run had none
	firstRunID string // provenance properties firstDetectionRunUrl
	suppressed bool   // the prior run had an ignore rule covering this finding
	matched    bool
}

// ParsePriorSARIF reads a tf-snag SARIF log (a previous run's tf-snag.sarif) so
// the next WriteSARIF can diff against it. Malformed JSON is an error; a log
// with no runs/results yields an empty set (everything will read as "new").
func ParsePriorSARIF(raw []byte) (*PriorResults, error) {
	raw, err := plan.DecodeUTF(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing baseline SARIF: %w", err)
	}
	var log sarifLog
	if err := json.Unmarshal(raw, &log); err != nil {
		return nil, fmt.Errorf("parsing baseline SARIF: %w", err)
	}
	pr := &PriorResults{byKey: map[string]*priorResult{}}
	for _, run := range log.Runs {
		for _, res := range run.Results {
			var firstSeen string
			if res.Provenance != nil {
				firstSeen = res.Provenance.FirstDetectionTimeUtc
			}
			pr.byKey[resultKey(res)] = &priorResult{
				ruleID:     res.RuleID,
				level:      res.Level,
				message:    stripAge(res.Message.Text),
				loc:        res.Locations,
				logloc:     res.LogicalLocations,
				fp:         res.PartialFingerprints,
				guid:       res.GUID,
				firstSeen:  firstSeen,
				firstRunID: res.Provenance.firstRunURL(),
				// Suppressed findings are written to the log too, so the
				// baseline records which ones an ignore rule covered. Reading it
				// back is what lets a later run notice a rule was removed.
				suppressed: len(res.Suppressions) > 0,
			}
		}
	}
	return pr, nil
}

// resultKey is a result's cross-run identity: its guid, else a stable rendering
// of its partialFingerprints, else ruleId + message text.
func resultKey(res sarifResult) string {
	if res.GUID != "" {
		return "g:" + res.GUID
	}
	if len(res.PartialFingerprints) > 0 {
		keys := make([]string, 0, len(res.PartialFingerprints))
		for k := range res.PartialFingerprints {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("f:")
		for _, k := range keys {
			fmt.Fprintf(&b, "%s=%s;", k, res.PartialFingerprints[k])
		}
		return b.String()
	}
	return "r:" + res.RuleID + "\x00" + stripAge(res.Message.Text)
}

// stamp sets res.BaselineState from the prior run: unmatched -> "new", anything
// carried over from the previous run -> "updated". We never emit "unchanged":
// the Scans tab hides that state by default and we want persistent drift and
// deprecations to stay on screen — provenance.firstDetectionTimeUtc (forwarded
// from the prior result, or now for a new one) is what tells long-standing
// findings apart from genuinely new ones. A nil receiver is a no-op.
func (pr *PriorResults) stamp(res *sarifResult) {
	if pr == nil {
		return
	}
	p, ok := pr.byKey[resultKey(*res)]
	if !ok {
		now := nowUTC()
		res.BaselineState = "new"
		res.Provenance = newProvenance(now, pr.RunURL)
		res.Message.Text = withAge(res.Message.Text, now)
		return
	}
	p.matched = true
	res.BaselineState = "updated"
	seen, runURL := p.firstSeen, p.firstRunID
	if seen == "" {
		// Prior run predates provenance tracking: this is the earliest sighting
		// we can attest to, so claim it rather than inventing a history.
		seen, runURL = nowUTC(), pr.RunURL
	}
	res.Provenance = newProvenance(seen, runURL)
	res.Message.Text = withAge(res.Message.Text, seen)
}

// StampReport sets BaselineState + FirstSeen on every drift and deprecation in
// r by matching each against the previous run — the same guid derivation
// WriteSARIF uses. Unmatched -> "new" + FirstSeen now; carried over -> "updated"
// + the prior run's FirstSeen (or now, if the prior run predates provenance).
// A nil receiver is a no-op: findings stay unstamped and the extension renders
// them as "new". Used by `-format json -baseline`.
func (pr *PriorResults) StampReport(r *Report) {
	if pr == nil {
		return
	}
	now := nowUTC()
	for i := range r.Drift {
		m := pr.matchGUID(resultGUID("resource-drift", r.Drift[i].Address))
		r.Drift[i].BaselineState = m.state
		r.Drift[i].FirstSeen = firstOr(m.firstSeen, now)
		r.Drift[i].FirstRunURL = firstOr(m.firstRunURL, pr.RunURL)
		r.Drift[i].Unsuppressed = m.wasSuppressed && !r.Drift[i].Suppressed
	}
	for i := range r.Deprecations {
		m := pr.matchGUID(resultGUID("deprecation", r.Deprecations[i].key()))
		r.Deprecations[i].BaselineState = m.state
		r.Deprecations[i].FirstSeen = firstOr(m.firstSeen, now)
		r.Deprecations[i].FirstRunURL = firstOr(m.firstRunURL, pr.RunURL)
		r.Deprecations[i].Unsuppressed = m.wasSuppressed && !r.Deprecations[i].Suppressed
	}
}

// baselineMatch is what the previous run knew about one finding.
type baselineMatch struct {
	state         string // "new" | "updated"
	firstSeen     string
	firstRunURL   string
	wasSuppressed bool
}

// matchGUID looks a finding up by the guid key ParsePriorSARIF stores under
// ("g:"+guid).
func (pr *PriorResults) matchGUID(guid string) baselineMatch {
	p, ok := pr.byKey["g:"+guid]
	if !ok {
		return baselineMatch{state: "new"}
	}
	p.matched = true
	return baselineMatch{
		state:         "updated",
		firstSeen:     p.firstSeen,
		firstRunURL:   p.firstRunID,
		wasSuppressed: p.suppressed,
	}
}

// HasNewlyActionable reports whether any finding is either new or has just come
// out from under an ignore rule — the two ways a run can produce something that
// warrants attention it did not warrant before.
func (r *Report) HasNewlyActionable() bool {
	for i := range r.Drift {
		if !r.Drift[i].Suppressed && (r.Drift[i].BaselineState == "new" || r.Drift[i].Unsuppressed) {
			return true
		}
	}
	for i := range r.Deprecations {
		if !r.Deprecations[i].Suppressed && (r.Deprecations[i].BaselineState == "new" || r.Deprecations[i].Unsuppressed) {
			return true
		}
	}
	// Retirements count too. They were added after this function, and leaving
	// them out made -teams-notify new and -pr-comment new silent about a
	// resource that had just landed on a published retirement notice — the one
	// finding nobody can fix by waiting.
	for i := range r.Retirements {
		if !r.Retirements[i].Suppressed && (r.Retirements[i].BaselineState == "new" || r.Retirements[i].Unsuppressed) {
			return true
		}
	}
	return false
}

func firstOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// nowUTC is the current time as a SARIF timestamp; a var so tests can pin it.
var nowUTC = func() string { return time.Now().UTC().Format(time.RFC3339) }

// ageParens is the " (first seen <date>, <n> days ago)" note tacked onto the
// end of a baselined result message's first line — the Azure DevOps Scans tab
// has no Age column, so this is the only place the first-detection time shows
// there. Returns "" if firstSeenUTC is unparseable.
func ageParens(firstSeenUTC string) string {
	t, err := time.Parse(time.RFC3339, firstSeenUTC)
	if err != nil {
		return ""
	}
	days := int(time.Now().UTC().Sub(t).Hours() / 24)
	switch {
	case days <= 0:
		return " (first seen today)"
	case days == 1:
		return " (first seen " + t.Format("2006-01-02") + ", 1 day ago)"
	default:
		return fmt.Sprintf(" (first seen %s, %d days ago)", t.Format("2006-01-02"), days)
	}
}

// withAge inserts ageParens at the end of msg's first line.
func withAge(msg, firstSeenUTC string) string {
	p := ageParens(firstSeenUTC)
	if p == "" {
		return msg
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		return msg[:i] + p + msg[i:]
	}
	return msg + p
}

// stripAge removes any " (first seen ...)" note so a previous run's message
// round-trips cleanly.
func stripAge(msg string) string {
	for {
		i := strings.Index(msg, " (first seen ")
		if i < 0 {
			return msg
		}
		j := strings.IndexByte(msg[i:], ')')
		if j < 0 {
			return msg
		}
		msg = msg[:i] + msg[i+j+1:]
	}
}

// absent returns one "absent" result per prior result that no current result
// matched — a drift remediated or a deprecation removed since last run. Sorted
// by rule then message for stable output. Nil receiver -> nil.
func (pr *PriorResults) absent() []sarifResult {
	if pr == nil {
		return nil
	}
	var out []sarifResult
	for _, p := range pr.byKey {
		if p.matched {
			continue
		}
		lvl := p.level
		if lvl == "" {
			lvl = "note"
		}
		res := sarifResult{
			RuleID:              p.ruleID,
			GUID:                p.guid,
			Level:               lvl,
			BaselineState:       "absent",
			Message:             sarifText{Text: p.message},
			Locations:           p.loc,
			LogicalLocations:    p.logloc,
			PartialFingerprints: p.fp,
		}
		if p.firstSeen != "" {
			res.Provenance = newProvenance(p.firstSeen, p.firstRunID)
			res.Message.Text = withAge(res.Message.Text, p.firstSeen)
		}
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuleID != out[j].RuleID {
			return out[i].RuleID < out[j].RuleID
		}
		return out[i].Message.Text < out[j].Message.Text
	})
	return out
}

func sarifProps(rr ResourceReport) map[string]string {
	p := map[string]string{"action": rr.Action}
	if rr.Type != "" {
		p["resourceType"] = rr.Type
	}
	if rr.Module != "" {
		p["module"] = rr.Module
	}
	if rr.Identity != "" {
		p["identity"] = rr.Identity
	}
	return p
}

// sarifSeverityLevel maps a Terraform diagnostic severity onto a SARIF level.
func sarifSeverityLevel(sev string) string {
	if sev == "error" {
		return "error"
	}
	return "warning"
}

// siteLocation is the SARIF location for one deprecation site: the .tf file and
// line when known, otherwise the resource address as bare location text.
func siteLocation(s DeprecationSite) sarifLocation {
	var loc sarifLocation
	if s.File != "" {
		loc.PhysicalLocation.ArtifactLocation.URI = s.File
		if s.Line > 0 {
			loc.PhysicalLocation.Region = &sarifRegion{StartLine: s.Line}
		}
	} else {
		loc.PhysicalLocation.ArtifactLocation.URI = s.Address
	}
	return loc
}

// siteLabel renders one site as "address (file:line)", degrading to whichever
// half is known.
func siteLabel(s DeprecationSite) string {
	loc := s.File
	if s.File != "" && s.Line > 0 {
		loc = fmt.Sprintf("%s:%d", s.File, s.Line)
	}
	switch {
	case s.Address != "" && loc != "":
		return s.Address + " (" + loc + ")"
	case s.Address != "":
		return s.Address
	default:
		return loc
	}
}

// deprecationMessage is the Scans-grid message: summary on line 1, detail
// (newlines flattened, clipped) on line 2, then one "address (file:line)" line
// per site — always, so a lone deprecation shows where it is just like a
// multi-site one does. The Scans tab renders message.text with
// white-space:pre-line, so the breaks show.
func deprecationMessage(d Deprecation) string {
	b := d.Summary
	if d.Detail != "" {
		b += "\n" + clip(strings.ReplaceAll(d.Detail, "\n", " "), 400)
	}
	for _, s := range d.Sites {
		if lbl := siteLabel(s); lbl != "" {
			b += "\n" + lbl
		}
	}
	return b
}

// retirementLevel escalates as the date nears: a deadline that has passed, or
// is inside 90 days, is an error in the Scans tab; anything further out is a
// warning, so a 2028 date does not sit at the top of the list forever.
func retirementLevel(rt Retirement) string {
	switch rt.Urgency {
	case string(retire.Retired), string(retire.Imminent):
		return "error"
	default:
		return "warning"
	}
}

func retirementMessage(rt Retirement) string {
	b := fmt.Sprintf("%s — retires %s (%s)", rt.Title, rt.RetiresOn, rt.When())
	for _, in := range rt.Instances {
		b += "\n" + in.Address
	}
	if rt.Remediation != "" {
		b += "\n" + clip(rt.Remediation, 400)
	}
	return b
}

func retirementProps(rt Retirement) map[string]string {
	p := map[string]string{
		"retiresOn": rt.RetiresOn,
		"urgency":   rt.Urgency,
		"count":     strconv.Itoa(len(rt.Instances)),
	}
	if rt.Type != "" {
		p["resourceType"] = rt.Type
	}
	if rt.Severity != "" {
		p["severity"] = rt.Severity
	}
	if rt.URL != "" {
		p["url"] = rt.URL
	}
	return p
}

func deprecationProps(d Deprecation) map[string]string {
	p := map[string]string{"severity": d.Severity}
	switch {
	case len(d.Sites) > 1:
		p["count"] = strconv.Itoa(len(d.Sites))
	case len(d.Sites) == 1 && d.Sites[0].Address != "":
		p["address"] = d.Sites[0].Address
	}
	return p
}

// sarifMessage is the Scans-grid message: "<Deleted|Updated|Created> — <what>".
// No resource address or path echoed (the Path column links to the .tf file).
// A single changed attribute stays on the verb line; two or more break onto one
// line each — the Scans tab renders message.text with white-space:pre-line, so
// the newlines show without needing message.markdown (which we avoid).
func (rr ResourceReport) sarifMessage() string {
	verb := driftVerb(rr.Action)
	switch rr.Action {
	case "delete", "create":
		id := rr.Identity
		if id == "" {
			id = resourceName(rr.Address)
		}
		return verb + " — " + id
	default:
		if len(rr.Attrs) == 0 {
			return verb + " — attributes changed"
		}
		parts := make([]string, len(rr.Attrs))
		for i, a := range rr.Attrs {
			parts[i] = fmt.Sprintf("%s: %s → %s", a.Path, clip(render(a.Old), 60), clip(render(a.New), 60))
		}
		if len(parts) == 1 {
			return verb + " — " + parts[0]
		}
		return verb + "\n" + clip(strings.Join(parts, "\n"), 400)
	}
}

// resourceName is the last dotted segment of an address (the local name).
func resourceName(address string) string {
	if i := strings.LastIndex(address, "."); i >= 0 {
		return address[i+1:]
	}
	return address
}

var actionSign = map[string]string{
	"create":  "+",
	"update":  "~",
	"delete":  "-",
	"replace": "±",
	"no-op":   " ",
}

// palette is the set of ANSI codes for one text render. The zero value is
// empty strings, so colour-free output runs through the same code path.
type palette struct {
	reset, bold, red, green, yellow, dim string
}

var colorPalette = palette{
	reset:  "\x1b[0m",
	bold:   "\x1b[1m",
	red:    "\x1b[31m",
	green:  "\x1b[32m",
	yellow: "\x1b[33m",
	dim:    "\x1b[2m",
}

// WriteText renders the plain-text report.
func (r *Report) WriteText(w io.Writer) error { return r.writeText(w, palette{}) }

// WriteTextColor renders the text report with ANSI colour (Azure DevOps and
// most terminals render it; pipe through `-color=never` or a non-TTY to disable).
func (r *Report) WriteTextColor(w io.Writer) error { return r.writeText(w, colorPalette) }

func (r *Report) writeText(w io.Writer, c palette) error {
	bw := &errWriter{w: w}
	drift, ignoredDrift := r.gatingDrift()

	if len(drift) == 0 {
		bw.printf("%stf-snag — no drift detected%s", c.green, c.reset)
		if len(r.Pending) > 0 {
			bw.printf(" (%d pending change(s) from configuration)", len(r.Pending))
		}
		bw.printf("\n")
		r.writeDeprecations(bw, c)
		r.writeRetirements(bw, c)
		r.writeIgnored(bw, c, ignoredDrift)
		return bw.err
	}

	head := fmt.Sprintf("tf-snag — %d resource(s) changed outside Terraform", len(drift))
	if b := driftBreakdown(drift); b != "" {
		head += "  (" + b + ")"
	}
	bw.printf("%s%s%s%s\n\n", c.bold, c.yellow, head, c.reset)

	for _, rr := range drift {
		bw.printf("  %s%s%s %s%s\n", signColor(c, rr.Action), sign(rr.Action), c.reset,
			rr.Address, moduleSuffix(rr.Module))
		if len(rr.Attrs) == 0 {
			bw.printf("      %s%s%s\n", c.dim, rr.summaryLine(), c.reset)
			continue
		}
		pad := attrPad(rr.Attrs)
		for _, a := range rr.Attrs {
			bw.printf("      %-*s  %s%s%s => %s%s%s\n", pad, a.Path,
				c.red, clip(render(a.Old), 100), c.reset, c.green, clip(render(a.New), 100), c.reset)
		}
	}

	if len(r.Pending) > 0 {
		add, chg, del := tally(r.Pending)
		bw.printf("\n%spending changes from configuration: %d to add, %d to change, %d to destroy%s\n",
			c.dim, add, chg, del, c.reset)
		for _, rr := range r.Pending {
			bw.printf("  %s%s%s %s%s\n", signColor(c, rr.Action), sign(rr.Action), c.reset,
				rr.Address, moduleSuffix(rr.Module))
		}
	}

	r.writeDeprecations(bw, c)
	r.writeRetirements(bw, c)
	r.writeIgnored(bw, c, ignoredDrift)
	return bw.err
}

// writeDeprecations prints the un-suppressed deprecation section. No-op when
// there are none to show.
func (r *Report) writeDeprecations(bw *errWriter, c palette) {
	depr, _ := r.gatingDeprecations()
	if len(depr) == 0 {
		return
	}
	bw.printf("\n%sdeprecation warning(s): %d%s\n", c.dim, len(depr), c.reset)
	for _, d := range depr {
		bw.printf("  %s%s%s\n", c.yellow, d.Summary, c.reset)
		for _, s := range d.Sites {
			bw.printf("      %s%s%s\n", c.dim, siteLabel(s), c.reset)
		}
	}
}

// writeIgnored prints the suppressed drift + deprecations tail. No-op when none.
func (r *Report) writeIgnored(bw *errWriter, c palette, ignoredDrift []ResourceReport) {
	_, ignoredDepr := r.gatingDeprecations()
	if len(ignoredDrift) == 0 && len(ignoredDepr) == 0 {
		return
	}
	bw.printf("\n%signored: %d drift, %d deprecation(s)%s\n",
		c.dim, len(ignoredDrift), len(ignoredDepr), c.reset)
	for _, rr := range ignoredDrift {
		bw.printf("  %s%s %s — %s [%s]%s\n", c.dim,
			sign(rr.Action), rr.Address+moduleSuffix(rr.Module),
			reasonOr(rr.SuppressReason), rr.SuppressSrc, c.reset)
	}
	for _, d := range ignoredDepr {
		bw.printf("  %s%s — %s [%s]%s\n", c.dim,
			d.Summary, reasonOr(d.SuppressReason), d.SuppressSrc, c.reset)
	}
}

func reasonOr(s string) string {
	if s == "" {
		return "no reason given"
	}
	return s
}

func attrPad(attrs []plan.AttrDiff) int {
	n := 0
	for _, a := range attrs {
		if len(a.Path) > n {
			n = len(a.Path)
		}
	}
	return n
}

func signColor(c palette, action string) string {
	switch action {
	case "create":
		return c.green
	case "delete", "replace":
		return c.red
	case "update":
		return c.yellow
	default:
		return ""
	}
}

func moduleSuffix(mod string) string {
	if mod == "" {
		return ""
	}
	return "  (" + mod + ")"
}

func tally(rs []ResourceReport) (add, chg, del int) {
	for _, rr := range rs {
		switch rr.Action {
		case "create":
			add++
		case "update":
			chg++
		case "delete":
			del++
		case "replace":
			add++
			del++
		}
	}
	return
}

func sign(action string) string {
	if s, ok := actionSign[action]; ok {
		return s
	}
	return "?"
}

func render(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// errWriter defers the first write error so the format functions stay linear.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}
