package ado

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wes-key/tf-snag/internal/plan"
	"github.com/wes-key/tf-snag/internal/report"
)

// fake is an Azure DevOps stand-in that remembers what was written to it.
type fake struct {
	mu       sync.Mutex
	items    map[int]*WorkItem // by id
	nextID   int
	created  []NewItem
	patched  map[int][]patch
	validate int
}

func newFake(t *testing.T, seed ...WorkItem) (*Client, *fake) {
	t.Helper()
	f := &fake{items: map[int]*WorkItem{}, nextID: 100, patched: map[int][]patch{}}
	for i := range seed {
		it := seed[i]
		f.items[it.ID] = &it
		if it.ID >= f.nextID {
			f.nextID = it.ID + 1
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "wit/wiql"):
			var ids []map[string]int
			for id := range f.items {
				ids = append(ids, map[string]int{"id": id})
			}
			json.NewEncoder(w).Encode(map[string]any{"workItems": ids})

		case strings.Contains(r.URL.Path, "wit/workitemsbatch"):
			var req struct {
				IDs []int `json:"ids"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			var vals []map[string]any
			for _, id := range req.IDs {
				it := f.items[id]
				vals = append(vals, map[string]any{"id": it.ID, "fields": map[string]string{
					"System.State": it.State, "System.Title": it.Title,
					"System.Tags": strings.Join(it.Tags, "; "),
				}})
			}
			json.NewEncoder(w).Encode(map[string]any{"value": vals})

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "wit/workitems/$"):
			var ps []patch
			json.NewDecoder(r.Body).Decode(&ps)
			if strings.Contains(r.URL.RawQuery, "validateOnly=true") {
				f.validate++
				io.WriteString(w, `{"id":0,"fields":{}}`)
				return
			}
			it := WorkItem{ID: f.nextID, State: "New"}
			f.nextID++
			ni := NewItem{}
			for _, p := range ps {
				switch p.Path {
				case "/fields/System.Title":
					it.Title, _ = p.Value.(string)
					ni.Title = it.Title
				case "/fields/System.Tags":
					s, _ := p.Value.(string)
					it.Tags = splitTags(s)
					ni.FindingID = findingIDFromTags(it.Tags)
				}
			}
			f.items[it.ID] = &it
			f.created = append(f.created, ni)
			fmt.Fprintf(w, `{"id":%d,"fields":{"System.State":"New","System.Title":%q}}`, it.ID, it.Title)

		case r.Method == http.MethodPatch:
			var id int
			fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%d", &id)
			var ps []patch
			json.NewDecoder(r.Body).Decode(&ps)
			f.patched[id] = append(f.patched[id], ps...)
			for _, p := range ps {
				if p.Path == "/fields/System.State" {
					if it := f.items[id]; it != nil {
						it.State, _ = p.Value.(string)
					}
				}
			}
			io.WriteString(w, `{"id":0,"fields":{}}`)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "no route", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{OrgURL: srv.URL, Project: "proj", Token: "pat", HTTP: srv.Client()}, f
}

func driftReport(addrs ...string) *report.Report {
	var rc []plan.ResourceChange
	for _, a := range addrs {
		rc = append(rc, plan.ResourceChange{Address: a, Type: "azurerm_x", Change: plan.Change{
			Actions: []string{"update"},
			Before:  map[string]any{"v": 1}, After: map[string]any{"v": 2},
		}})
	}
	return report.Build(&plan.Plan{ResourceDrift: rc})
}

func defaultOpts() Options {
	return Options{Type: "Task", ClosedState: "Closed"}
}

// The whole point of tagging: a second run must recognise its own work items and
// not raise duplicates.
func TestSyncDoesNotDuplicateOnSecondRun(t *testing.T) {
	c, f := newFake(t)

	r1 := driftReport("azurerm_x.a", "azurerm_x.b")
	res1, err := c.Sync(r1, defaultOpts(), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res1.Created) != 2 {
		t.Fatalf("first run created %d, want 2", len(res1.Created))
	}
	if r1.Drift[0].WorkItem == 0 || r1.Drift[0].WorkItemURL == "" {
		t.Errorf("report not stamped with the work item: %+v", r1.Drift[0])
	}

	// Same findings again.
	r2 := driftReport("azurerm_x.a", "azurerm_x.b")
	res2, err := c.Sync(r2, defaultOpts(), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Created) != 0 {
		t.Errorf("second run created %d work items, want 0", len(res2.Created))
	}
	if res2.Existing != 2 {
		t.Errorf("second run linked %d existing, want 2", res2.Existing)
	}
	if len(f.created) != 2 {
		t.Errorf("server saw %d creates in total, want 2", len(f.created))
	}
	// And the second run still links the finding to the item raised on the first.
	if r2.Drift[0].WorkItem != r1.Drift[0].WorkItem {
		t.Errorf("run 2 linked #%d, run 1 raised #%d", r2.Drift[0].WorkItem, r1.Drift[0].WorkItem)
	}
}

// An ignored finding is one somebody has already decided not to act on, so
// raising work for it would be perverse.
func TestSyncSkipsSuppressedFindings(t *testing.T) {
	c, _ := newFake(t)
	r := driftReport("azurerm_x.a", "azurerm_x.b")
	r.Drift[1].Suppressed = true
	r.Drift[1].SuppressReason = "known"

	res, err := c.Sync(r, defaultOpts(), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 {
		t.Errorf("created %d, want 1 (the suppressed finding skipped)", len(res.Created))
	}
	if r.Drift[1].WorkItem != 0 {
		t.Errorf("suppressed finding got work item #%d", r.Drift[1].WorkItem)
	}
}

// eligible gates creation only: a finding that already has an item is always
// linked, so long-standing drift keeps pointing at the item raised on day one.
func TestSyncEligibilityGatesCreationNotLinking(t *testing.T) {
	c, _ := newFake(t)

	// Run 1 raises for both.
	r1 := driftReport("azurerm_x.a", "azurerm_x.b")
	if _, err := c.Sync(r1, defaultOpts(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}

	// Run 2: nothing is eligible, and a third finding appears.
	r2 := driftReport("azurerm_x.a", "azurerm_x.b", "azurerm_x.c")
	res, err := c.Sync(r2, defaultOpts(), func(FindingKind, string) bool { return false }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 0 || res.Skipped != 1 {
		t.Errorf("created=%d skipped=%d, want 0 created and the new finding skipped", len(res.Created), res.Skipped)
	}
	if res.Existing != 2 {
		t.Errorf("existing = %d, want the two known findings still linked", res.Existing)
	}
	for i := 0; i < 2; i++ {
		if r2.Drift[i].WorkItem == 0 {
			t.Errorf("drift[%d] lost its work item link despite being ineligible", i)
		}
	}
}

func TestSyncClosesResolvedFindings(t *testing.T) {
	c, f := newFake(t)

	r1 := driftReport("azurerm_x.a", "azurerm_x.b")
	if _, err := c.Sync(r1, defaultOpts(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	goneID := r1.Drift[1].WorkItem

	// b is fixed; only a is still reported.
	opts := defaultOpts()
	opts.Close = true
	r2 := driftReport("azurerm_x.a")
	res, err := c.Sync(r2, opts, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Closed) != 1 || res.Closed[0].ID != goneID {
		t.Fatalf("closed = %+v, want just #%d", res.Closed, goneID)
	}
	if f.items[goneID].State != "Closed" {
		t.Errorf("work item state = %q, want Closed", f.items[goneID].State)
	}
	// The reason goes in the history so the change is explicable later.
	var sawHistory bool
	for _, p := range f.patched[goneID] {
		if p.Path == "/fields/System.History" {
			sawHistory = true
			if s, _ := p.Value.(string); !strings.Contains(s, "no longer reported") {
				t.Errorf("history note = %q", s)
			}
		}
	}
	if !sawHistory {
		t.Error("closing an item should record why in its history")
	}
	// The item for the finding that is still there must be untouched.
	if stillOpen := r1.Drift[0].WorkItem; f.items[stillOpen].State == "Closed" {
		t.Errorf("closed #%d, whose finding is still reported", stillOpen)
	}
}

// Closing mutates work someone may have triaged, so it only happens when asked.
func TestSyncDoesNotCloseWithoutTheFlag(t *testing.T) {
	c, f := newFake(t)
	r1 := driftReport("azurerm_x.a")
	if _, err := c.Sync(r1, defaultOpts(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	id := r1.Drift[0].WorkItem

	res, err := c.Sync(driftReport(), defaultOpts(), nil, io.Discard) // finding gone
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Closed) != 0 {
		t.Errorf("closed %d items without -ado-close", len(res.Closed))
	}
	if f.items[id].State == "Closed" {
		t.Error("item closed despite Close being false")
	}
}

func TestSyncDryRunChangesNothing(t *testing.T) {
	c, f := newFake(t)
	opts := defaultOpts()
	opts.DryRun = true
	opts.Close = true

	var out strings.Builder
	res, err := c.Sync(driftReport("azurerm_x.a"), opts, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 {
		t.Errorf("dry run should report 1 would-be creation, got %d", len(res.Created))
	}
	if len(f.created) != 0 {
		t.Errorf("dry run created %d work items for real", len(f.created))
	}
	if !strings.Contains(out.String(), "would create Task") {
		t.Errorf("dry run should say what it would do, got:\n%s", out.String())
	}
}

// A deprecation's identity is its summary+detail, so all the sites tripping one
// notice share a single work item.
func TestSyncOneItemPerDeprecation(t *testing.T) {
	c, _ := newFake(t)
	r := &report.Report{Deprecations: []report.Deprecation{{
		Severity: "warning", Summary: "Argument is deprecated", Detail: "use bar",
		Sites: []report.DeprecationSite{
			{Address: "azurerm_x.a", File: "main.tf", Line: 3},
			{Address: "azurerm_x.b", File: "main.tf", Line: 9},
		},
	}}}

	res, err := c.Sync(r, defaultOpts(), nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 {
		t.Errorf("created %d items for one deprecation with two sites, want 1", len(res.Created))
	}
	if r.Deprecations[0].WorkItem == 0 {
		t.Error("deprecation not stamped with its work item")
	}
}

// The item body has to carry enough for someone triaging it a week later.
func TestItemBodyCarriesTheDetail(t *testing.T) {
	rr := driftReport("azurerm_x.a").Drift[0]
	rr.Module = "module.net"
	rr.File, rr.Line = "terraform/main.tf", 12

	body := driftBody(rr, Options{RunURL: "https://run", Context: "nightly · main"})
	for _, want := range []string{"azurerm_x.a", "module.net", "terraform/main.tf:12", "nightly", "https://run"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	// Values are HTML-escaped: a resource name with an angle bracket must not
	// break the description markup. Assert on the address itself — the body's
	// own <b> tags are legitimate markup and would match a naive check.
	esc := driftBody(report.ResourceReport{Address: `a<b>&c`, Action: "update"}, Options{})
	if strings.Contains(esc, `a<b>&c`) {
		t.Errorf("address went into the HTML body unescaped:\n%s", esc)
	}
	if !strings.Contains(esc, `a&lt;b&gt;&amp;c`) {
		t.Errorf("address not escaped as expected:\n%s", esc)
	}
}

func TestTitlesAreDistinctAndClipped(t *testing.T) {
	d := driftTitle(report.ResourceReport{Address: strings.Repeat("a", 400)})
	if len([]rune(d)) > titleMax {
		t.Errorf("drift title is %d runes, over the %d cap", len([]rune(d)), titleMax)
	}
	if !strings.HasPrefix(d, "Terraform drift:") {
		t.Errorf("drift title = %q", d)
	}
	p := deprTitle(report.Deprecation{Summary: "Argument is deprecated"})
	if !strings.HasPrefix(p, "Terraform deprecation:") {
		t.Errorf("deprecation title = %q", p)
	}
}

// Adding an ignore rule for a finding that already has a work item closes that
// item, because a suppressed finding is not in the "still reported" set. That is
// arguably right — you have decided not to act on it — but the history note must
// not claim the finding went away, because it did not.
func TestSyncClosingAnIgnoredFindingExplainsItself(t *testing.T) {
	c, f := newFake(t)

	r1 := driftReport("azurerm_x.a")
	if _, err := c.Sync(r1, defaultOpts(), nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	id := r1.Drift[0].WorkItem

	// Same finding, now suppressed by an ignore rule.
	opts := defaultOpts()
	opts.Close = true
	r2 := driftReport("azurerm_x.a")
	r2.Drift[0].Suppressed = true
	r2.Drift[0].SuppressReason = "temp tag — JIRA-123"

	res, err := c.Sync(r2, opts, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Closed) != 1 {
		t.Fatalf("closed %d items, want the ignored finding's item closed", len(res.Closed))
	}
	if f.items[id].State != "Closed" {
		t.Errorf("state = %q, want Closed", f.items[id].State)
	}

	var note string
	for _, p := range f.patched[id] {
		if p.Path == "/fields/System.History" {
			note, _ = p.Value.(string)
		}
	}
	if strings.Contains(note, "no longer reported") {
		t.Errorf("history claims the finding went away, but it was ignored: %q", note)
	}
	if !strings.Contains(strings.ToLower(note), "ignore") {
		t.Errorf("history should say the finding is now ignored, got: %q", note)
	}
}

// Ignoring a finding closes its item; removing the rule must open a fresh one.
// Without that, the drift is live, actionable and tracked by nothing — the item
// having been closed on the way in.
func TestSyncUnignoredFindingGetsANewItem(t *testing.T) {
	c, f := newFake(t)
	opts := defaultOpts()
	opts.Close = true

	// Run 1: reported, item raised.
	r1 := driftReport("azurerm_x.a")
	if _, err := c.Sync(r1, opts, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	first := r1.Drift[0].WorkItem

	// Run 2: an ignore rule is added. The item closes.
	r2 := driftReport("azurerm_x.a")
	r2.Drift[0].Suppressed = true
	if _, err := c.Sync(r2, opts, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.items[first].State != "Closed" {
		t.Fatalf("item state = %q, want Closed once ignored", f.items[first].State)
	}

	// Run 3: the rule is removed. A closed item does not track a live finding,
	// so a fresh one is raised rather than linking back to the closed one.
	r3 := driftReport("azurerm_x.a")
	r3.Drift[0].Unsuppressed = true
	res, err := c.Sync(r3, opts, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("created %d, want a fresh item for the unignored finding", len(res.Created))
	}
	second := r3.Drift[0].WorkItem
	if second == 0 || second == first {
		t.Errorf("linked #%d, want a new item distinct from the closed #%d", second, first)
	}
	if f.items[first].State != "Closed" {
		t.Errorf("the original item should stay closed as the record of that period")
	}
	// ...and the new item must not immediately close itself in the same pass.
	if f.items[second].State == "Closed" {
		t.Errorf("the freshly raised item was closed in the same run")
	}
}

// An item still open is linked, not duplicated — the closed-item rule must not
// leak into the ordinary path.
func TestSyncOpenItemIsStillReused(t *testing.T) {
	c, _ := newFake(t)
	opts := defaultOpts()
	opts.Close = true

	r1 := driftReport("azurerm_x.a")
	if _, err := c.Sync(r1, opts, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	r2 := driftReport("azurerm_x.a")
	res, err := c.Sync(r2, opts, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) != 0 || res.Existing != 1 {
		t.Errorf("created=%d existing=%d, want the open item reused", len(res.Created), res.Existing)
	}
	if r2.Drift[0].WorkItem != r1.Drift[0].WorkItem {
		t.Errorf("linked #%d, want the original #%d", r2.Drift[0].WorkItem, r1.Drift[0].WorkItem)
	}
}

func TestProvenanceBadgeDistinguishesUnignoredFromNew(t *testing.T) {
	newly := provenanceBadge("new", false, "")
	if !strings.Contains(newly, ">New<") {
		t.Errorf("new finding badge = %q", newly)
	}
	un := provenanceBadge("updated", true, "2026-08-20T06:00:00Z")
	if !strings.Contains(un, "No longer ignored") {
		t.Errorf("unignored badge = %q", un)
	}
	// It keeps the real age: the item is new, the finding is not.
	if !strings.Contains(un, "first detected 2026-08-20") {
		t.Errorf("unignored badge dropped the age, misreporting how long the drift has been there: %q", un)
	}
	if strings.Contains(un, ">New<") {
		t.Errorf("unignored finding should not claim to be newly detected: %q", un)
	}
}
