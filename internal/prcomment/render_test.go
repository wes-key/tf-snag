package prcomment

import (
	"strings"
	"testing"

	"github.com/wes-key/tf-snag/internal/plan"
	"github.com/wes-key/tf-snag/internal/report"
)

func TestRenderCarriesTheMarkerAndTheRun(t *testing.T) {
	got := Render(&report.Report{}, Options{Context: "nightly-drift · main", RunURL: "https://example/run/1"})

	for _, want := range []string{
		Marker,
		"nothing to flag",
		"nightly-drift · main",
		"[this run](https://example/run/1)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("comment is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderListsEachKindOfFinding(t *testing.T) {
	rep := &report.Report{
		Drift: []report.ResourceReport{{
			Address:       "azurerm_storage_account.data",
			Action:        "Updated",
			BaselineState: "new",
			Attrs: []plan.AttrDiff{
				{Path: "min_tls_version", Old: "TLS1_2", New: "TLS1_0"},
			},
		}},
		Deprecations: []report.Deprecation{{
			Summary: "Argument is deprecated",
			Detail:  "`live_trace_enabled` has been\n  superseded",
			Sites:   []report.DeprecationSite{{Address: "azurerm_signalr_service.a", File: "main.tf", Line: 12}},
		}},
		Retirements: []report.Retirement{{
			Title:     "Basic SKU load balancers retire",
			RetiresOn: "2025-09-30",
			URL:       "https://learn.microsoft.com/lb",
			Days:      -352,
			Urgency:   "retired",
			Instances: []report.RetirementInstance{{Address: "azurerm_lb.legacy"}},
		}},
	}

	got := Render(rep, Options{})

	for _, want := range []string{
		"1 resource changed outside Terraform, 1 deprecation, 1 retirement",
		"**`azurerm_storage_account.data`** — updated — **new**",
		"`min_tls_version`: `\"TLS1_2\"` → `\"TLS1_0\"`",
		"**Argument is deprecated**",
		"`live_trace_enabled` has been superseded", // collapsed onto one line
		"`azurerm_signalr_service.a` (`main.tf:12`)",
		"[Basic SKU load balancers retire](https://learn.microsoft.com/lb)",
		"**retired 11 months ago** (2025-09-30)",
		"`azurerm_lb.legacy`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("comment is missing %q:\n%s", want, got)
		}
	}
}

// An ignored finding is somebody's decision already taken. A reviewer should see
// that it exists, not have to read it again.
func TestRenderCountsIgnoredFindingsWithoutListingThem(t *testing.T) {
	rep := &report.Report{
		Drift: []report.ResourceReport{
			{Address: "azurerm_key_vault.waived", Action: "Updated", Suppressed: true, SuppressReason: "PIM"},
		},
	}

	got := Render(rep, Options{})

	if !strings.Contains(got, "1 finding suppressed by an ignore rule") {
		t.Errorf("suppressed finding is not counted:\n%s", got)
	}
	if strings.Contains(got, "azurerm_key_vault.waived") {
		t.Errorf("suppressed finding is listed:\n%s", got)
	}
	if !strings.Contains(got, "nothing to flag") {
		t.Errorf("a run with only ignored findings should read as clean:\n%s", got)
	}
}

func TestRenderCapsLongSections(t *testing.T) {
	rep := &report.Report{}
	for i := 0; i < maxPerSection+5; i++ {
		rep.Drift = append(rep.Drift, report.ResourceReport{Address: "azurerm_x.y", Action: "Updated"})
	}

	got := Render(rep, Options{})

	if !strings.Contains(got, "…and 5 more.") {
		t.Errorf("long section is not capped:\n%s", got)
	}
	if n := strings.Count(got, "- **`azurerm_x.y`**"); n != maxPerSection {
		t.Errorf("listed %d findings, want %d", n, maxPerSection)
	}
}

// The urgent ones have to lead: a reviewer reads the first line and stops.
func TestRenderSortsRetirementsByUrgency(t *testing.T) {
	rep := &report.Report{Retirements: []report.Retirement{
		{Title: "Scheduled", RetiresOn: "2028-09-30", Days: 743, BeyondFailWindow: true},
		{Title: "Retired", RetiresOn: "2025-09-30", Days: -352},
		{Title: "Imminent", RetiresOn: "2026-12-01", Days: 75},
	}}

	got := Render(rep, Options{})

	retired := strings.Index(got, "Retired")
	imminent := strings.Index(got, "Imminent")
	scheduled := strings.Index(got, "Scheduled")
	if !(retired < imminent && imminent < scheduled) {
		t.Errorf("retirements are not most-urgent-first:\n%s", got)
	}
	if !strings.Contains(got, "outside the fail window") {
		t.Errorf("a deferred retirement should say it does not gate:\n%s", got)
	}
}

func TestRenderBadgesAFindingNoLongerIgnored(t *testing.T) {
	rep := &report.Report{Drift: []report.ResourceReport{{
		Address:      "azurerm_lb.a",
		Action:       "Updated",
		Unsuppressed: true,
		FirstSeen:    "2026-08-31T09:12:00Z",
	}}}

	got := Render(rep, Options{})

	if !strings.Contains(got, "**no longer ignored**") {
		t.Errorf("unsuppressed finding is not badged:\n%s", got)
	}
	if strings.Contains(got, "**new**") {
		t.Errorf("unsuppressed is not the same as new:\n%s", got)
	}
}

func TestRenderShowsFirstSeenAsADate(t *testing.T) {
	rep := &report.Report{Drift: []report.ResourceReport{{
		Address:       "azurerm_lb.a",
		Action:        "Updated",
		BaselineState: "updated",
		FirstSeen:     "2026-08-31T09:12:00Z",
	}}}

	if got := Render(rep, Options{}); !strings.Contains(got, "first seen 2026-08-31") {
		t.Errorf("first-seen date missing or not trimmed:\n%s", got)
	}
}

func TestSpan(t *testing.T) {
	for _, tc := range []struct {
		days int
		want string
	}{
		{1, "1 day"},
		{45, "45 days"},
		{90, "3 months"},
		{729, "24 months"},
		{730, "2 years"},
	} {
		if got := span(tc.days); got != tc.want {
			t.Errorf("span(%d) = %q, want %q", tc.days, got, tc.want)
		}
	}
}

func TestShortClipsOnRunes(t *testing.T) {
	long := strings.Repeat("é", 100)
	got := short(long)
	if r := []rune(got); len(r) != 61 || r[60] != '…' {
		t.Errorf("short() clipped to %d runes, want 60 plus an ellipsis", len([]rune(got)))
	}
}

// Under `new` the comment is about the change, not the estate. Listing findings
// the target branch already carries is how a comment becomes wallpaper.
func TestRenderOnlyNewListsWhatThePullRequestAdds(t *testing.T) {
	rep := &report.Report{
		Drift: []report.ResourceReport{
			{Address: "azurerm_lb.carried_over", Action: "Updated", BaselineState: "updated"},
			{Address: "azurerm_lb.brand_new", Action: "Updated", BaselineState: "new"},
			{Address: "azurerm_lb.unignored", Action: "Updated", BaselineState: "updated", Unsuppressed: true},
		},
		Retirements: []report.Retirement{
			{Title: "Old news", RetiresOn: "2025-09-30", Days: -352, BaselineState: "updated"},
			{Title: "Just appeared", RetiresOn: "2026-06-30", Days: -79, BaselineState: "new"},
		},
	}

	got := Render(rep, Options{OnlyNew: true})

	for _, want := range []string{
		"azurerm_lb.brand_new",
		"azurerm_lb.unignored", // no longer ignored counts as new
		"Just appeared",
		"2 findings already on the target branch, not listed here.",
		"new in this pull request",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("comment is missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"azurerm_lb.carried_over", "Old news"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("comment lists %q, which the target branch already has:\n%s", unwanted, got)
		}
	}
}

func TestRenderOnlyNewWithNothingNew(t *testing.T) {
	rep := &report.Report{Drift: []report.ResourceReport{
		{Address: "azurerm_lb.carried_over", Action: "Updated", BaselineState: "updated"},
	}}

	got := Render(rep, Options{OnlyNew: true})

	if !strings.Contains(got, "nothing new in this pull request") {
		t.Errorf("headline should say nothing is new:\n%s", got)
	}
	if !strings.Contains(got, "1 finding already on the target branch") {
		t.Errorf("the carried-over finding should still be counted:\n%s", got)
	}
}

// Without OnlyNew the comment is the whole picture, and must not claim otherwise.
func TestRenderWithoutOnlyNewListsEverything(t *testing.T) {
	rep := &report.Report{Drift: []report.ResourceReport{
		{Address: "azurerm_lb.carried_over", Action: "Updated", BaselineState: "updated"},
		{Address: "azurerm_lb.brand_new", Action: "Updated", BaselineState: "new"},
	}}

	got := Render(rep, Options{})

	if !strings.Contains(got, "azurerm_lb.carried_over") || !strings.Contains(got, "azurerm_lb.brand_new") {
		t.Errorf("both findings should be listed:\n%s", got)
	}
	if strings.Contains(got, "new in this pull request") || strings.Contains(got, "already on the target branch") {
		t.Errorf("comment should not claim to be filtered:\n%s", got)
	}
}
