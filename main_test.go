package main

import (
	"bytes"
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
	for _, want := range []string{`"version": "2.1.0"`, `"ruleId": "resource-drift"`, `"name": "tf-snag"`, `"baselineState"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("sarif output missing %q\n---\n%s", want, out.String())
		}
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

func TestRunDeprecationsRejectsJSONFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "deprecations", "-plan-log", "testdata/plan-log-clean.jsonl", "-format", "json"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "sarif or text") {
		t.Errorf("stderr = %q", errb.String())
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
