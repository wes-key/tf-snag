package retire

import (
	"strings"
	"testing"
	"time"

	"github.com/wes-key/tf-snag/internal/plan"
)

func mustParse(t *testing.T, y string) *Catalogue {
	t.Helper()
	c, err := parse([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

func at(t *testing.T, date string) time.Time {
	t.Helper()
	tm, err := time.Parse(DateLayout, date)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func resource(address, typ string, values map[string]any) plan.StateResource {
	return plan.StateResource{Address: address, Type: typ, Mode: "managed", Values: values}
}

const testCatalogue = `
updated: 2026-09-16
retirements:
  - id: basic-pip
    title: Basic SKU public IPs retire
    retires_on: 2026-12-01
    url: https://example.test/pip
    severity: high
    match:
      type: azurerm_public_ip
      attributes:
        sku: Basic
  - id: old-tls
    title: Storage accounts below TLS 1.2 stop working
    retires_on: 2027-06-01
    url: https://example.test/tls
    match:
      type: azurerm_storage_account
      attributes:
        min_tls_version: {not: TLS1_2}
  - id: legacy-kinds
    title: Legacy storage kinds retire
    retires_on: 2026-09-01
    url: https://example.test/kind
    match:
      type: azurerm_storage_account
      attributes:
        account_kind: {in: [Storage, BlobStorage]}
  - id: whole-type
    title: Single Server MySQL retires
    retires_on: 2026-10-15
    url: https://example.test/mysql
    match:
      type: azurerm_mysql_server
`

func TestCheckMatchesEqualityInAndNot(t *testing.T) {
	resources := []plan.StateResource{
		resource("azurerm_public_ip.a", "azurerm_public_ip", map[string]any{"sku": "Basic"}),
		resource("azurerm_public_ip.b", "azurerm_public_ip", map[string]any{"sku": "Standard"}),
		resource("azurerm_storage_account.old", "azurerm_storage_account",
			map[string]any{"min_tls_version": "TLS1_0", "account_kind": "Storage"}),
		resource("azurerm_storage_account.new", "azurerm_storage_account",
			map[string]any{"min_tls_version": "TLS1_2", "account_kind": "StorageV2"}),
		resource("azurerm_mysql_server.legacy", "azurerm_mysql_server", map[string]any{"version": "5.7"}),
	}
	got := map[string][]string{}
	for _, f := range Check(mustParse(t, testCatalogue), resources, at(t, "2026-09-16")) {
		for _, i := range f.Instances {
			got[f.Entry.ID] = append(got[f.Entry.ID], i.Address)
		}
	}
	want := map[string][]string{
		"basic-pip":    {"azurerm_public_ip.a"},
		"old-tls":      {"azurerm_storage_account.old"},
		"legacy-kinds": {"azurerm_storage_account.old"},
		// No attributes: the whole resource type is retiring.
		"whole-type": {"azurerm_mysql_server.legacy"},
	}
	for id, addrs := range want {
		if strings.Join(got[id], ",") != strings.Join(addrs, ",") {
			t.Errorf("%s matched %v, want %v", id, got[id], addrs)
		}
	}
	if len(got) != len(want) {
		t.Errorf("matched entries %v, want %v", keys(got), keys(want))
	}
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestCheckIgnoresResourcesMissingTheAttribute(t *testing.T) {
	// A storage account whose min_tls_version the plan does not carry must not
	// match `not: TLS1_2`: absence is not evidence of an old TLS setting.
	resources := []plan.StateResource{
		resource("azurerm_storage_account.unknown", "azurerm_storage_account", map[string]any{"name": "st1"}),
	}
	if got := Check(mustParse(t, testCatalogue), resources, at(t, "2026-09-16")); len(got) != 0 {
		t.Fatalf("Check() = %v, want no findings", got)
	}
}

func TestCheckOrdersBySoonestAndGroupsInstances(t *testing.T) {
	resources := []plan.StateResource{
		resource("azurerm_public_ip.a", "azurerm_public_ip", map[string]any{"sku": "Basic"}),
		resource("azurerm_public_ip.c", "azurerm_public_ip", map[string]any{"sku": "basic"}), // case-insensitive
		resource("azurerm_public_ip.b", "azurerm_public_ip", map[string]any{"sku": "Basic"}),
		resource("azurerm_mysql_server.x", "azurerm_mysql_server", nil),
	}
	got := Check(mustParse(t, testCatalogue), resources, at(t, "2026-09-16"))
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2", len(got))
	}
	if got[0].Entry.ID != "whole-type" {
		t.Errorf("first finding = %s, want whole-type (retires 2026-10-15, sooner)", got[0].Entry.ID)
	}
	pip := got[1]
	if len(pip.Instances) != 3 {
		t.Fatalf("basic-pip instances = %d, want 3 grouped into one finding", len(pip.Instances))
	}
	if pip.Instances[0].Address != "azurerm_public_ip.a" || pip.Instances[2].Address != "azurerm_public_ip.c" {
		t.Errorf("instances not sorted by address: %+v", pip.Instances)
	}
	if pip.Instances[0].Matched["sku"] != "Basic" {
		t.Errorf("Matched = %v, want the value that matched", pip.Instances[0].Matched)
	}
}

func TestUrgencyBands(t *testing.T) {
	cases := []struct {
		date string
		want Urgency
		days int
	}{
		{"2026-09-15", Retired, -1},
		{"2026-09-16", Imminent, 0},
		{"2026-12-15", Imminent, 90},
		{"2026-12-16", Approaching, 91},
		{"2027-03-15", Approaching, 180},
		{"2027-03-16", Scheduled, 181},
	}
	now := at(t, "2026-09-16")
	for _, c := range cases {
		d := Date{at(t, c.date)}
		days := daysUntil(d, now)
		if days != c.days {
			t.Errorf("daysUntil(%s) = %d, want %d", c.date, days, c.days)
		}
		if got := urgency(days); got != c.want {
			t.Errorf("urgency(%s, %d days) = %s, want %s", c.date, days, got, c.want)
		}
	}
}

func TestLookupWalksNestedMapsAndLists(t *testing.T) {
	values := map[string]any{
		"sku":      []any{map[string]any{"name": "Standard_v2", "tier": "Standard_v2"}},
		"identity": map[string]any{"type": "SystemAssigned"},
		"zones":    []any{"1", "2"},
	}
	cases := []struct {
		path string
		want any
	}{
		{"sku.name", "Standard_v2"},   // fans out across a block list
		{"sku.0.tier", "Standard_v2"}, // explicit index
		{"identity.type", "SystemAssigned"},
		{"zones", nil}, // a list itself, not a leaf: returned whole
	}
	for _, c := range cases {
		got := lookup(values, c.path)
		if c.want == nil {
			continue
		}
		if len(got) == 0 || got[0] != c.want {
			t.Errorf("lookup(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	if got := lookup(values, "sku.missing"); got != nil {
		t.Errorf("lookup of an absent path = %v, want nil", got)
	}
}

func TestParseRejectsBadCatalogues(t *testing.T) {
	cases := map[string]string{
		"unknown field": `retirements: [{id: a, title: t, retires_on: 2026-01-01, url: u, matches: {type: x}}]`,
		"duplicate id": `retirements:
  - {id: a, title: t, retires_on: 2026-01-01, url: u, match: {type: x}}
  - {id: a, title: t2, retires_on: 2026-01-01, url: u, match: {type: y}}`,
		"bad date":     `retirements: [{id: a, title: t, retires_on: 01-01-2026, url: u, match: {type: x}}]`,
		"no url":       `retirements: [{id: a, title: t, retires_on: 2026-01-01, match: {type: x}}]`,
		"no type":      `retirements: [{id: a, title: t, retires_on: 2026-01-01, url: u, match: {}}]`,
		"bad severity": `retirements: [{id: a, title: t, retires_on: 2026-01-01, url: u, severity: urgent, match: {type: x}}]`,
		"empty":        `updated: 2026-09-16`,
	}
	for name, y := range cases {
		if _, err := parse([]byte(y)); err == nil {
			t.Errorf("%s: parse succeeded, want an error", name)
		}
	}
}

func TestParseRejectsAmbiguousPredicate(t *testing.T) {
	y := `retirements: [{id: a, title: t, retires_on: 2026-01-01, url: u, match: {type: x, attributes: {k: {in: [1], not: 2}}}}]`
	if _, err := parse([]byte(y)); err == nil {
		t.Error("parse accepted a predicate with both in and not")
	}
}
