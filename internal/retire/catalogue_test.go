package retire

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wes-key/tf-snag/internal/plan"
)

func TestEmbeddedCatalogueLoadsAndValidates(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("the shipped catalogue does not parse: %v", err)
	}
	if c.Source != "built-in" {
		t.Errorf("Source = %q, want built-in", c.Source)
	}
	if c.Updated.IsZero() {
		t.Error("catalogue has no updated date, so a report cannot say how stale it is")
	}
	for _, e := range c.Entries {
		if e.Remediation == "" {
			t.Errorf("%s: no remediation - a finding nobody can act on", e.ID)
		}
		if e.Announced.After(e.RetiresOn.Time) && !e.Announced.IsZero() {
			t.Errorf("%s: announced %s is after retires_on %s", e.ID, e.Announced, e.RetiresOn)
		}
	}
}

// Every shipped entry needs a case here: one resource it must flag, and one
// near miss it must leave alone. A wrong match tells someone their production
// estate is dying, so an entry without evidence does not ship.
func TestEmbeddedEntriesMatchAffectedResourcesOnly(t *testing.T) {
	cases := map[string]struct {
		affected   map[string]any
		unaffected map[string]any
	}{
		"azure-public-ip-basic-sku": {
			affected:   map[string]any{"sku": "Basic", "allocation_method": "Dynamic"},
			unaffected: map[string]any{"sku": "Standard", "allocation_method": "Static"},
		},
		"azure-application-gateway-v1": {
			affected:   map[string]any{"sku": []any{map[string]any{"name": "WAF_Medium", "tier": "WAF"}}},
			unaffected: map[string]any{"sku": []any{map[string]any{"name": "WAF_v2", "tier": "WAF_v2"}}},
		},
		"azure-lb-basic-sku": {
			affected:   map[string]any{"sku": "Basic", "sku_tier": "Regional"},
			unaffected: map[string]any{"sku": "Standard", "sku_tier": "Regional"},
		},
		"azure-aks-basic-load-balancer": {
			affected:   map[string]any{"network_profile": []any{map[string]any{"load_balancer_sku": "basic"}}},
			unaffected: map[string]any{"network_profile": []any{map[string]any{"load_balancer_sku": "standard"}}},
		},
		"azure-vpn-gateway-legacy-sku": {
			affected:   map[string]any{"sku": "HighPerformance", "type": "Vpn"},
			unaffected: map[string]any{"sku": "VpnGw2AZ", "type": "Vpn"},
		},
		"azure-storage-min-tls": {
			affected:   map[string]any{"min_tls_version": "TLS1_0"},
			unaffected: map[string]any{"min_tls_version": "TLS1_2"},
		},
		"azure-log-analytics-agent": {
			affected: map[string]any{
				"publisher": "Microsoft.EnterpriseCloud.Monitoring",
				"type":      "OmsAgentForLinux",
			},
			// Same publisher, a different extension: both attribute tests have to
			// pass, or every monitoring extension would be flagged.
			unaffected: map[string]any{
				"publisher": "Microsoft.EnterpriseCloud.Monitoring",
				"type":      "AzureMonitorLinuxAgent",
			},
		},
		"azure-cdn-edgio": {
			affected:   map[string]any{"sku": "Standard_Verizon"},
			unaffected: map[string]any{"sku": "Standard_AzureFrontDoor"},
		},
		"azure-cdn-classic-microsoft": {
			affected:   map[string]any{"sku": "Standard_Microsoft"},
			unaffected: map[string]any{"sku": "Standard_AzureFrontDoor"},
		},
		"azure-front-door-classic": {
			// Whole-type retirement: every azurerm_frontdoor is classic, so there
			// is no near miss to write.
			affected: map[string]any{"friendly_name": "edge"},
		},
		"azure-redis-cache-tiers": {
			affected: map[string]any{"sku_name": "Standard", "capacity": float64(1)},
			// Not a tier azurerm_redis_cache accepts, but it proves the test is
			// bounded by the list rather than matching anything.
			unaffected: map[string]any{"sku_name": "Enterprise", "capacity": float64(1)},
		},
	}

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, e := range c.Entries {
		tc, ok := cases[e.ID]
		if !ok {
			t.Errorf("catalogue entry %q has no test case in this table - add one", e.ID)
			continue
		}
		single := func(values map[string]any) []Finding {
			return Check(&Catalogue{Entries: []Entry{e}},
				[]plan.StateResource{{Address: "x.y", Type: e.Match.Type, Mode: "managed", Values: values}}, now)
		}
		if got := single(tc.affected); len(got) != 1 {
			t.Errorf("%s: did not match its affected example %v", e.ID, tc.affected)
		}
		if tc.unaffected == nil {
			continue // whole-type retirement: nothing of that type is unaffected
		}
		if got := single(tc.unaffected); len(got) != 0 {
			t.Errorf("%s: matched its unaffected example %v - false positive", e.ID, tc.unaffected)
		}
	}
}

func TestLoadReadsAnOverrideFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retirements.yaml")
	body := `updated: 2026-01-01
retirements:
  - id: local-deadline
    title: Internal deadline for Basic tier
    retires_on: 2026-12-31
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
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Entries) != 1 || c.Entries[0].ID != "local-deadline" {
		t.Fatalf("Load(%s) = %+v, want only the file's entry", path, c.Entries)
	}
	if c.Source != path {
		t.Errorf("Source = %q, want the file path so the report can cite it", c.Source)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("Load of a missing file succeeded; want an error")
	}
}

func TestAgeReportsCatalogueStaleness(t *testing.T) {
	c := &Catalogue{Updated: Date{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	if got := c.Age(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)); got.Hours()/24 != 90 {
		t.Errorf("Age = %v, want 90 days", got)
	}
	if got := (&Catalogue{}).Age(time.Now()); got != 0 {
		t.Errorf("Age of a catalogue with no date = %v, want 0", got)
	}
}
