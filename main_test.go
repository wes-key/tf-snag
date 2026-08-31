package main

import (
	"bytes"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestRunWithPlanFileReportsDriftAndExits2(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json"}, strings.NewReader(""), &out, &errb)

	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	for _, want := range []string{
		"changed outside Terraform",
		"azurerm_storage_account.data",
		"min_tls_version",
		"1 to add, 0 to change, 1 to destroy",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q\n---\n%s", want, out.String())
		}
	}
}

func TestRunExitCodeFalseStillZeroOnDrift(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-exit-code=false"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
}

func TestRunJSONFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "json"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Errorf("expected JSON object, got: %s", out.String())
	}
}

func TestRunJUnitFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "junit"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{`<testsuites>`, `name="tf-snag"`, `<failure message=`, "azurerm_storage_account.data"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("junit output missing %q\n---\n%s", want, out.String())
		}
	}
}

func TestRunMarkdownFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "markdown"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "🔴") {
		t.Errorf("expected markdown to open with the status line, got: %s", out.String())
	}
}

func TestRunSARIFFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "sarif"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{`"version": "2.1.0"`, `"ruleId": "resource-drift"`, `"name": "tf-snag"`, `"guid"`, `"driftAddress/v1"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("sarif output missing %q\n---\n%s", want, out.String())
		}
	}
	// tf-snag leaves baselineState for the downstream Multitool pass.
	if strings.Contains(out.String(), `"baselineState"`) {
		t.Errorf("baselineState should not be emitted by tf-snag\n%s", out.String())
	}
}

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-version"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, "tf-snag ") || !strings.Contains(got, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("version line = %q", got)
	}
}

func TestRunUnknownFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "yaml"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown format") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunStdin(t *testing.T) {
	in := `{"format_version":"1.2","terraform_version":"1.9.6","resource_drift":[],"resource_changes":[]}`
	var out, errb bytes.Buffer
	code := run(nil, strings.NewReader(in), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "no drift detected") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestRunBadInput(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(nil, strings.NewReader(`{"nope":true}`), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "format_version") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunDeprecationsSARIF(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "deprecations", "-plan-log", "testdata/plan-log-deprecations.jsonl", "-format", "sarif"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{`"id": "resource-drift"`, `"id": "deprecation"`, `"ruleId": "deprecation"`, "Argument is deprecated"} {
		if !strings.Contains(s, want) {
			t.Errorf("sarif missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, `"ruleId": "resource-drift"`) {
		t.Errorf("did not expect any resource-drift results\n%s", s)
	}
	// Without -plan-log-dir the fixture's paths are used verbatim.
	if !strings.Contains(s, `"uri": "main.tf"`) || strings.Contains(s, `"uri": "terraform/main.tf"`) {
		t.Errorf("expected bare fixture paths\n%s", s)
	}
}

func TestRunDeprecationsPlanLogDirPrefixesPaths(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{
		"-check", "deprecations",
		"-plan-log", "testdata/plan-log-deprecations.jsonl",
		"-plan-log-dir", "terraform",
		"-format", "sarif",
	}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{`"uri": "terraform/main.tf"`, `"uri": "terraform/modules/a/main.tf"`} {
		if !strings.Contains(s, want) {
			t.Errorf("sarif missing prefixed path %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, `"uri": "main.tf"`) {
		t.Errorf("unprefixed path leaked through\n%s", s)
	}
	// Message lines carry the prefixed location too.
	if !strings.Contains(s, "(terraform/main.tf:5)") {
		t.Errorf("message location not prefixed\n%s", s)
	}
}

func TestRunPlanLogDirWithoutDeprecations(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan-log-dir", "terraform", "-plan", "testdata/plan-drift.json"},
		strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "-plan-log-dir set but") {
		t.Errorf("exit=%d stderr=%q", code, errb.String())
	}
}

func TestRunBothChecksSARIF(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{
		"-check", "all",
		"-plan", "testdata/plan-drift.json",
		"-plan-log", "testdata/plan-log-deprecations.jsonl",
		"-format", "sarif",
	}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	drift := strings.Index(s, `"ruleId": "resource-drift"`)
	depr := strings.Index(s, `"ruleId": "deprecation"`)
	if drift < 0 || depr < 0 {
		t.Fatalf("want both rule ids in results\n%s", s)
	}
	if drift > depr {
		t.Errorf("resource-drift results should precede deprecation results (%d vs %d)", drift, depr)
	}
}

func TestRunDeprecationsStdin(t *testing.T) {
	raw, err := os.ReadFile("testdata/plan-log-deprecations.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run([]string{"-check", "deprecations", "-format", "sarif"}, bytes.NewReader(raw), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), `"ruleId": "deprecation"`) {
		t.Errorf("sarif missing deprecation result\n%s", out.String())
	}
}

func TestRunDeprecationsCleanExitsZero(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "deprecations", "-plan-log", "testdata/plan-log-clean.jsonl"},
		strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "no drift detected") || strings.Contains(s, "deprecation warning(s)") {
		t.Errorf("unexpected text output:\n%s", s)
	}
}

func TestRunDeprecationsRejectsJUnitFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "deprecations", "-plan-log", "testdata/plan-log-clean.jsonl", "-format", "junit"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "sarif, json, text or markdown") {
		t.Errorf("stderr = %q", errb.String())
	}
}

// -check deprecations -format json is now allowed (schema 2 carries them); the
// tf-snag extension consumes it.
func TestRunDeprecationsJSON(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "all", "-plan", "testdata/plan-drift.json",
		"-plan-log", "testdata/plan-log-deprecations.jsonl", "-format", "json"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	var doc struct {
		Schema       int `json:"schema"`
		Deprecations []struct {
			Summary string `json:"summary"`
		} `json:"deprecations"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out.String())
	}
	if doc.Schema != 2 {
		t.Errorf("schema = %d, want 2", doc.Schema)
	}
	if len(doc.Deprecations) == 0 {
		t.Errorf("deprecations not carried in JSON: %s", out.String())
	}
}

