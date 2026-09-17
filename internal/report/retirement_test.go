package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wes-key/tf-snag/internal/retire"
)

func sampleFindings(t *testing.T) ([]retire.Finding, *retire.Catalogue) {
	t.Helper()
	c := &retire.Catalogue{
		Updated: retire.Date{Time: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		Source:  "built-in",
	}
	entry := retire.Entry{
		ID:          "azure-public-ip-basic-sku",
		Title:       "Basic SKU public IP addresses retire",
		RetiresOn:   retire.Date{Time: time.Date(2025, 9, 30, 0, 0, 0, 0, time.UTC)},
		URL:         "https://learn.microsoft.com/azure/pip",
		Severity:    "high",
		Remediation: "Move to Standard SKU.",
		Match:       retire.Match{Type: "azurerm_public_ip"},
	}
	return []retire.Finding{{
		Entry:   entry,
		Urgency: retire.Retired,
		Days:    -351,
		Instances: []retire.Instance{
			{Address: "azurerm_public_ip.bastion", Matched: map[string]any{"sku": "Basic"}},
			{Address: "module.network.azurerm_public_ip.egress[0]", Matched: map[string]any{"sku": "Basic"}},
		},
	}}, c
}

func withRetirements(t *testing.T) *Report {
	t.Helper()
	r := &Report{Schema: ReportSchema}
	findings, c := sampleFindings(t)
	r.AttachRetirements(findings, c, time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC))
	return r
}

func TestAttachRetirementsCarriesEntryAndInstances(t *testing.T) {
	r := withRetirements(t)
	if len(r.Retirements) != 1 {
		t.Fatalf("retirements = %d, want 1", len(r.Retirements))
	}
	rt := r.Retirements[0]
	if rt.ID != "azure-public-ip-basic-sku" || rt.RetiresOn != "2025-09-30" || rt.Urgency != "retired" {
		t.Errorf("entry not carried: %+v", rt)
	}
	if len(rt.Instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(rt.Instances))
	}
	if got := rt.Instances[1].Module; got != "module.network" {
		t.Errorf("module = %q, want module.network", got)
	}
	if got := rt.When(); got != "retired 351 days ago" {
		t.Errorf("When() = %q", got)
	}
	// 15 days from 2026-09-01 to 2026-09-16: fresh, so no staleness warning.
	if r.Catalogue == nil || r.Catalogue.AgeDays != 15 || r.Catalogue.Stale {
		t.Errorf("catalogue info = %+v, want 15 days and not stale", r.Catalogue)
	}
}

func TestCatalogueGoesStaleAfterAQuarter(t *testing.T) {
	r := &Report{}
	findings, c := sampleFindings(t)
	r.AttachRetirements(findings, c, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if !r.Catalogue.Stale {
		t.Errorf("catalogue %+v should be stale after %d days", r.Catalogue, StaleAfterDays)
	}
	var out bytes.Buffer
	if err := r.WriteText(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "may be missing newer notices") {
		t.Errorf("text report does not warn about the stale catalogue:\n%s", out.String())
	}
}

func TestRetirementGateWindow(t *testing.T) {
	r := withRetirements(t)
	// Already retired, so inside any window.
	if !r.HasGatingFindings() {
		t.Error("a retired finding should gate")
	}
	r.Retirements[0].Days = 400
	r.RetirementGateDays = 90
	if r.HasGatingFindings() {
		t.Error("a retirement 400 days out should not gate with -retirements-fail-within 90d")
	}
	r.RetirementGateDays = 0
	if !r.HasGatingFindings() {
		t.Error("with no window every un-suppressed retirement gates")
	}
	r.Retirements[0].Suppressed = true
	if r.HasGatingFindings() {
		t.Error("a suppressed retirement must not gate")
	}
}

func TestWriteTextShowsRetirements(t *testing.T) {
	var out bytes.Buffer
	if err := withRetirements(t).WriteText(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"retirement(s): 1",
		"azurerm_public_ip",
		"Basic SKU public IP addresses retire (2025-09-30)",
		"azurerm_public_ip.bastion",
		"retired 351 days ago",
		"Move to Standard SKU.",
		"https://learn.microsoft.com/azure/pip",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("text report missing %q:\n%s", want, got)
		}
	}
}

func TestWriteJSONCarriesRetirementsAtSchema3(t *testing.T) {
	var out bytes.Buffer
	if err := withRetirements(t).WriteJSON(&out); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Schema      int `json:"schema"`
		Retirements []struct {
			ID        string `json:"id"`
			Urgency   string `json:"urgency"`
			Days      int    `json:"days"`
			Instances []struct {
				Address string         `json:"address"`
				Matched map[string]any `json:"matched"`
			} `json:"instances"`
		} `json:"retirements"`
		Catalogue struct {
			Source  string `json:"source"`
			Updated string `json:"updated"`
		} `json:"retirement_catalogue"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if doc.Schema != 3 {
		t.Errorf("schema = %d, want 3 (retirements are a schema change)", doc.Schema)
	}
	if len(doc.Retirements) != 1 || doc.Retirements[0].ID != "azure-public-ip-basic-sku" {
		t.Fatalf("retirements not carried: %s", out.String())
	}
	if doc.Retirements[0].Instances[0].Matched["sku"] != "Basic" {
		t.Errorf("matched attribute not carried: %+v", doc.Retirements[0].Instances[0])
	}
	if doc.Catalogue.Source != "built-in" || doc.Catalogue.Updated != "2026-09-01" {
		t.Errorf("catalogue info not carried: %+v", doc.Catalogue)
	}
}

func TestWriteSARIFEmitsRetirementResults(t *testing.T) {
	var out bytes.Buffer
	if err := withRetirements(t).WriteSARIF(&out, nil, nil); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Runs []struct {
			Results []struct {
				RuleID           string                `json:"ruleId"`
				Level            string                `json:"level"`
				Message          struct{ Text string } `json:"message"`
				Properties       map[string]string     `json:"properties"`
				LogicalLocations []struct {
					FullyQualifiedName string `json:"fullyQualifiedName"`
				} `json:"logicalLocations"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Runs[0].Results) != 1 {
		t.Fatalf("results = %d, want 1", len(doc.Runs[0].Results))
	}
	res := doc.Runs[0].Results[0]
	if res.RuleID != "retirement" {
		t.Errorf("ruleId = %q, want retirement", res.RuleID)
	}
	// Past its date, so it reads as an error rather than a warning.
	if res.Level != "error" {
		t.Errorf("level = %q, want error for a retirement already past", res.Level)
	}
	if res.Properties["retiresOn"] != "2025-09-30" || res.Properties["count"] != "2" {
		t.Errorf("properties = %+v", res.Properties)
	}
	if len(res.LogicalLocations) != 2 {
		t.Errorf("logical locations = %d, want one per affected resource", len(res.LogicalLocations))
	}
}

