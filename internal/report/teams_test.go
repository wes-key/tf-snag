package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wes-key/tf-snag/internal/plan"
)

// acEl mirrors an Adaptive Card element, deep enough to walk Containers and
// ColumnSets.
type acEl struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Text      string `json:"text"`
	Color     string `json:"color"`
	Style     string `json:"style"`
	Weight    string `json:"weight"`
	IsVisible *bool  `json:"isVisible"`
	Facts     []struct {
		Title string `json:"title"`
		Value string `json:"value"`
	} `json:"facts"`
	Items   []acEl `json:"items"`
	Columns []struct {
		Items []acEl `json:"items"`
	} `json:"columns"`
	SelectAction *struct {
		Type           string   `json:"type"`
		TargetElements []string `json:"targetElements"`
	} `json:"selectAction"`
}

// teamsDoc mirrors the parts of the payload the tests assert on.
type teamsDoc struct {
	Type        string `json:"type"`
	Attachments []struct {
		ContentType string `json:"contentType"`
		Content     struct {
			Type    string `json:"type"`
			Version string `json:"version"`
			Body    []acEl `json:"body"`
			Actions []struct {
				Type  string `json:"type"`
				Title string `json:"title"`
				URL   string `json:"url"`
			} `json:"actions"`
			MSTeams struct {
				Width string `json:"width"`
			} `json:"msteams"`
		} `json:"content"`
	} `json:"attachments"`
}

// walk visits every element in the card, descending into Containers and
// ColumnSet columns.
func walk(els []acEl, fn func(acEl)) {
	for _, e := range els {
		fn(e)
		walk(e.Items, fn)
		for _, c := range e.Columns {
			walk(c.Items, fn)
		}
	}
}

func allEls(doc teamsDoc) []acEl {
	var out []acEl
	walk(doc.Attachments[0].Content.Body, func(e acEl) { out = append(out, e) })
	return out
}

// elByID finds an element by its Adaptive Card id.
func elByID(doc teamsDoc, id string) (acEl, bool) {
	for _, e := range allEls(doc) {
		if e.ID == id {
			return e, true
		}
	}
	return acEl{}, false
}

func parseTeams(t *testing.T, r *Report, opts TeamsOptions) (teamsDoc, string) {
	t.Helper()
	var buf bytes.Buffer
	if err := r.WriteTeams(&buf, opts); err != nil {
		t.Fatal(err)
	}
	var doc teamsDoc
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("payload is not valid JSON: %v\n%s", err, buf.String())
	}
	return doc, buf.String()
}

func mustParseTeams(t *testing.T, r *Report) teamsDoc {
	t.Helper()
	doc, _ := parseTeams(t, r, TeamsOptions{})
	return doc
}

// bodyText is every TextBlock in the card, joined and un-escaped — i.e. roughly
// what a reader sees once Teams has applied the Markdown. Assertions are written
// against that rather than the on-the-wire text, which is full of backslashes.
func bodyText(doc teamsDoc) string {
	var b strings.Builder
	walk(doc.Attachments[0].Content.Body, func(e acEl) {
		if e.Text != "" {
			b.WriteString(e.Text)
			b.WriteString("\n")
		}
	})
	return strings.NewReplacer(`\*`, "*", `\_`, "_", `\[`, "[", `\]`, "]").Replace(b.String())
}

// headline is the first TextBlock in the banner container.
func headline(doc teamsDoc) acEl {
	var out acEl
	walk(doc.Attachments[0].Content.Body, func(e acEl) {
		if out.Text == "" && e.Type == "TextBlock" {
			out = e
		}
	})
	return out
}

func factsOf(doc teamsDoc) map[string]string {
	m := map[string]string{}
	walk(doc.Attachments[0].Content.Body, func(e acEl) {
		for _, f := range e.Facts {
			m[f.Title] = f.Value
		}
	})
	return m
}

