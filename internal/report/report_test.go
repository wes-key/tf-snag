package report

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"regexp"
	"strings"
	"testing"

	"github.com/wes-key/tf-snag/internal/plan"
)

func samplePlan() *plan.Plan {
	return &plan.Plan{
		TerraformVersion: "1.9.6",
		ResourceDrift: []plan.ResourceChange{{
			Address:       "azurerm_storage_account.data",
			ModuleAddress: "module.storage",
			Change: plan.Change{
				Actions: []string{"update"},
				Before:  map[string]any{"min_tls_version": "TLS1_2"},
				After:   map[string]any{"min_tls_version": "TLS1_0"},
			},
		}},
		ResourceChanges: []plan.ResourceChange{
			{Address: "azurerm_resource_group.new", Change: plan.Change{
				Actions: []string{"create"}, After: map[string]any{"name": "rg"}}},
			{Address: "azurerm_subnet.old", Change: plan.Change{Actions: []string{"no-op"}}},
			{Address: "data.azurerm_client_config.current", Change: plan.Change{Actions: []string{"read"}}},
		},
	}
}

func TestBuildSeparatesDriftFromPendingAndSkipsNoOpAndRead(t *testing.T) {
	r := Build(samplePlan())

	if len(r.Drift) != 1 {
		t.Fatalf("Drift = %d, want 1", len(r.Drift))
	}
	if len(r.Drift[0].Attrs) != 1 || r.Drift[0].Attrs[0].Path != "min_tls_version" {
		t.Errorf("drift attrs = %+v", r.Drift[0].Attrs)
	}
	if len(r.Pending) != 1 || r.Pending[0].Action != "create" {
		t.Fatalf("Pending = %+v, want a single create", r.Pending)
	}
	if r.Pending[0].Attrs != nil {
		t.Errorf("create should carry no attr diff, got %+v", r.Pending[0].Attrs)
	}
	if !r.HasDrift() {
		t.Error("HasDrift() = false, want true")
	}
}

func TestBuildDropsEmptyishOnlyDrift(t *testing.T) {
	p := &plan.Plan{
		TerraformVersion: "1.9.6",
		ResourceDrift: []plan.ResourceChange{
			{ // provider refresh round-trip, not real drift
				Address: "azurerm_signalr_service.drift_check",
				Type:    "azurerm_signalr_service",
				Change: plan.Change{
					Actions: []string{"update"},
					Before:  map[string]any{"tags": nil},
					After:   map[string]any{"tags": map[string]any{}},
				},
			},
			{ // genuine drift, must survive
				Address: "azurerm_storage_account.data",
				Type:    "azurerm_storage_account",
				Change: plan.Change{
					Actions: []string{"update"},
					Before:  map[string]any{"tags": nil, "min_tls_version": "TLS1_2"},
					After:   map[string]any{"tags": map[string]any{}, "min_tls_version": "TLS1_0"},
				},
			},
		},
	}
	r := Build(p)
	if len(r.Drift) != 1 || r.Drift[0].Address != "azurerm_storage_account.data" {
		t.Fatalf("Drift = %+v, want only the storage account", r.Drift)
	}
	if len(r.Drift[0].Attrs) != 1 || r.Drift[0].Attrs[0].Path != "min_tls_version" {
		t.Errorf("surviving drift attrs = %+v", r.Drift[0].Attrs)
	}
}

func TestWriteTextShowsChangedAttributeAndTally(t *testing.T) {
	var buf bytes.Buffer
	if err := Build(samplePlan()).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"1 resource(s) changed outside Terraform",
		"~ azurerm_storage_account.data  (module.storage)",
		`min_tls_version  "TLS1_2" => "TLS1_0"`,
		"1 to add, 0 to change, 0 to destroy",
		"+ azurerm_resource_group.new",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain WriteText leaked ANSI codes:\n%s", out)
	}
}

