package plan

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func loadFixture(t *testing.T, name string) *Plan {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func addresses(rs []StateResource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Address
	}
	return out
}

func TestResourcesWalksModulesAndPrefersPlannedValues(t *testing.T) {
	got := addresses(loadFixture(t, "plan-state.json").Resources())
	// Sorted by address, every module depth included. The data source is absent,
	// and so is azurerm_storage_account.legacy: this plan destroys it, so it is
	// in prior_state but not in planned_values.
	want := []string{
		"azurerm_public_ip.bastion",
		"azurerm_storage_account.data",
		"module.network.azurerm_public_ip.egress[0]",
		"module.network.azurerm_public_ip.egress[1]",
		`module.network.module.firewall.azurerm_public_ip.fw["primary"]`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Resources() = %v, want %v", got, want)
	}
}

func TestResourcesReportsPlannedAttributeValues(t *testing.T) {
	for _, r := range loadFixture(t, "plan-state.json").Resources() {
		if r.Address != "azurerm_storage_account.data" {
			continue
		}
		// prior_state has TLS1_0 and this plan raises it. A check must see the
		// fixed value, or it reports a finding the run already resolves.
		if got := r.Values["min_tls_version"]; got != "TLS1_2" {
			t.Fatalf("min_tls_version = %v, want TLS1_2 (the planned value)", got)
		}
		return
	}
	t.Fatal("azurerm_storage_account.data missing from Resources()")
}

func TestResourcesCarriesTypeIndexAndModule(t *testing.T) {
	byAddress := make(map[string]StateResource)
	for _, r := range loadFixture(t, "plan-state.json").Resources() {
		byAddress[r.Address] = r
	}

	root := byAddress["azurerm_public_ip.bastion"]
	if root.Type != "azurerm_public_ip" || root.Name != "bastion" {
		t.Errorf("root resource = %+v", root)
	}
	if root.ModuleAddress != "" {
		t.Errorf("root ModuleAddress = %q, want empty", root.ModuleAddress)
	}
	if root.Values["sku"] != "Basic" {
		t.Errorf("sku = %v, want Basic", root.Values["sku"])
	}

	counted := byAddress["module.network.azurerm_public_ip.egress[1]"]
	if counted.ModuleAddress != "module.network" {
		t.Errorf("ModuleAddress = %q, want module.network", counted.ModuleAddress)
	}
	if idx, ok := counted.Index.(float64); !ok || idx != 1 {
		t.Errorf("Index = %#v, want 1", counted.Index)
	}

	nested := byAddress[`module.network.module.firewall.azurerm_public_ip.fw["primary"]`]
	if nested.ModuleAddress != "module.network.module.firewall" {
		t.Errorf("nested ModuleAddress = %q", nested.ModuleAddress)
	}
	if nested.Index != "primary" {
		t.Errorf("for_each Index = %#v, want \"primary\"", nested.Index)
	}
}

func TestResourcesFallsBackToPriorState(t *testing.T) {
	raw := []byte(`{
	  "format_version": "1.2",
	  "prior_state": {"values": {"root_module": {"resources": [
	    {"address":"azurerm_public_ip.a","mode":"managed","type":"azurerm_public_ip",
	     "name":"a","values":{"sku":"Basic"}}
	  ]}}}
	}`)
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := addresses(p.Resources())
	if len(got) != 1 || got[0] != "azurerm_public_ip.a" {
		t.Fatalf("Resources() = %v, want [azurerm_public_ip.a] from prior_state", got)
	}
}

func TestResourcesEmptyWithoutStateSections(t *testing.T) {
	// plan-drift.json is a change-only plan: no planned_values, no prior_state.
	if got := loadFixture(t, "plan-drift.json").Resources(); len(got) != 0 {
		t.Fatalf("Resources() = %v, want none for a plan with no state sections", addresses(got))
	}
	p, err := Parse([]byte(`{"format_version":"1.2","planned_values":{"root_module":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Resources(); len(got) != 0 {
		t.Fatalf("Resources() = %v, want none for an empty root module", addresses(got))
	}
}

// TestResourcesPrefersPlannedValuesForRetirements guards the property the
// retirements check depends on: a plan that fixes a resource must not report it
// as still affected. Written here because it is a plan-parsing promise, not a
// retirement one.
func TestResourcesPrefersPlannedValuesForRetirements(t *testing.T) {
	for _, r := range loadFixture(t, "plan-state.json").Resources() {
		if r.Address == "azurerm_storage_account.data" && r.Values["min_tls_version"] == "TLS1_0" {
			t.Fatal("prior_state value won over planned_values; a fix in this plan would still be reported")
		}
	}
}
