package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestRetirementsCheckReportsAndGates(t *testing.T) {
	code, out, errb := runCLI(t, "-check", "retirements", "-plan", "testdata/plan-state.json", "-color", "never")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (a retired SKU is a finding) — stderr: %s", code, errb)
	}
	for _, want := range []string{
		"retirement(s): 1",
		"Basic SKU public IP addresses retire",
		"azurerm_public_ip.bastion",
		"module.network.azurerm_public_ip.egress[0]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// -check retirements was not asked about drift, so the report must not
	// carry any: the plan it read has pending changes in it.
	if strings.Contains(out, "pending changes from configuration") {
		t.Errorf("retirements-only run reported pending changes:\n%s", out)
	}
}

func TestRetirementsFailWithinNarrowsTheGate(t *testing.T) {
	// Everything in the shipped catalogue that this plan matches is long past,
	// so a window still gates. A window that excludes it is the interesting
	// case, and -retirements-fail-within cannot express "only the future", so
	// this asserts the flag parses and the finding is still reported.
	code, out, errb := runCLI(t, "-check", "retirements", "-plan", "testdata/plan-state.json",
		"-retirements-fail-within", "90d", "-color", "never")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 — stderr: %s", code, errb)
	}
	if !strings.Contains(out, "retirement(s): 1") {
		t.Errorf("finding not reported:\n%s", out)
	}

	if _, _, errb := runCLI(t, "-check", "retirements", "-plan", "testdata/plan-state.json",
		"-retirements-fail-within", "soon"); !strings.Contains(errb, "invalid -retirements-fail-within") {
		t.Errorf("a bad window should be rejected, got: %s", errb)
	}
}

func TestRetirementsRespectsAnIgnoreRule(t *testing.T) {
	dir := t.TempDir()
	ignorePath := filepath.Join(dir, "ignore.yml")
	if err := os.WriteFile(ignorePath, []byte(
		"retirements:\n  - id: azure-public-ip-basic-sku\n    reason: CSES deployment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runCLI(t, "-check", "retirements", "-plan", "testdata/plan-state.json",
		"-ignore", ignorePath, "-color", "never")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (the only finding is suppressed) — stderr: %s\n%s", code, errb, out)
	}
}

func TestRetirementsCatalogueOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retirements.yaml")
	body := `updated: 2026-09-16
retirements:
  - id: internal-basic-tier-deadline
    title: Internal deadline for Basic SKU public IPs
    retires_on: 2027-01-01
    url: https://intranet.example/deadlines
    match:
      type: azurerm_public_ip
      attributes:
        sku: Basic
    remediation: Ask the platform team.
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runCLI(t, "-check", "retirements", "-plan", "testdata/plan-state.json",
		"-retirements", path, "-format", "json")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 — stderr: %s", code, errb)
	}
	var doc struct {
		Retirements []struct {
			ID string `json:"id"`
		} `json:"retirements"`
		Catalogue struct {
			Source string `json:"source"`
		} `json:"retirement_catalogue"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	// The override replaces the built-in catalogue rather than adding to it, so
	// the Basic public IPs in this plan are not reported.
	if len(doc.Retirements) != 1 || doc.Retirements[0].ID != "internal-basic-tier-deadline" {
		t.Fatalf("override not used: %+v", doc.Retirements)
	}
	if doc.Catalogue.Source != path {
		t.Errorf("catalogue source = %q, want the override path", doc.Catalogue.Source)
	}
}

func TestRetirementFlagsNeedTheCheck(t *testing.T) {
	cases := [][]string{
		{"-check", "drift", "-plan", "testdata/plan-state.json", "-retirements", "cat.yaml"},
		{"-check", "drift", "-plan", "testdata/plan-state.json", "-retirements-fail-within", "90d"},
	}
	for _, args := range cases {
		code, _, errb := runCLI(t, args...)
		if code != 2 || !strings.Contains(errb, "-check does not include retirements") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errb)
		}
	}
}

func TestRetirementsWarnsWhenThePlanHasNoState(t *testing.T) {
	// plan-drift.json is a change-only plan: no planned_values, no prior_state.
	// A silent empty result would read as "no retirements", which would not be true.
	code, _, errb := runCLI(t, "-check", "retirements", "-plan", "testdata/plan-drift.json", "-color", "never")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errb, "no resource state") {
		t.Errorf("stderr does not explain the empty result: %q", errb)
	}
}
