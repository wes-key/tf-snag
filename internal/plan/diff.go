package plan

import (
	"bytes"
	"encoding/json"
	"sort"
)

// AttrDiff is one leaf attribute that differs between before and after.
// Old or New is nil when the key is absent on that side.
type AttrDiff struct {
	Path string `json:"path"`
	Old  any    `json:"old"`
	New  any    `json:"new"`
}

// DiffAttrs returns the leaf attributes that differ between two attribute maps,
// sorted by dotted path. Nested maps are recursed into; slices and other
// composite values are compared whole.
func DiffAttrs(before, after map[string]any) []AttrDiff {
	var out []AttrDiff
	walk("", before, after, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func walk(prefix string, before, after map[string]any, out *[]AttrDiff) {
	seen := make(map[string]struct{}, len(before)+len(after))
	for k := range before {
		seen[k] = struct{}{}
	}
	for k := range after {
		seen[k] = struct{}{}
	}

	for k := range seen {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		bv, bok := before[k]
		av, aok := after[k]

		bm, bIsMap := bv.(map[string]any)
		am, aIsMap := av.(map[string]any)
		if bIsMap && aIsMap {
			walk(path, bm, am, out)
			continue
		}
		if isEmptyish(bv) && isEmptyish(av) {
			continue
		}
		if equalJSON(bv, av) {
			continue
		}
		d := AttrDiff{Path: path}
		if bok {
			d.Old = bv
		}
		if aok {
			d.New = av
		}
		*out = append(*out, d)
	}
}

// isEmptyish reports whether v is one of the interchangeable "no value" forms a
// provider round-trips between refreshes: null, an empty object, or an empty
// array. A leaf where both sides are empty-ish (e.g. `tags: null -> {}`) is
// refresh noise, not a real change.
func isEmptyish(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	default:
		return false
	}
}

func equalJSON(x, y any) bool {
	bx, errx := json.Marshal(x)
	by, erry := json.Marshal(y)
	if errx != nil || erry != nil {
		return false
	}
	return bytes.Equal(bx, by)
}
