package wiki

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wes-key/tf-snag/internal/ado"
	"github.com/wes-key/tf-snag/internal/ignore"
	"github.com/wes-key/tf-snag/internal/report"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// exception builds an ignore.Exception the way Set.Exceptions would. Rule's
// identity fields are unexported, which is fine: nothing here reads them.
func exception(addr, match, reason, src string, inSrc bool, scopes []string, hits ...ignore.Hit) ignore.Exception {
	return ignore.Exception{
		Rule:   ignore.Rule{Addr: addr, Match: match, Reason: reason, Src: src, InSrc: inSrc},
		Scopes: scopes,
		Hits:   hits,
	}
}

func TestRenderSplitsLiveFromStale(t *testing.T) {
	exs := []ignore.Exception{
		exception("azurerm_storage_container.test", "", "reconciled by the ingest job", "modules/sa/main.tf:11", true,
			[]string{"drift"}, ignore.Hit{Kind: "drift", Address: "module.sa[0].azurerm_storage_container.test"}),
		exception("azurerm_resource_group.rg", "", "tags reconciled by policy", ".tf-snag-ignore.yml", false,
			[]string{"drift"}),
	}
	rep := &report.Report{Drift: []report.ResourceReport{
		{Address: "module.sa[0].azurerm_storage_container.test", FirstSeen: "2026-08-21T06:00:00Z"},
	}}

	got := Render(exs, rep, Options{Now: at("2026-09-13T06:00:00Z")})

	if !strings.Contains(got, "**2 exceptions** · 1 in effect · **1 suppressing nothing**") {
		t.Errorf("summary line wrong:\n%s", got)
	}
	live, stale, ok := strings.Cut(got, "## Suppressing nothing")
	if !ok {
		t.Fatalf("no stale section:\n%s", got)
	}
	if !strings.Contains(live, "azurerm_storage_container.test") {
		t.Error("matched rule missing from the In effect table")
	}
	if strings.Contains(live, "azurerm_resource_group.rg") {
		t.Error("a rule suppressing nothing must not be listed as in effect")
	}
	if !strings.Contains(stale, "azurerm_resource_group.rg") {
		t.Error("unmatched rule missing from the stale table")
	}
	// The age is the point of the column: how long the estate has carried it.
	if !strings.Contains(live, "2026-08-21 (23 days)") {
		t.Errorf("first-seen age missing or wrong:\n%s", live)
	}
	if !strings.Contains(live, "(inline)") {
		t.Error("inline rules should be distinguishable from file rules")
	}
}

func TestRenderNoRules(t *testing.T) {
	got := Render(nil, &report.Report{}, Options{Now: at("2026-09-13T06:00:00Z")})
	if !strings.Contains(got, "_No ignore rules are defined._") {
		t.Errorf("empty register should say so:\n%s", got)
	}
	if strings.Contains(got, "## Suppressing nothing") {
		t.Error("no rules means no stale section")
	}
}

// A reason is free text from a .tf comment or YAML. A pipe in it would split the
// table row and silently corrupt every column after it.
func TestRenderEscapesTableBreakingText(t *testing.T) {
	exs := []ignore.Exception{
		exception("azurerm_x.y", "", "waived | see DJCS-1 | and DJCS-2", "a.tf:1", true, []string{"drift"},
			ignore.Hit{Kind: "drift", Address: "azurerm_x.y"}),
	}
	got := Render(exs, &report.Report{}, Options{Now: at("2026-09-13T06:00:00Z")})
	if strings.Contains(got, "waived | see") {
		t.Errorf("pipe not escaped:\n%s", got)
	}
	if !strings.Contains(got, `waived \| see DJCS-1 \| and DJCS-2`) {
		t.Errorf("escaped reason missing:\n%s", got)
	}
}

func TestRenderFlagsMissingReason(t *testing.T) {
	exs := []ignore.Exception{exception("azurerm_x.y", "", "  ", "a.tf:1", true, []string{"drift"},
		ignore.Hit{Kind: "drift", Address: "azurerm_x.y"})}
	got := Render(exs, &report.Report{}, Options{Now: at("2026-09-13T06:00:00Z")})
	if !strings.Contains(got, "_no reason given_") {
		t.Errorf("an unexplained waiver should say so:\n%s", got)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	exs := []ignore.Exception{
		exception("azurerm_x.y", "", "r", "a.tf:1", true, []string{"drift", "deprecation"},
			ignore.Hit{Kind: "drift", Address: "azurerm_x.y"}),
	}
	o := Options{Now: at("2026-09-13T06:00:00Z"), Context: "ctx", RunURL: "https://example/run"}
	if a, b := Render(exs, &report.Report{}, o), Render(exs, &report.Report{}, o); a != b {
		t.Error("same input must render identically, or every run rewrites the page")
	}
}

// --- publishing --------------------------------------------------------------

var testWiki = ado.Wiki{ID: "w-1", Name: "proj.wiki", Branch: "wikiMaster"}

type fakeWiki struct {
	page  Page
	puts  int
	etag  string
	saved string
}

func (f *fakeWiki) Page(w ado.Wiki, path string) (Page, error) { return f.page, nil }
func (f *fakeWiki) Put(w ado.Wiki, path, content, etag string) (bool, error) {
	f.puts++
	f.etag = etag
	f.saved = content
	return etag == "", nil
}

func TestPublishSkipsAnIdenticalPage(t *testing.T) {
	body := Render(nil, &report.Report{}, Options{Now: at("2026-09-13T06:00:00Z")})
	// Same register, rendered an hour later: only the footer differs.
	later := Render(nil, &report.Report{}, Options{Now: at("2026-09-13T07:00:00Z")})

	f := &fakeWiki{page: Page{Content: body, ETag: `"v1"`, Exists: true}}
	var log bytes.Buffer
	res, err := Publish(f, testWiki, "/tf-snag/Exceptions", later, false, &log)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unchanged || f.puts != 0 {
		t.Errorf("a run that changes nothing must not add a wiki revision (puts=%d, res=%+v)", f.puts, res)
	}
	if !strings.Contains(log.String(), "already up to date") {
		t.Errorf("log = %q", log.String())
	}
}

func TestPublishCreatesWhenAbsentAndUpdatesWithETag(t *testing.T) {
	var log bytes.Buffer

	absent := &fakeWiki{page: Page{Exists: false}}
	res, err := Publish(absent, testWiki, "/p", "new content", false, &log)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || absent.puts != 1 || absent.etag != "" {
		t.Errorf("create: res=%+v puts=%d etag=%q", res, absent.puts, absent.etag)
	}

	existing := &fakeWiki{page: Page{Content: "old", ETag: `"v7"`, Exists: true}}
	res, err = Publish(existing, testWiki, "/p", "new content", false, &log)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || existing.puts != 1 {
		t.Errorf("update: res=%+v puts=%d", res, existing.puts)
	}
	// Without If-Match the write is refused, or worse, clobbers a concurrent edit.
	if existing.etag != `"v7"` {
		t.Errorf("update did not send the ETag back: %q", existing.etag)
	}
}

func TestPublishDryRunWritesNothing(t *testing.T) {
	f := &fakeWiki{page: Page{Content: "old", ETag: `"v1"`, Exists: true}}
	var log bytes.Buffer
	if _, err := Publish(f, testWiki, "/p", "new", true, &log); err != nil {
		t.Fatal(err)
	}
	if f.puts != 0 {
		t.Error("dry run wrote to the wiki")
	}
	if !strings.Contains(log.String(), "would be updating") {
		t.Errorf("log = %q", log.String())
	}
}