func TestWriteTeamsEnvelope(t *testing.T) {
	doc, raw := parseTeams(t, Build(deletedVaultPlan()), TeamsOptions{})

	if doc.Type != "message" {
		t.Errorf("type = %q, want message (the Workflows envelope)", doc.Type)
	}
	if len(doc.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(doc.Attachments))
	}
	att := doc.Attachments[0]
	if att.ContentType != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("contentType = %q", att.ContentType)
	}
	if att.Content.Type != "AdaptiveCard" || att.Content.Version != teamsCardVersion {
		t.Errorf("card = %s/%s, want AdaptiveCard/%s", att.Content.Type, att.Content.Version, teamsCardVersion)
	}
	if att.Content.MSTeams.Width != "Full" {
		t.Errorf("msteams.width = %q, want Full", att.Content.MSTeams.Width)
	}
	// contentUrl must be present-and-null; Workflows rejects the attachment
	// without the key.
	if !strings.Contains(raw, `"contentUrl": null`) {
		t.Errorf("payload missing an explicit null contentUrl:\n%s", raw)
	}
}

// The verdict is carried by the tint of the banner container, not by colouring
// the headline text (which would be unreadable on the tint).
func TestWriteTeamsHeadlineAndBannerStyle(t *testing.T) {
	cases := []struct {
		name     string
		report   *Report
		headline string
		style    string
	}{
		{
			name:     "clean",
			report:   Build(&plan.Plan{TerraformVersion: "1.9.6"}),
			headline: "No drift or deprecations",
			style:    "good",
		},
		{
			name:     "drift only",
			report:   Build(deletedVaultPlan()),
			headline: "1 resource changed outside Terraform",
			style:    "attention",
		},
		{
			name: "deprecations only",
			report: &Report{Deprecations: []Deprecation{
				{Severity: "warning", Summary: "Argument is deprecated"},
				{Severity: "warning", Summary: "Deprecated resource"},
			}},
			headline: "2 deprecation warnings",
			style:    "warning",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, _ := parseTeams(t, tc.report, TeamsOptions{})

			banner := doc.Attachments[0].Content.Body[0]
			if banner.Type != "Container" || banner.Style != tc.style {
				t.Errorf("banner = %s/%q, want Container/%q", banner.Type, banner.Style, tc.style)
			}
			if head := headline(doc); head.Text != tc.headline {
				t.Errorf("headline = %q, want %q", head.Text, tc.headline)
			}
			if head := headline(doc); head.Color != "" {
				t.Errorf("headline colour = %q, want none (the container tint is the signal)", head.Color)
			}
		})
	}
}

// Each kind gets a group that is collapsed on arrival and expands via
// Action.ToggleVisibility.
func TestWriteTeamsGroupsCollapsedByDefault(t *testing.T) {
	doc, _ := parseTeams(t, suppressedReport(), TeamsOptions{})

	for _, id := range []string{"driftBody", "deprBody"} {
		body, ok := elByID(doc, id)
		if !ok {
			t.Fatalf("no %q container — groups missing", id)
		}
		if body.IsVisible == nil || *body.IsVisible {
			t.Errorf("%s should start hidden, isVisible = %v", id, body.IsVisible)
		}
	}

	// The header toggles the body and both chevrons in one action.
	var toggles int
	walk(doc.Attachments[0].Content.Body, func(e acEl) {
		if e.SelectAction == nil {
			return
		}
		toggles++
		if e.SelectAction.Type != "Action.ToggleVisibility" {
			t.Errorf("selectAction = %q", e.SelectAction.Type)
		}
		if len(e.SelectAction.TargetElements) != 3 {
			t.Errorf("targetElements = %v, want body + both chevrons", e.SelectAction.TargetElements)
		}
		if e.Style != "emphasis" {
			t.Errorf("group header style = %q, want emphasis", e.Style)
		}
	})
	if toggles != 2 {
		t.Errorf("toggle headers = %d, want 2 (drift + deprecations)", toggles)
	}

	// Collapsed chevron shown, expanded one hidden.
	if shut, _ := elByID(doc, "driftShut"); shut.IsVisible == nil || !*shut.IsVisible {
		t.Error("collapsed chevron should be visible to start")
	}
	if open, _ := elByID(doc, "driftOpen"); open.IsVisible == nil || *open.IsVisible {
		t.Error("expanded chevron should be hidden to start")
	}
}