func TestRunDeprecationsMarkdown(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "all", "-plan", "testdata/plan-drift.json",
		"-plan-log", "testdata/plan-log-deprecations.jsonl", "-format", "markdown"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"deprecation warning", "### Deprecation warnings", "Argument is deprecated"} {
		if !strings.Contains(s, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, s)
		}
	}
}

func TestRunUnknownCheck(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "bogus", "-plan", "testdata/plan-drift.json"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown check") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunPlanLogWithoutDeprecations(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "drift", "-plan", "testdata/plan-drift.json", "-plan-log", "testdata/plan-log-clean.jsonl"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "plan-log set but") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunBothChecksBothStdin(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "all"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "both the plan and the plan log from stdin") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunDefaultCheckIsDrift(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "sarif"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	s := out.String()
	if !strings.Contains(s, `"ruleId": "resource-drift"`) {
		t.Errorf("expected resource-drift results\n%s", s)
	}
	if strings.Contains(s, `"ruleId": "deprecation"`) {
		t.Errorf("plain drift run should have no deprecation results\n%s", s)
	}
}

func TestRunIgnoreFileSuppressesDrift(t *testing.T) {
	var out, errb bytes.Buffer
	// tf-snag-ignore.yml ignores azurerm_storage_account.data (the only real drift).
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "sarif",
		"-ignore", "testdata/tf-snag-ignore.yml"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (all drift suppressed) — stderr: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, `"suppressions"`) || !strings.Contains(s, `"kind": "external"`) {
		t.Errorf("sarif missing suppression on the ignored result:\n%s", s)
	}
}

func TestRunIgnoreFileBadPath(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-ignore", "testdata/nope.yml"},
		strings.NewReader(""), &out, &errb)
	if code != 2 || errb.Len() == 0 {
		t.Errorf("exit=%d stderr=%q, want 2 + an error", code, errb.String())
	}
}

func TestRunInlineIgnoreViaSource(t *testing.T) {
	// A plan whose only drift is on azurerm_role_assignment, which
	// testdata/tfsrc/main.tf marks `# tf-snag:ignore-drift`.
	plan := `{"format_version":"1.2","terraform_version":"1.9.6","resource_drift":[` +
		`{"address":"module.rbac.azurerm_role_assignment.admin[0]","type":"azurerm_role_assignment",` +
		`"change":{"actions":["update"],"before":{"role":"Reader"},"after":{"role":"Owner"}}}],` +
		`"resource_changes":[]}`
	var out, errb bytes.Buffer
	code := run([]string{"-format", "text", "-source", "testdata/tfsrc"}, strings.NewReader(plan), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (inline-ignored) — stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "ignored: 1 drift") ||
		!strings.Contains(out.String(), "managed by PIM") {
		t.Errorf("text missing inline-ignored section:\n%s", out.String())
	}
}

// -format json with -source carries the .tf file+line for each drifted resource
// (testdata/tfsrc/main.tf declares azurerm_signalr_service.legacy at line 6).
func TestRunJSONSourceLocations(t *testing.T) {
	plan := `{"format_version":"1.2","terraform_version":"1.9.6","resource_drift":[` +
		`{"address":"azurerm_signalr_service.legacy","type":"azurerm_signalr_service",` +
		`"change":{"actions":["update"],"before":{"a":1},"after":{"a":2}}}],"resource_changes":[]}`
	var out, errb bytes.Buffer
	code := run([]string{"-format", "json", "-source", "testdata/tfsrc"}, strings.NewReader(plan), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 — stderr: %s", code, errb.String())
	}
	var doc struct {
		Drift []struct {
			Address string `json:"address"`
			File    string `json:"file"`
			Line    int    `json:"line"`
		} `json:"drift"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out.String())
	}
	if len(doc.Drift) != 1 || doc.Drift[0].File != "main.tf" || doc.Drift[0].Line != 6 {
		t.Errorf("drift[0] file/line = %q/%d, want main.tf/6 (%+v)", doc.Drift[0].File, doc.Drift[0].Line, doc.Drift)
	}
}
