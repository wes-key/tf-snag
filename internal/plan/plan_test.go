package plan

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

func TestActionCollapsesActionsArray(t *testing.T) {
	cases := []struct {
		want    string
		actions []string
	}{
		{"no-op", []string{"no-op"}},
		{"create", []string{"create"}},
		{"update", []string{"update"}},
		{"delete", []string{"delete"}},
		{"replace", []string{"create", "delete"}},
		{"replace", []string{"delete", "create"}},
	}
	for _, c := range cases {
		if got := (Change{Actions: c.actions}).Action(); got != c.want {
			t.Errorf("Action(%v) = %q, want %q", c.actions, got, c.want)
		}
	}
}

func TestParseRejectsNonPlanJSON(t *testing.T) {
	if _, err := Parse([]byte(`{"hello":"world"}`)); err == nil {
		t.Fatal("expected error for JSON without format_version")
	}
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseReadsDriftAndVersion(t *testing.T) {
	raw := []byte(`{
	  "format_version": "1.2",
	  "terraform_version": "1.9.6",
	  "resource_drift": [
	    {"address":"azurerm_x.y","module_address":"module.m",
	     "change":{"actions":["update"],"before":{"a":1},"after":{"a":2}}}
	  ],
	  "resource_changes": [
	    {"address":"azurerm_x.y","change":{"actions":["no-op"],"before":{},"after":{}}}
	  ]
	}`)
	p, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.TerraformVersion != "1.9.6" {
		t.Errorf("TerraformVersion = %q", p.TerraformVersion)
	}
	if len(p.ResourceDrift) != 1 || p.ResourceDrift[0].Address != "azurerm_x.y" {
		t.Fatalf("drift not parsed: %+v", p.ResourceDrift)
	}
	if p.ResourceDrift[0].ModuleAddress != "module.m" {
		t.Errorf("ModuleAddress = %q", p.ResourceDrift[0].ModuleAddress)
	}
}

func TestParseHandlesBOMsAndUTF16(t *testing.T) {
	plain := []byte(`{"format_version":"1.2","terraform_version":"1.9.6","resource_drift":[],"resource_changes":[]}`)

	utf16le := func(b []byte) []byte {
		u := utf16.Encode([]rune(string(b)))
		out := make([]byte, 2+len(u)*2)
		out[0], out[1] = 0xFF, 0xFE
		for i, r := range u {
			binary.LittleEndian.PutUint16(out[2+i*2:], r)
		}
		return out
	}
	utf16be := func(b []byte) []byte {
		u := utf16.Encode([]rune(string(b)))
		out := make([]byte, 2+len(u)*2)
		out[0], out[1] = 0xFE, 0xFF
		for i, r := range u {
			binary.BigEndian.PutUint16(out[2+i*2:], r)
		}
		return out
	}

	cases := map[string][]byte{
		"utf8 no bom": plain,
		"utf8 bom":    append([]byte{0xEF, 0xBB, 0xBF}, plain...),
		"utf16 le":    utf16le(plain), // what PowerShell's > redirection writes
		"utf16 be":    utf16be(plain),
	}
	for name, raw := range cases {
		p, err := Parse(raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if p.TerraformVersion != "1.9.6" {
			t.Errorf("%s: TerraformVersion = %q", name, p.TerraformVersion)
		}
	}
}
