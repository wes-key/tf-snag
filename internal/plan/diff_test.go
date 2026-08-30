package plan

import "testing"

func TestDiffAttrsNestedAndMissingKeys(t *testing.T) {
	before := map[string]any{
		"min_tls_version": "TLS1_2",
		"tags":            map[string]any{"owner": "platform"},
		"gone":            "was-here",
	}
	after := map[string]any{
		"min_tls_version": "TLS1_0",
		"tags":            map[string]any{"owner": "platform", "temp": "true"},
		"added":           "now-here",
	}

	got := DiffAttrs(before, after)

	want := map[string]AttrDiff{
		"added":           {Path: "added", Old: nil, New: "now-here"},
		"gone":            {Path: "gone", Old: "was-here", New: nil},
		"min_tls_version": {Path: "min_tls_version", Old: "TLS1_2", New: "TLS1_0"},
		"tags.temp":       {Path: "tags.temp", Old: nil, New: "true"},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d diffs, want %d: %+v", len(got), len(want), got)
	}
	// DiffAttrs sorts by path, so unchanged "tags.owner" must not appear.
	for _, d := range got {
		w, ok := want[d.Path]
		if !ok {
			t.Errorf("unexpected diff at %q", d.Path)
			continue
		}
		if !equalJSON(d.Old, w.Old) || !equalJSON(d.New, w.New) {
			t.Errorf("%s: got %v => %v, want %v => %v", d.Path, d.Old, d.New, w.Old, w.New)
		}
	}
}

func TestDiffAttrsIsSortedByPath(t *testing.T) {
	got := DiffAttrs(
		map[string]any{"z": 1, "a": 1, "m": 1},
		map[string]any{"z": 2, "a": 2, "m": 2},
	)
	prev := ""
	for _, d := range got {
		if prev != "" && d.Path < prev {
			t.Errorf("paths not sorted: %q after %q", d.Path, prev)
		}
		prev = d.Path
	}
}

func TestDiffAttrsNilMaps(t *testing.T) {
	if d := DiffAttrs(nil, nil); len(d) != 0 {
		t.Errorf("nil,nil = %+v, want empty", d)
	}
}

func TestDiffAttrsTreatsEmptyFormsAsEqual(t *testing.T) {
	before := map[string]any{
		"tags":    nil,
		"aliases": nil,
		"real":    "old",
	}
	after := map[string]any{
		"tags":    map[string]any{},
		"aliases": []any{},
		"real":    "new",
	}
	got := DiffAttrs(before, after)
	if len(got) != 1 || got[0].Path != "real" {
		t.Fatalf("want only the 'real' diff, got %+v", got)
	}
}

func TestDiffAttrsEmptyVsPopulatedStillDiffs(t *testing.T) {
	got := DiffAttrs(
		map[string]any{"tags": nil},
		map[string]any{"tags": map[string]any{"owner": "x"}},
	)
	if len(got) != 1 || got[0].Path != "tags" {
		t.Fatalf("want a 'tags' diff, got %+v", got)
	}
}
