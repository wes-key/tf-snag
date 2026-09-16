package retire

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Predicate is one attribute test. Authors write the common case as a bare
// scalar - `sku: Basic` - and reach for a mapping only when they need a set or
// a negation:
//
//	sku: Basic                    # equals
//	account_kind: {in: [Storage, BlobStorage]}
//	min_tls_version: {not: TLS1_2}
type Predicate struct {
	Equals any
	In     []any
	Not    any

	kind predicateKind
}

type predicateKind int

const (
	predEquals predicateKind = iota
	predIn
	predNot
)

// UnmarshalYAML accepts either a scalar (equality) or a one-key mapping.
func (p *Predicate) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var v any
		if err := node.Decode(&v); err != nil {
			return err
		}
		p.Equals, p.kind = v, predEquals
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("attribute test must be a value or a {in: [...]} / {not: value} mapping")
	}
	var form struct {
		In  []any `yaml:"in"`
		Not any   `yaml:"not"`
	}
	if err := node.Decode(&form); err != nil {
		return err
	}
	switch {
	case form.In != nil && form.Not != nil:
		return fmt.Errorf("attribute test has both in and not; use one")
	case form.In != nil:
		p.In, p.kind = form.In, predIn
	case form.Not != nil:
		p.Not, p.kind = form.Not, predNot
	default:
		return fmt.Errorf("attribute test mapping needs in or not")
	}
	return nil
}

// test reports whether any of the values found at an attribute path satisfies
// the predicate. Absent attributes (no candidates) never match, including for
// `not`: tf-snag will not claim a resource is affected on the strength of an
// attribute the plan does not carry.
func (p Predicate) test(candidates []any) bool {
	if len(candidates) == 0 {
		return false
	}
	for _, got := range candidates {
		switch p.kind {
		case predEquals:
			if sameValue(got, p.Equals) {
				return true
			}
		case predIn:
			for _, want := range p.In {
				if sameValue(got, want) {
					return true
				}
			}
		case predNot:
			if !sameValue(got, p.Not) {
				return true
			}
		}
	}
	return false
}

// sameValue compares a plan value with a catalogue value. Strings compare
// case-insensitively, because Azure and the provider disagree about case
// ("Basic" vs "basic") often enough that a case-sensitive match would miss real
// findings. Numbers compare by value, so 2 in YAML matches 2.0 from JSON.
func sameValue(got, want any) bool {
	if gs, ok := got.(string); ok {
		if ws, ok := want.(string); ok {
			return strings.EqualFold(strings.TrimSpace(gs), strings.TrimSpace(ws))
		}
	}
	if gf, ok := toFloat(got); ok {
		if wf, ok := toFloat(want); ok {
			return gf == wf
		}
	}
	return got == want
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// lookup resolves a dotted attribute path against a resource's values and
// returns every value it finds. A path segment that lands on a list fans out
// across its elements, so `sku.name` matches whether the provider models sku as
// a map or as a single-element block list - which azurerm does inconsistently,
// and differently between provider majors.
func lookup(values map[string]any, path string) []any {
	current := []any{any(values)}
	for _, segment := range strings.Split(path, ".") {
		var next []any
		for _, node := range current {
			next = append(next, step(node, segment)...)
		}
		if len(next) == 0 {
			return nil
		}
		current = next
	}
	return current
}

func step(node any, segment string) []any {
	switch n := node.(type) {
	case map[string]any:
		if v, ok := n[segment]; ok {
			return []any{v}
		}
	case []any:
		// An explicit index picks one element; anything else fans out.
		if i, err := strconv.Atoi(segment); err == nil {
			if i >= 0 && i < len(n) {
				return []any{n[i]}
			}
			return nil
		}
		var out []any
		for _, item := range n {
			out = append(out, step(item, segment)...)
		}
		return out
	}
	return nil
}

// Date is a calendar date that reads as YYYY-MM-DD in YAML.
type Date struct{ time.Time }

func (d *Date) UnmarshalYAML(node *yaml.Node) error {
	if node.Value == "" {
		return nil
	}
	t, err := time.Parse(DateLayout, node.Value)
	if err != nil {
		return fmt.Errorf("date %q is not YYYY-MM-DD", node.Value)
	}
	d.Time = t
	return nil
}

func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.Format(DateLayout)
}