func TestWriteTextColorEmitsANSIAndSameContent(t *testing.T) {
	var plainBuf, colorBuf bytes.Buffer
	if err := Build(samplePlan()).WriteText(&plainBuf); err != nil {
		t.Fatal(err)
	}
	if err := Build(samplePlan()).WriteTextColor(&colorBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(colorBuf.String(), "\x1b[") {
		t.Error("WriteTextColor emitted no ANSI codes")
	}
	// Stripping ANSI from the coloured output must reproduce the plain output.
	stripped := ansiStrip.ReplaceAllString(colorBuf.String(), "")
	if stripped != plainBuf.String() {
		t.Errorf("stripped colour output != plain\n--- stripped ---\n%s\n--- plain ---\n%s", stripped, plainBuf.String())
	}
}

var ansiStrip = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestWriteTextNoDrift(t *testing.T) {
	var buf bytes.Buffer
	p := &plan.Plan{
		TerraformVersion: "1.9.6",
		ResourceChanges: []plan.ResourceChange{
			{Address: "azurerm_resource_group.new", Change: plan.Change{Actions: []string{"create"}}},
		},
	}
	if err := Build(p).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "no drift detected") ||
		!strings.Contains(got, "1 pending change(s)") {
		t.Errorf("unexpected output: %q", got)
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := Build(samplePlan()).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if back.Schema != ReportSchema {
		t.Errorf("schema = %d, want %d", back.Schema, ReportSchema)
	}
	if len(back.Drift) != 1 || back.Drift[0].Address != "azurerm_storage_account.data" {
		t.Errorf("round-tripped drift = %+v", back.Drift)
	}
}

func TestWriteJUnitOneFailurePerDriftedResource(t *testing.T) {
	var buf bytes.Buffer
	if err := Build(samplePlan()).WriteJUnit(&buf); err != nil {
		t.Fatal(err)
	}
	var suites struct {
		Suites []struct {
			Name     string `xml:"name,attr"`
			Tests    int    `xml:"tests,attr"`
			Failures int    `xml:"failures,attr"`
			Cases    []struct {
				Name    string `xml:"name,attr"`
				Failure *struct {
					Message string `xml:"message,attr"`
					Body    string `xml:",chardata"`
				} `xml:"failure"`
			} `xml:"testcase"`
		} `xml:"testsuite"`
	}
	if err := xml.Unmarshal(buf.Bytes(), &suites); err != nil {
		t.Fatalf("not valid JUnit XML: %v\n%s", err, buf.String())
	}
	if len(suites.Suites) != 1 {
		t.Fatalf("suites = %d, want 1", len(suites.Suites))
	}
	s := suites.Suites[0]
	if s.Name != "tf-snag" || s.Tests != 1 || s.Failures != 1 {
		t.Errorf("suite = %+v, want tf-snag tests=1 failures=1", s)
	}
	if len(s.Cases) != 1 || s.Cases[0].Failure == nil {
		t.Fatalf("cases = %+v, want one failing case", s.Cases)
	}
	if !strings.Contains(s.Cases[0].Name, "azurerm_storage_account.data") {
		t.Errorf("case name = %q", s.Cases[0].Name)
	}
	if !strings.Contains(s.Cases[0].Failure.Body, `min_tls_version: "TLS1_2" => "TLS1_0"`) {
		t.Errorf("failure body = %q", s.Cases[0].Failure.Body)
	}
}

func TestWriteJUnitCleanReportIsGreen(t *testing.T) {
	var buf bytes.Buffer
	p := &plan.Plan{TerraformVersion: "1.9.6"}
	if err := Build(p).WriteJUnit(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `failures="0"`) || !strings.Contains(out, "no drift detected") {
		t.Errorf("clean JUnit should have a single passing case:\n%s", out)
	}
}

func TestWriteMarkdownShowsDriftAndPending(t *testing.T) {
	var buf bytes.Buffer
	if err := Build(samplePlan()).WriteMarkdown(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.HasPrefix(out, "## tf-snag") {
		t.Errorf("markdown should not start with a redundant '## tf-snag' heading:\n%s", out)
	}
	for _, want := range []string{
		"🔴 **1 resource changed outside Terraform**",
		"_Terraform 1.9.6 · pending: 1 to add, 0 to change, 0 to destroy_",
		"### Changed outside Terraform",
		"**`azurerm_storage_account.data`** · update _(module.storage)_",
		"- `min_tls_version`: `\"TLS1_2\"` → `\"TLS1_0\"`",
		"### Pending changes from configuration",
		"- `+` `azurerm_resource_group.new`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "|---") || strings.Contains(out, "| Resource |") {
		t.Errorf("markdown contains a pipe table (does not render in the ADO summary):\n%s", out)
	}
}

func TestWriteMarkdownNeutralisesBacktick(t *testing.T) {
	p := &plan.Plan{
		TerraformVersion: "1.9.6",
		ResourceDrift: []plan.ResourceChange{{
			Address: "azurerm_storage_account.data",
			Change: plan.Change{
				Actions: []string{"update"},
				Before:  map[string]any{"note": "was `raw`"},
				After:   map[string]any{"note": "now plain"},
			},
		}},
	}
	var buf bytes.Buffer
	if err := Build(p).WriteMarkdown(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "was `raw`") {
		t.Errorf("backtick in value not neutralised:\n%s", buf.String())
	}
}

func deletedVaultPlan() *plan.Plan {
	return &plan.Plan{
		TerraformVersion: "1.9.6",
		ResourceDrift: []plan.ResourceChange{{
			Address:       "module.key_vault[0].azurerm_key_vault.vault",
			Type:          "azurerm_key_vault",
			ModuleAddress: "module.key_vault[0]",
			Change: plan.Change{
				Actions: []string{"delete"},
				Before: map[string]any{
					"name":                   "tfsnagdevukschksa01",
					"id":                     "/subscriptions/x/vaults/tfsnagdevukschksa01",
					"sku_name":               "standard",
					"enabled_for_deployment": false,
					"tags":                   map[string]any{},
				},
				After: nil,
			},
		}},
	}
}

func TestDeleteDriftIsSummarisedNotDumped(t *testing.T) {
	rep := Build(deletedVaultPlan())

	if len(rep.Drift) != 1 || rep.Drift[0].Action != "delete" {
		t.Fatalf("drift = %+v", rep.Drift)
	}
	if len(rep.Drift[0].Attrs) != 0 {
		t.Errorf("delete drift carried %d attribute diffs, want 0", len(rep.Drift[0].Attrs))
	}
	if rep.Drift[0].Identity != "name=tfsnagdevukschksa01" {
		t.Errorf("identity = %q", rep.Drift[0].Identity)
	}

	var text, md, ju bytes.Buffer
	_ = rep.WriteText(&text)
	_ = rep.WriteMarkdown(&md)
	_ = rep.WriteJUnit(&ju)

	for name, out := range map[string]string{"text": text.String(), "markdown": md.String(), "junit": ju.String()} {
		if !strings.Contains(out, "deleted outside Terraform") {
			t.Errorf("%s: missing summary line\n%s", name, out)
		}
		if !strings.Contains(out, "tfsnagdevukschksa01") {
			t.Errorf("%s: missing identity\n%s", name, out)
		}
		if strings.Contains(out, "sku_name") || strings.Contains(out, "=> null") || strings.Contains(out, "→ `null`") {
			t.Errorf("%s: still dumping null attributes\n%s", name, out)
		}
	}
}

func TestDriftHeaderBreakdown(t *testing.T) {
	p := deletedVaultPlan()
	p.ResourceDrift = append(p.ResourceDrift, plan.ResourceChange{
		Address: "azurerm_storage_account.data",
		Change: plan.Change{
			Actions: []string{"update"},
			Before:  map[string]any{"min_tls_version": "TLS1_2"},
			After:   map[string]any{"min_tls_version": "TLS1_0"},
		},
	})
	var buf bytes.Buffer
	if err := Build(p).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "2 resource(s) changed outside Terraform  (1 deleted, 1 updated)") {
		t.Errorf("missing breakdown:\n%s", buf.String())
	}
}

func TestClipTruncatesLongValues(t *testing.T) {
	long := strings.Repeat("x", 300)
	p := &plan.Plan{
		TerraformVersion: "1.9.6",
		ResourceDrift: []plan.ResourceChange{{
			Address: "azurerm_storage_account.data",
			Change: plan.Change{
				Actions: []string{"update"},
				Before:  map[string]any{"blob": "short"},
				After:   map[string]any{"blob": long},
			},
		}},
	}
	var buf bytes.Buffer
	if err := Build(p).WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), long) || !strings.Contains(buf.String(), "…") {
		t.Errorf("long value not clipped:\n%s", buf.String())
	}
}