// A clean report has no groups at all — nothing to collapse.
func TestWriteTeamsCleanReportHasNoGroups(t *testing.T) {
	doc, _ := parseTeams(t, Build(&plan.Plan{TerraformVersion: "1.9.6"}), TeamsOptions{})
	if _, ok := elByID(doc, "driftBody"); ok {
		t.Error("clean report should not carry a drift group")
	}
	if _, ok := elByID(doc, "deprBody"); ok {
		t.Error("clean report should not carry a deprecations group")
	}
}

// The sign glyph is coloured per action so the group reads at a glance.
func TestWriteTeamsColoursActionGlyphs(t *testing.T) {
	r := Build(&plan.Plan{ResourceDrift: []plan.ResourceChange{
		driftUpdate("azurerm_x.upd", map[string]any{"v": 1}, map[string]any{"v": 2}),
	}})
	r.Drift = append(r.Drift, ResourceReport{Address: "azurerm_x.gone", Action: "delete"})
	r.Drift = append(r.Drift, ResourceReport{Address: "azurerm_x.made", Action: "create"})

	doc, _ := parseTeams(t, r, TeamsOptions{})
	got := map[string]string{}
	walk(doc.Attachments[0].Content.Body, func(e acEl) {
		if e.Color != "" && e.Text != "" {
			got[e.Text] = e.Color
		}
	})
	want := map[string]string{sign("update"): "Warning", sign("delete"): "Attention", sign("create"): "Good"}
	for glyph, colour := range want {
		if got[glyph] != colour {
			t.Errorf("glyph %q colour = %q, want %q (all: %v)", glyph, got[glyph], colour, got)
		}
	}
}

func TestWriteTeamsFactsAndContext(t *testing.T) {
	r := Build(deletedVaultPlan())
	doc, _ := parseTeams(t, r, TeamsOptions{
		Context: "nightly-drift · main · run 20260901.1",
		RunURL:  "https://dev.azure.com/wes-key/p/_build/results?buildId=42",
	})

	if got := bodyText(doc); !strings.Contains(got, "nightly-drift · main · run 20260901.1") {
		t.Errorf("context line missing:\n%s", got)
	}

	facts := factsOf(doc)
	if !strings.HasPrefix(facts["Drift"], "1") {
		t.Errorf("Drift fact = %q", facts["Drift"])
	}
	if facts["Deprecations"] != "0" {
		t.Errorf("Deprecations fact = %q, want 0", facts["Deprecations"])
	}
	if _, ok := facts["Ignored"]; ok {
		t.Error("Ignored fact should be omitted when nothing is suppressed")
	}

	acts := doc.Attachments[0].Content.Actions
	if len(acts) != 1 || acts[0].Type != "Action.OpenUrl" || acts[0].Title != "View run" {
		t.Fatalf("actions = %+v, want one Action.OpenUrl", acts)
	}
	if acts[0].URL != "https://dev.azure.com/wes-key/p/_build/results?buildId=42" {
		t.Errorf("action url = %q", acts[0].URL)
	}
}

func TestWriteTeamsNoRunURLNoAction(t *testing.T) {
	doc, _ := parseTeams(t, Build(deletedVaultPlan()), TeamsOptions{})
	if len(doc.Attachments[0].Content.Actions) != 0 {
		t.Errorf("actions = %+v, want none without a RunURL", doc.Attachments[0].Content.Actions)
	}
}

