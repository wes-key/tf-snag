// Package report turns a parsed plan into tf-snag's summary: resources changed
// outside Terraform (drift) and, secondarily, the pending changes a plan would
// apply from configuration.
package report

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/wes-key/tf-snag/internal/plan"
)

// ReportSchema is the version of the JSON emitted by WriteJSON. Consumers (the
// tf-snag-tab Azure DevOps extension) key off it and should refuse a version
// they do not recognise. Bump it on any breaking change to the JSON shape.
const ReportSchema = 1

type Report struct {
	Schema           int              `json:"schema"`
	TerraformVersion string           `json:"terraform_version"`
	Drift            []ResourceReport `json:"drift"`
	Pending          []ResourceReport `json:"pending"`
	// Deprecations is populated only when `-check` asks for it. It rides on the
	// same Report so WriteSARIF/writeText can emit it alongside drift, but it is
	// omitted from WriteJSON output until the JSON schema is bumped to carry it.
	Deprecations []Deprecation `json:"deprecations,omitempty"`
}

// Deprecation is one deprecation notice from a `terraform plan -json` log,
// collapsed across every source location that trips it.
type Deprecation struct {
	Severity string            `json:"severity"`
	Summary  string            `json:"summary"`
	Detail   string            `json:"detail,omitempty"`
	Sites    []DeprecationSite `json:"sites,omitempty"`
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
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

// WriteJUnit renders the drift list as a JUnit test suite: one failing test case
// per resource changed outside Terraform, its attribute diffs in the failure
// body. A clean report emits a single passing case so the Tests tab shows green
// rather than "no results". Pending changes are not tests — they are context and
// belong in the text/markdown/JSON output.
func (r *Report) WriteJUnit(w io.Writer) error {
	suite := junitSuite{Name: "tf-snag"}

	if len(r.Drift) == 0 {
		suite.Cases = append(suite.Cases, junitCase{
			Name:      "no drift detected",
			Classname: "tf-snag",
		})
	}
	for _, rr := range r.Drift {
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
	suite.Tests = len(suite.Cases)
	suite.Failures = len(r.Drift)

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
// "Extensions". That renderer only handles a basic subset — headings, bold,
// italic, inline code and lists — so this deliberately avoids tables, raw HTML
// and `<details>`, all of which show up as literal text there.
func (r *Report) WriteMarkdown(w io.Writer) error {
	bw := &errWriter{w: w}
	add, chg, del := tally(r.Pending)

	// No leading "## tf-snag" — Azure DevOps already titles the summary
	// section from the attachment filename, so a heading here doubles it up.
	if len(r.Drift) == 0 {
		bw.printf("🟢 **No drift detected**\n")
	} else {
		line := fmt.Sprintf("🔴 **%d resource%s changed outside Terraform**", len(r.Drift), plural(len(r.Drift)))
		if b := driftBreakdown(r.Drift); b != "" {
			line += " — " + b
		}
		bw.printf("%s\n", line)
	}
	meta := fmt.Sprintf("pending: %d to add, %d to change, %d to destroy", add, chg, del)
	if r.TerraformVersion != "" {
		meta = "Terraform " + r.TerraformVersion + " · " + meta
	}
	bw.printf("\n_%s_\n", meta)

	if len(r.Drift) > 0 {
		bw.printf("\n### Changed outside Terraform\n\n")
		for _, rr := range r.Drift {
			bw.printf("**`%s`** · %s%s\n", mdText(rr.Address), rr.Action, mdItalicModule(rr.Module))
			if len(rr.Attrs) == 0 {
				bw.printf("- _%s_\n\n", mdText(rr.summaryLine()))
				continue
			}
			for _, a := range rr.Attrs {
				bw.printf("- `%s`: `%s` → `%s`\n",
					mdText(a.Path), mdText(clip(render(a.Old), 200)), mdText(clip(render(a.New), 200)))
			}
			bw.printf("\n")
		}
	}

	if len(r.Pending) > 0 {
		bw.printf("### Pending changes from configuration\n\n")
		bw.printf("_%d to add, %d to change, %d to destroy_\n\n", add, chg, del)
		for _, rr := range r.Pending {
			bw.printf("- `%s` `%s`%s\n", sign(rr.Action), mdText(rr.Address), mdItalicModule(rr.Module))
		}
	}

	return bw.err
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
	RuleID              string            `json:"ruleId"`
	Level               string            `json:"level"`
	BaselineState       string            `json:"baselineState,omitempty"`
	Message             sarifText         `json:"message"`
	Locations           []sarifLocation   `json:"locations,omitempty"`
	RelatedLocations    []sarifLocation   `json:"relatedLocations,omitempty"`
	LogicalLocations    []sarifLogicalLoc `json:"logicalLocations,omitempty"`
	PartialFingerprints map[string]string `json:"partialFingerprints,omitempty"`
	Properties          map[string]string `json:"properties,omitempty"`
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
}

// WriteSARIF renders the report as a SARIF 2.1.0 log: one `resource-drift`
// result per resource changed outside Terraform, then one `deprecation` result
// per notice in r.Deprecations. src maps "<type>.<name>" to the .tf declaration
// (may be nil) for drift locations; deprecation locations come from the plan
// log itself. A clean report writes an empty results array.
func (r *Report) WriteSARIF(w io.Writer, src map[string]SourceLoc) error {
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
			RuleID:        "resource-drift",
			Level:         sarifLevel(rr.Action),
			BaselineState: sarifBaseline(rr.Action),
			Message:       sarifText{Text: rr.sarifMessage()},
			LogicalLocations: []sarifLogicalLoc{{
				FullyQualifiedName: rr.Address,
				Name:               resourceName(rr.Address),
				Kind:               "resource",
			}},
			PartialFingerprints: map[string]string{"driftAddress": rr.Address},
			Properties:          sarifProps(rr),
		}

		var loc sarifLocation
		if s, ok := src[rr.Type+"."+resourceName(rr.Address)]; ok {
			loc.PhysicalLocation.ArtifactLocation.URI = s.File
			loc.PhysicalLocation.Region = &sarifRegion{StartLine: s.Line}
		} else {
			loc.PhysicalLocation.ArtifactLocation.URI = rr.Address
		}
		res.Locations = []sarifLocation{loc}

		run.Results = append(run.Results, res)
	}

	for _, d := range r.Deprecations {
		res := sarifResult{
			RuleID:              "deprecation",
			Level:               sarifSeverityLevel(d.Severity),
			Message:             sarifText{Text: deprecationMessage(d)},
			PartialFingerprints: map[string]string{"deprecation": d.key()},
			Properties:          deprecationProps(d),
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

		run.Results = append(run.Results, res)
	}

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

// sarifBaseline reuses SARIF's baselineState vocabulary to describe the drift:
// a resource gone from the real world is "absent", a new one "new", an edited
// one "updated". Without it the Scans tab shows every row as "new".
func sarifBaseline(action string) string {
	switch action {
	case "delete":
		return "absent"
	case "create":
		return "new"
	default:
		return "updated"
	}
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
// (newlines flattened, clipped) on line 2, then — when more than one resource
// trips it — one "address (file:line)" line per site. The Scans tab renders
// message.text with white-space:pre-line, so the breaks show.
func deprecationMessage(d Deprecation) string {
	b := d.Summary
	if d.Detail != "" {
		b += "\n" + clip(strings.ReplaceAll(d.Detail, "\n", " "), 400)
	}
	if len(d.Sites) > 1 {
		for _, s := range d.Sites {
			b += "\n" + siteLabel(s)
		}
	}
	return b
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

	if len(r.Drift) == 0 {
		bw.printf("%stf-snag — no drift detected%s", c.green, c.reset)
		if len(r.Pending) > 0 {
			bw.printf(" (%d pending change(s) from configuration)", len(r.Pending))
		}
		bw.printf("\n")
		r.writeDeprecations(bw, c)
		return bw.err
	}

	head := fmt.Sprintf("tf-snag — %d resource(s) changed outside Terraform", len(r.Drift))
	if b := driftBreakdown(r.Drift); b != "" {
		head += "  (" + b + ")"
	}
	bw.printf("%s%s%s%s\n\n", c.bold, c.yellow, head, c.reset)

	for _, rr := range r.Drift {
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
	return bw.err
}

// writeDeprecations prints the deprecation section for the text report. No-op
// when the report carries none.
func (r *Report) writeDeprecations(bw *errWriter, c palette) {
	if len(r.Deprecations) == 0 {
		return
	}
	bw.printf("\n%sdeprecation warning(s): %d%s\n", c.dim, len(r.Deprecations), c.reset)
	for _, d := range r.Deprecations {
		bw.printf("  %s%s%s\n", c.yellow, d.Summary, c.reset)
		for _, s := range d.Sites {
			bw.printf("      %s%s%s\n", c.dim, siteLabel(s), c.reset)
		}
	}
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
