// Package plan parses the JSON produced by `terraform show -json PLANFILE`
// into the subset of fields tf-snag needs: what changed outside Terraform
// (resource_drift) and what applying would do (resource_changes).
package plan

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"unicode/utf16"
)

// Plan is a trimmed view of the Terraform plan representation.
// See https://developer.hashicorp.com/terraform/internals/json-format
type Plan struct {
	FormatVersion    string           `json:"format_version"`
	TerraformVersion string           `json:"terraform_version"`
	ResourceChanges  []ResourceChange `json:"resource_changes"`
	ResourceDrift    []ResourceChange `json:"resource_drift"`
	// The whole estate, not just what changed - see state.go.
	PlannedValues *StateValues `json:"planned_values"`
	PriorState    *State       `json:"prior_state"`
}

// ResourceChange describes a single resource's before/after state.
type ResourceChange struct {
	Address       string `json:"address"`
	ModuleAddress string `json:"module_address"`
	Mode          string `json:"mode"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	ProviderName  string `json:"provider_name"`
	Change        Change `json:"change"`
}

// Change holds the action and the before/after attribute maps. Terraform emits
// `before`/`after` as null for creates/deletes, which decode to a nil map.
type Change struct {
	Actions []string       `json:"actions"`
	Before  map[string]any `json:"before"`
	After   map[string]any `json:"after"`
}

// Action collapses Terraform's actions array into a single verb:
// no-op, create, read, update, delete, or replace.
func (c Change) Action() string {
	a := c.Actions
	switch {
	case len(a) == 1:
		return a[0]
	case len(a) == 2 && a[0] == "create" && a[1] == "delete":
		return "replace"
	case len(a) == 2 && a[0] == "delete" && a[1] == "create":
		return "replace"
	default:
		b, _ := json.Marshal(a)
		return string(b)
	}
}

// Parse decodes `terraform show -json` output and sanity-checks that it is a
// plan representation rather than some other JSON.
func Parse(raw []byte) (*Plan, error) {
	raw, err := DecodeUTF(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing plan JSON: %w", err)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parsing plan JSON: %w", err)
	}
	if p.FormatVersion == "" {
		return nil, fmt.Errorf("input has no format_version; expected `terraform show -json PLANFILE` output")
	}
	return &p, nil
}

// DecodeUTF strips a UTF-8 BOM and transcodes UTF-16 (LE or BE, identified by
// its BOM) to UTF-8. PowerShell's `>` and `Out-File` redirection writes UTF-16
// LE with a BOM by default, so `terraform show -json PLANFILE > plan.json` on
// Windows would otherwise hand us bytes Go's JSON decoder cannot read.
func DecodeUTF(raw []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}):
		return raw[3:], nil
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}):
		return utf16ToUTF8(raw[2:], binary.LittleEndian)
	case bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		return utf16ToUTF8(raw[2:], binary.BigEndian)
	default:
		return raw, nil
	}
}

func utf16ToUTF8(b []byte, order binary.ByteOrder) ([]byte, error) {
	if len(b)%2 != 0 {
		return nil, fmt.Errorf("input starts with a UTF-16 BOM but has an odd byte length")
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = order.Uint16(b[i*2:])
	}
	return []byte(string(utf16.Decode(u))), nil
}