// Suppressed findings stay off the card's headline, counts and listings; they
// are only acknowledged by the "Ignored" fact. suppressedReport has one
// suppressed and one live finding of each kind.
func TestWriteTeamsExcludesSuppressed(t *testing.T) {
	doc, _ := parseTeams(t, suppressedReport(), TeamsOptions{})

	head := headline(doc).Text
	if head != "1 resource changed outside Terraform, 1 deprecation" {
		t.Errorf("headline = %q, want only the un-suppressed findings counted", head)
	}
	facts := factsOf(doc)
	if !strings.HasPrefix(facts["Drift"], "1") || facts["Deprecations"] != "1" {
		t.Errorf("suppressed findings leaked into the counts: %+v", facts)
	}
	if !strings.HasPrefix(facts["Ignored"], "2") {
		t.Errorf("Ignored fact = %q, want 2", facts["Ignored"])
	}

	body := bodyText(doc)
	if strings.Contains(body, "azurerm_storage_account.data") {
		t.Errorf("suppressed resource listed on the card:\n%s", body)
	}
	if strings.Contains(body, "Argument is deprecated") {
		t.Errorf("suppressed deprecation listed on the card:\n%s", body)
	}
	if !strings.Contains(body, "azurerm_key_vault.vault") {
		t.Errorf("live drift missing from the card:\n%s", body)
	}
}

func TestWriteTeamsCapsItems(t *testing.T) {
	var drift []plan.ResourceChange
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		drift = append(drift, driftUpdate("azurerm_x."+n, map[string]any{"v": 1}, map[string]any{"v": 2}))
	}
	r := Build(&plan.Plan{ResourceDrift: drift})

	doc, _ := parseTeams(t, r, TeamsOptions{MaxItems: 2})
	body := bodyText(doc)
	if !strings.Contains(body, "…and 3 more resources") {
		t.Errorf("missing the truncation line:\n%s", body)
	}
	// The headline still counts everything, only the listing is capped.
	if head := headline(doc).Text; !strings.Contains(head, "5 resources") {
		t.Errorf("headline = %q, want the full count", head)
	}
}

// A resource address containing Markdown must not break the TextBlock: Teams
// honours **bold** / _italic_ / [links] inside one.
func TestTeamsTextEscapesMarkdown(t *testing.T) {
	got := teamsText(`module.a_b["*x*"].res[0]`)
	for _, c := range []string{"*", "_", "[", "]"} {
		if strings.Contains(strings.ReplaceAll(got, "\\"+c, ""), c) {
			t.Errorf("teamsText(%q) left an unescaped %q: %q", `module.a_b["*x*"].res[0]`, c, got)
		}
	}
	if strings.Contains(teamsText("a\nb"), "\n") {
		t.Error("teamsText should flatten newlines")
	}
}

func TestWriteTeamsListsDeprecationSites(t *testing.T) {
	r := &Report{Deprecations: []Deprecation{{
		Severity: "warning",
		Summary:  "Argument is deprecated",
		Detail:   "The \"foo\" argument is deprecated. Use \"bar\" instead.",
		Sites: []DeprecationSite{
			{Address: "azurerm_x.y", File: "main.tf", Line: 5},
			{Address: "azurerm_x.z", File: "main.tf", Line: 9},
		},
	}}}
	doc, _ := parseTeams(t, r, TeamsOptions{})
	body := bodyText(doc)
	for _, want := range []string{"Argument is deprecated", "azurerm_x.y", "azurerm_x.z"} {
		if !strings.Contains(body, want) {
			t.Errorf("card missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, `The "foo" argument is deprecated.`) {
		t.Errorf("detail missing:\n%s", body)
	}
}

// A long detail is cut to its first sentence so one deprecation cannot swamp
// the card.
func TestWriteTeamsTrimsLongDetail(t *testing.T) {
	long := "This argument has been deprecated since provider version 3.0 and will be removed. " +
		"Migrate to the replacement block before upgrading to 6.0."
	r := &Report{Deprecations: []Deprecation{{
		Severity: "warning", Summary: "Argument is deprecated", Detail: long,
		Sites: []DeprecationSite{{Address: "azurerm_x.y"}},
	}}}
	body := bodyText(mustParseTeams(t, r))
	if !strings.Contains(body, "deprecated since provider version 3.0 and will be removed.") {
		t.Errorf("first sentence missing:\n%s", body)
	}
	if strings.Contains(body, "Migrate to the replacement block") {
		t.Errorf("detail not trimmed to the first sentence:\n%s", body)
	}
}
