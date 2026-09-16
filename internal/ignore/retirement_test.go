package ignore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wes-key/tf-snag/internal/report"
)

func ruleFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".tf-snag-ignore.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func retirementReport() *report.Report {
	return &report.Report{Retirements: []report.Retirement{{
		ID:    "azure-public-ip-basic-sku",
		Title: "Basic SKU public IP addresses retire",
		Instances: []report.RetirementInstance{
			{Address: "azurerm_public_ip.bastion"},
			{Address: "module.legacy.azurerm_public_ip.edge"},
		},
	}}}
}

func TestIgnoreRetirementByID(t *testing.T) {
	s, err := LoadFile(ruleFile(t, `
retirements:
  - id: azure-public-ip-basic-sku
    reason: CSES deployment, exempt from the retirement
`))
	if err != nil {
		t.Fatal(err)
	}
	r := retirementReport()
	s.Apply(r)
	if !r.Retirements[0].Suppressed {
		t.Fatal("rule by id did not suppress the retirement")
	}
	if r.Retirements[0].SuppressReason != "CSES deployment, exempt from the retirement" {
		t.Errorf("reason = %q", r.Retirements[0].SuppressReason)
	}
	// Suppressing by id leaves the instances in place: the report still shows
	// what was waived, it just does not gate.
	if len(r.Retirements[0].Instances) != 2 {
		t.Errorf("instances = %d, want both kept", len(r.Retirements[0].Instances))
	}
}

func TestIgnoreRetirementByAddressDropsOnlyThatInstance(t *testing.T) {
	s, err := LoadFile(ruleFile(t, `
retirements:
  - address: module.legacy.azurerm_public_ip.edge
    reason: decommissioned in Q3, tracked in PLAT-412
`))
	if err != nil {
		t.Fatal(err)
	}
	r := retirementReport()
	s.Apply(r)
	rt := r.Retirements[0]
	if rt.Suppressed {
		t.Error("one waived instance must not silence the finding for the others")
	}
	if len(rt.Instances) != 1 || rt.Instances[0].Address != "azurerm_public_ip.bastion" {
		t.Errorf("instances = %+v, want only the un-waived one", rt.Instances)
	}
}

func TestIgnoreRetirementSuppressesWhenEveryInstanceIsWaived(t *testing.T) {
	s, err := LoadFile(ruleFile(t, `
retirements:
  - address: "*azurerm_public_ip*"
    reason: whole estate migrating together
`))
	if err != nil {
		t.Fatal(err)
	}
	r := retirementReport()
	s.Apply(r)
	if !r.Retirements[0].Suppressed {
		t.Error("with no instances left the finding should be suppressed, not empty")
	}
}

func TestRetirementRulesAppearInTheExceptionsRegister(t *testing.T) {
	s, err := LoadFile(ruleFile(t, `
retirements:
  - id: azure-public-ip-basic-sku
    reason: CSES deployment
  - id: never-matches-anything
    reason: written for an estate we no longer have
`))
	if err != nil {
		t.Fatal(err)
	}
	s.Apply(retirementReport())
	exs := s.Exceptions()
	if len(exs) != 2 {
		t.Fatalf("exceptions = %d, want 2", len(exs))
	}
	if exs[0].Scopes[0] != "retirement" {
		t.Errorf("scope = %v, want retirement", exs[0].Scopes)
	}
	if exs[0].Stale() {
		t.Error("a rule that suppressed a finding is not stale")
	}
	if !exs[1].Stale() {
		t.Error("a rule that matched nothing should read as stale")
	}
}