type sarifTestLoc struct {
	PhysicalLocation struct {
		ArtifactLocation struct{ URI string } `json:"artifactLocation"`
		Region           *struct {
			StartLine int `json:"startLine"`
		} `json:"region"`
	} `json:"physicalLocation"`
}

type sarifDoc struct {
	Version string `json:"version"`
	Runs    []struct {
		Tool struct {
			Driver struct {
				Name  string `json:"name"`
				Rules []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"rules"`
			} `json:"driver"`
		} `json:"tool"`
		Results []struct {
			RuleID        string `json:"ruleId"`
			GUID          string `json:"guid"`
			Level         string `json:"level"`
			BaselineState string `json:"baselineState"`
			Message       struct {
				Text     string `json:"text"`
				Markdown string `json:"markdown"` // must stay absent
			} `json:"message"`
			Locations        []sarifTestLoc `json:"locations"`
			RelatedLocations []sarifTestLoc `json:"relatedLocations"`
			LogicalLocations []struct {
				FullyQualifiedName string `json:"fullyQualifiedName"`
			} `json:"logicalLocations"`
			PartialFingerprints map[string]string `json:"partialFingerprints"`
			Properties          map[string]string `json:"properties"`
		} `json:"results"`
	} `json:"runs"`
}

func parseSARIF(t *testing.T, b []byte) sarifDoc {
	t.Helper()
	var d sarifDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("not valid SARIF JSON: %v\n%s", err, b)
	}
	return d
}

func TestWriteSARIFResultShape(t *testing.T) {
	p := deletedVaultPlan()
	p.ResourceDrift = append(p.ResourceDrift, plan.ResourceChange{
		Address:       "module.resource_group[0].azurerm_resource_group.rg",
		Type:          "azurerm_resource_group",
		ModuleAddress: "module.resource_group[0]",
		Change: plan.Change{
			Actions: []string{"update"},
			Before:  map[string]any{"tags": map[string]any{}},
			After:   map[string]any{"tags": map[string]any{"Owner": "Wes", "Test": "Test"}},
		},
	})

	var buf bytes.Buffer
	if err := Build(p).WriteSARIF(&buf, nil); err != nil {
		t.Fatal(err)
	}
	doc := parseSARIF(t, buf.Bytes())
	if doc.Version != "2.1.0" || len(doc.Runs) != 1 {
		t.Fatalf("version=%q runs=%d", doc.Version, len(doc.Runs))
	}
	run := doc.Runs[0]
	if len(run.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(run.Results))
	}

	// Two rules -> two Scans groups: resource-drift then deprecation.
	if len(run.Tool.Driver.Rules) != 2 ||
		run.Tool.Driver.Rules[0].ID != "resource-drift" ||
		run.Tool.Driver.Rules[1].ID != "deprecation" {
		t.Fatalf("want [resource-drift deprecation] rules, got %+v", run.Tool.Driver.Rules)
	}

	byMsg := map[string]bool{}
	for _, res := range run.Results {
		byMsg[res.Message.Text] = true
		if res.RuleID != "resource-drift" {
			t.Errorf("ruleId = %q, want resource-drift", res.RuleID)
		}
		if res.Message.Markdown != "" {
			t.Errorf("message.markdown should be absent, got %q", res.Message.Markdown)
		}
		// tf-snag never sets baselineState — Sarif.Multitool fills it in later.
		if res.BaselineState != "" {
			t.Errorf("baselineState should be unset, got %q", res.BaselineState)
		}
		if len(res.GUID) != 36 {
			t.Errorf("guid = %q, want a UUID", res.GUID)
		}
		switch {
		case strings.HasPrefix(res.Message.Text, "Deleted"):
			if res.Level != "error" {
				t.Errorf("deleted: level=%q", res.Level)
			}
			if strings.Contains(res.Message.Text, "\n") {
				t.Errorf("delete message must stay single line, got %q", res.Message.Text)
			}
			if res.Message.Text != "Deleted — name=tfsnagdevukschksa01" {
				t.Errorf("deleted message = %q", res.Message.Text)
			}
			// No source index -> location falls back to the address, no region.
			if res.Locations[0].PhysicalLocation.ArtifactLocation.URI != "module.key_vault[0].azurerm_key_vault.vault" {
				t.Errorf("fallback uri = %q", res.Locations[0].PhysicalLocation.ArtifactLocation.URI)
			}
			if res.Locations[0].PhysicalLocation.Region != nil {
				t.Errorf("fallback location should have no region")
			}
		case strings.HasPrefix(res.Message.Text, "Updated"):
			if res.Level != "warning" {
				t.Errorf("updated: level=%q", res.Level)
			}
			// Two changed attributes -> one line each under the verb.
			if res.Message.Text != "Updated\ntags.Owner: null → \"Wes\"\ntags.Test: null → \"Test\"" {
				t.Errorf("updated message = %q", res.Message.Text)
			}
			if res.Properties["resourceType"] != "azurerm_resource_group" {
				t.Errorf("properties = %+v", res.Properties)
			}
		default:
			t.Errorf("unexpected message %q", res.Message.Text)
		}
	}
	if len(byMsg) != 2 {
		t.Errorf("expected one Deleted and one Updated result, got %v", byMsg)
	}
}

func TestWriteSARIFUsesSourceLocation(t *testing.T) {
	src := map[string]SourceLoc{
		"azurerm_key_vault.vault": {File: "terraform/modules/kv/main.tf", Line: 12},
	}
	var buf bytes.Buffer
	if err := Build(deletedVaultPlan()).WriteSARIF(&buf, src); err != nil {
		t.Fatal(err)
	}
	res := parseSARIF(t, buf.Bytes()).Runs[0].Results[0]
	loc := res.Locations[0].PhysicalLocation
	if loc.ArtifactLocation.URI != "terraform/modules/kv/main.tf" {
		t.Errorf("uri = %q", loc.ArtifactLocation.URI)
	}
	if loc.Region == nil || loc.Region.StartLine != 12 {
		t.Errorf("region = %+v", loc.Region)
	}
}

func TestWriteSARIFCleanReportHasEmptyResults(t *testing.T) {
	var buf bytes.Buffer
	if err := Build(&plan.Plan{TerraformVersion: "1.9.6"}).WriteSARIF(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"results": []`) {
		t.Errorf("clean SARIF should have an empty results array:\n%s", buf.String())
	}
}

func TestWriteMarkdownNoDrift(t *testing.T) {
	var buf bytes.Buffer
	p := &plan.Plan{
		TerraformVersion: "1.9.6",
		ResourceChanges: []plan.ResourceChange{
			{Address: "azurerm_resource_group.new", Change: plan.Change{Actions: []string{"create"}}},
		},
	}
	if err := Build(p).WriteMarkdown(&buf); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); !strings.Contains(got, "🟢 **No drift detected**") ||
		!strings.Contains(got, "### Pending changes from configuration") {
		t.Errorf("unexpected markdown: %q", got)
	}
}

// --- deprecations -----------------------------------------------------------

func sampleDiags() []plan.Diagnostic {
	return []plan.Diagnostic{
		// Same argument, same source line, two instances -> one site.
		{Severity: "warning", Summary: "Argument is deprecated", Detail: `The "foo" argument is deprecated. Use "bar".`,
			Address: "module.a.azurerm_x.y", Filename: "modules/a/main.tf", Line: 12},
		{Severity: "warning", Summary: "Argument is deprecated", Detail: `The "foo" argument is deprecated. Use "bar".`,
			Address: "module.a.azurerm_x.z", Filename: "modules/a/main.tf", Line: 12},
		{Severity: "warning", Summary: "Deprecated attribute", Detail: `The attribute "bar" is deprecated.`,
			Address: "azurerm_p.q", Filename: "main.tf", Line: 5},
		{Severity: "warning", Summary: "Values may be incompatible", Detail: "Automatic type conversion may not preserve intent."},
	}
}

func TestDeprecationsFilterDedupeSort(t *testing.T) {
	got := Deprecations(sampleDiags())
	if len(got) != 2 {
		t.Fatalf("got %d deprecations, want 2: %+v", len(got), got)
	}
	// Sorted by first site: main.tf:5 before modules/a/main.tf:12.
	if got[0].Summary != "Deprecated attribute" || len(got[0].Sites) != 1 ||
		got[0].Sites[0].File != "main.tf" || got[0].Sites[0].Line != 5 {
		t.Errorf("got[0] = %+v", got[0])
	}
	// The two instances at modules/a/main.tf:12 collapse to a single site.
	if got[1].Summary != "Argument is deprecated" || len(got[1].Sites) != 1 ||
		got[1].Sites[0].File != "modules/a/main.tf" || got[1].Sites[0].Line != 12 ||
		got[1].Sites[0].Address != "module.a.azurerm_x.y" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestDeprecationsCollapsesAcrossLocations(t *testing.T) {
	det := "`live_trace_enabled` has been deprecated in favor of `live_trace`."
	in := []plan.Diagnostic{
		{Severity: "warning", Summary: "Argument is deprecated", Detail: det,
			Address: "azurerm_signalr_service.a", Filename: "main.tf", Line: 46},
		{Severity: "warning", Summary: "Argument is deprecated", Detail: det,
			Address: "azurerm_signalr_service.b", Filename: "main.tf", Line: 59},
		{Severity: "warning", Summary: "Argument is deprecated", Detail: det,
			Address: "azurerm_signalr_service.c", Filename: "main.tf", Line: 73},
	}
	got := Deprecations(in)
	if len(got) != 1 {
		t.Fatalf("want 1 collapsed deprecation, got %d: %+v", len(got), got)
	}
	if len(got[0].Sites) != 3 {
		t.Fatalf("want 3 sites, got %+v", got[0].Sites)
	}
	// Sites sorted by line.
	for i, want := range []int{46, 59, 73} {
		if got[0].Sites[i].Line != want {
			t.Errorf("site[%d].Line = %d, want %d", i, got[0].Sites[i].Line, want)
		}
	}
}

func TestDeprecationsSeverityEscalates(t *testing.T) {
	in := []plan.Diagnostic{
		{Severity: "warning", Summary: "X is deprecated", Detail: "d", Address: "a.a", Filename: "m.tf", Line: 1},
		{Severity: "error", Summary: "X is deprecated", Detail: "d", Address: "a.b", Filename: "m.tf", Line: 2},
	}
	got := Deprecations(in)
	if len(got) != 1 || got[0].Severity != "error" {
		t.Fatalf("want one error-severity deprecation, got %+v", got)
	}
}

func TestDeprecationsIgnoresNonDeprecation(t *testing.T) {
	in := []plan.Diagnostic{
		{Severity: "warning", Summary: "Values may be incompatible", Detail: "Nothing to see."},
		{Severity: "error", Summary: "Invalid reference", Detail: "A managed resource has not been declared."},
	}
	if got := Deprecations(in); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestDeprecationsEmpty(t *testing.T) {
	if got := Deprecations(nil); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestWriteSARIFDeprecationResultShape(t *testing.T) {
	rep := &Report{
		Schema: ReportSchema,
		Deprecations: []Deprecation{{
			Severity: "warning",
			Summary:  "Argument is deprecated",
			Detail:   "The \"foo\" argument is deprecated.\nUse \"bar\" instead.",
			Sites:    []DeprecationSite{{Address: "module.a.azurerm_x.y", File: "modules/a/main.tf", Line: 12}},
		}},
	}
	var buf bytes.Buffer
	if err := rep.WriteSARIF(&buf, nil); err != nil {
		t.Fatal(err)
	}
	run := parseSARIF(t, buf.Bytes()).Runs[0]
	if len(run.Results) != 1 {
		t.Fatalf("want 1 result, got %d: %s", len(run.Results), buf.String())
	}
	res := run.Results[0]
	if res.RuleID != "deprecation" || res.Level != "warning" {
		t.Fatalf("ruleId/level = %q/%q", res.RuleID, res.Level)
	}
	if res.Message.Markdown != "" {
		t.Errorf("message.markdown should be absent, got %q", res.Message.Markdown)
	}
	// summary / flattened detail / the one site (shown even for a lone deprecation).
	lines := strings.Split(res.Message.Text, "\n")
	if len(lines) != 3 || lines[0] != "Argument is deprecated" ||
		!strings.Contains(lines[1], "Use \"bar\" instead.") ||
		lines[2] != "module.a.azurerm_x.y (modules/a/main.tf:12)" {
		t.Errorf("message = %q", res.Message.Text)
	}
	loc := res.Locations[0].PhysicalLocation
	if loc.ArtifactLocation.URI != "modules/a/main.tf" || loc.Region == nil || loc.Region.StartLine != 12 {
		t.Errorf("location = %q %+v", loc.ArtifactLocation.URI, loc.Region)
	}
	if len(res.RelatedLocations) != 0 {
		t.Errorf("single-site result should have no relatedLocations, got %+v", res.RelatedLocations)
	}
	if len(res.LogicalLocations) != 1 || res.LogicalLocations[0].FullyQualifiedName != "module.a.azurerm_x.y" {
		t.Errorf("logicalLocations = %+v", res.LogicalLocations)
	}
	if res.PartialFingerprints["deprecation/v1"] == "" {
		t.Errorf("partialFingerprints = %+v", res.PartialFingerprints)
	}
	if len(res.GUID) != 36 || res.BaselineState != "" {
		t.Errorf("guid=%q baselineState=%q", res.GUID, res.BaselineState)
	}
	if res.Properties["severity"] != "warning" || res.Properties["address"] != "module.a.azurerm_x.y" {
		t.Errorf("properties = %+v", res.Properties)
	}
}

func TestResultGUIDIsStableAndDistinct(t *testing.T) {
	a1 := resultGUID("resource-drift", "azurerm_x.y")
	a2 := resultGUID("resource-drift", "azurerm_x.y")
	b := resultGUID("resource-drift", "azurerm_x.z")
	c := resultGUID("deprecation", "azurerm_x.y")
	if a1 != a2 {
		t.Errorf("not deterministic: %q vs %q", a1, a2)
	}
	if len(a1) != 36 || a1[14] != '5' || (a1[19] != '8' && a1[19] != '9' && a1[19] != 'a' && a1[19] != 'b') {
		t.Errorf("not a v5 UUID: %q", a1)
	}
	if a1 == b || a1 == c {
		t.Errorf("collisions: drift.y=%q drift.z=%q depr.y=%q", a1, b, c)
	}
}

func TestWriteSARIFDeprecationMultiSite(t *testing.T) {
	rep := &Report{
		Schema: ReportSchema,
		Deprecations: []Deprecation{{
			Severity: "warning",
			Summary:  "Argument is deprecated",
			Detail:   "live_trace_enabled is deprecated.",
			Sites: []DeprecationSite{
				{Address: "azurerm_signalr_service.a", File: "main.tf", Line: 46},
				{Address: "azurerm_signalr_service.b", File: "main.tf", Line: 59},
				{Address: "azurerm_signalr_service.c", File: "main.tf", Line: 73},
			},
		}},
	}
	var buf bytes.Buffer
	if err := rep.WriteSARIF(&buf, nil); err != nil {
		t.Fatal(err)
	}
	run := parseSARIF(t, buf.Bytes()).Runs[0]
	if len(run.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(run.Results))
	}
	res := run.Results[0]
	// First site is the primary location, the other two are related.
	if res.Locations[0].PhysicalLocation.Region.StartLine != 46 {
		t.Errorf("primary location = %+v", res.Locations[0])
	}
	if len(res.RelatedLocations) != 2 ||
		res.RelatedLocations[0].PhysicalLocation.Region.StartLine != 59 ||
		res.RelatedLocations[1].PhysicalLocation.Region.StartLine != 73 {
		t.Errorf("relatedLocations = %+v", res.RelatedLocations)
	}
	if len(res.LogicalLocations) != 3 {
		t.Errorf("want 3 logicalLocations, got %+v", res.LogicalLocations)
	}
	// Message lists every site after the detail line.
	for _, want := range []string{"azurerm_signalr_service.a (main.tf:46)", "(main.tf:59)", "(main.tf:73)"} {
		if !strings.Contains(res.Message.Text, want) {
			t.Errorf("message missing %q:\n%s", want, res.Message.Text)
		}
	}
	if res.Properties["count"] != "3" || res.Properties["address"] != "" {
		t.Errorf("properties = %+v", res.Properties)
	}
}

func TestWriteSARIFDeprecationNoLocation(t *testing.T) {
	rep := &Report{
		Schema:       ReportSchema,
		Deprecations: []Deprecation{{Severity: "error", Summary: "Deprecated resource", Sites: []DeprecationSite{{Address: "azurerm_p.q"}}}},
	}
	var buf bytes.Buffer
	if err := rep.WriteSARIF(&buf, nil); err != nil {
		t.Fatal(err)
	}
	res := parseSARIF(t, buf.Bytes()).Runs[0].Results[0]
	if res.RuleID != "deprecation" || res.Level != "error" {
		t.Fatalf("ruleId/level = %q/%q", res.RuleID, res.Level)
	}
	loc := res.Locations[0].PhysicalLocation
	if loc.ArtifactLocation.URI != "azurerm_p.q" || loc.Region != nil {
		t.Errorf("fallback location = %q %+v", loc.ArtifactLocation.URI, loc.Region)
	}
}

func TestWriteTextShowsDeprecations(t *testing.T) {
	rep := Build(deletedVaultPlan())
	rep.Deprecations = []Deprecation{{
		Severity: "warning", Summary: "Argument is deprecated",
		Sites: []DeprecationSite{
			{Address: "azurerm_signalr_service.a", File: "main.tf", Line: 46},
			{Address: "azurerm_signalr_service.b", File: "main.tf", Line: 59},
		},
	}}
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{
		"deprecation warning(s): 1", "Argument is deprecated",
		"azurerm_signalr_service.a (main.tf:46)", "azurerm_signalr_service.b (main.tf:59)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("text missing %q:\n%s", want, got)
		}
	}
}

func TestWriteTextNoDriftButDeprecations(t *testing.T) {
	rep := Build(&plan.Plan{})
	rep.Deprecations = []Deprecation{
		{Severity: "warning", Summary: "Deprecated attribute", Sites: []DeprecationSite{{Address: "azurerm_p.q"}}},
	}
	var buf bytes.Buffer
	if err := rep.WriteText(&buf); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "no drift detected") || !strings.Contains(got, "deprecation warning(s): 1") ||
		!strings.Contains(got, "azurerm_p.q") {
		t.Errorf("unexpected text:\n%s", got)
	}
}