func TestTeamsCardCarriesRetirements(t *testing.T) {
	var out bytes.Buffer
	if err := withRetirements(t).WriteTeams(&out, TeamsOptions{}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Retirements") {
		t.Errorf("card has no retirements group:\n%s", got)
	}
	if !strings.Contains(got, "1 (1 already retired)") {
		t.Errorf("card does not call out the retired count:\n%s", got)
	}
}

func TestTeamsCardOmitsRetirementFactWhenCheckDidNotRun(t *testing.T) {
	var out bytes.Buffer
	if err := (&Report{}).WriteTeams(&out, TeamsOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Retirements") {
		t.Errorf("a run that never checked retirements must not claim a count:\n%s", out.String())
	}
}

func TestFailWindowNarrowsTheGateButNotTheReport(t *testing.T) {
	r := withRetirements(t)
	r.RetirementGateDays = 90
	r.Retirements[0].Days = 838
	r.Retirements[0].BeyondFailWindow = r.Retirements[0].Deferred(r.RetirementGateDays)

	if r.HasGatingFindings() {
		t.Error("a retirement beyond the window must not fail the run")
	}
	// ...but it is still news, so every surface still carries it.
	var text bytes.Buffer
	if err := r.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "beyond the 90-day fail window") {
		t.Errorf("text report drops the deferred retirement:\n%s", text.String())
	}
	var md bytes.Buffer
	if err := r.WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "beyond the fail window") {
		t.Errorf("markdown drops the deferred retirement:\n%s", md.String())
	}
	var junit bytes.Buffer
	if err := r.WriteJUnit(&junit); err != nil {
		t.Fatal(err)
	}
	// Skipped, not failed: the Tests tab should not show a red case for
	// something the pipeline deliberately does not gate on.
	if !strings.Contains(junit.String(), "beyond the 90-day fail window") {
		t.Errorf("junit drops the deferred retirement:\n%s", junit.String())
	}
	if strings.Count(junit.String(), "<failure") != 0 {
		t.Errorf("junit marks a deferred retirement as a failure:\n%s", junit.String())
	}
}

// A retirement that has just appeared is newly actionable. Leaving retirements
// out of this is what made -teams-notify new and -pr-comment new silent about a
// resource that had just landed on a published retirement notice.
func TestHasNewlyActionableIncludesRetirements(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   Retirement
		want bool
	}{
		{"new", Retirement{ID: "a", BaselineState: "new"}, true},
		{"no longer ignored", Retirement{ID: "a", Unsuppressed: true}, true},
		{"carried over", Retirement{ID: "a", BaselineState: "updated"}, false},
		{"still ignored", Retirement{ID: "a", BaselineState: "new", Suppressed: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := &Report{Retirements: []Retirement{tc.rt}}
			if got := rep.HasNewlyActionable(); got != tc.want {
				t.Errorf("HasNewlyActionable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A retirement has to be stamped against the baseline like anything else.
// Without it the fields existed and were never set, so "new" was unreachable:
// no Teams card, no pull request comment, no work item under -ado-raise new.
func TestStampReportMarksRetirementsNewAndCarriedOver(t *testing.T) {
	prev := `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"tf-snag","rules":[]}},"results":[{` +
		`"ruleId":"retirement","guid":"` + (Retirement{ID: "azure-lb-basic-sku"}).FindingID() + `",` +
		`"level":"error","message":{"text":"Basic SKU load balancers retire"},` +
		`"provenance":{"firstDetectionTimeUtc":"2026-01-02T03:04:05Z"}}]}]}`
	prior, err := ParsePriorSARIF([]byte(prev))
	if err != nil {
		t.Fatalf("ParsePriorSARIF: %v", err)
	}

	rep := &Report{Retirements: []Retirement{
		{ID: "azure-lb-basic-sku"},           // in the baseline
		{ID: "azure-vpn-gateway-legacy-sku"}, // not in the baseline
	}}
	prior.StampReport(rep)

	if got := rep.Retirements[0].BaselineState; got != "updated" {
		t.Errorf("carried-over retirement state = %q, want updated", got)
	}
	if got := rep.Retirements[0].FirstSeen; got != "2026-01-02T03:04:05Z" {
		t.Errorf("first seen = %q, want the prior run's timestamp", got)
	}
	if got := rep.Retirements[1].BaselineState; got != "new" {
		t.Errorf("unmatched retirement state = %q, want new", got)
	}
	if rep.Retirements[1].FirstSeen == "" {
		t.Error("a new retirement should be stamped with a first-seen time")
	}
	if !rep.HasNewlyActionable() {
		t.Error("a new retirement is newly actionable: this is what -pr-comment new asks")
	}
}
