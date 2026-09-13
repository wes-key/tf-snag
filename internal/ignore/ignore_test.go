package ignore

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wes-key/tf-snag/internal/report"
)

// A bare `# tf-snag:ignore` is filed under both checks. The register has to
// report it once, with both scopes - not as two lookalike rules.
func TestExceptionsFoldsBothScopesIntoOneEntry(t *testing.T) {
	src := `
resource "azurerm_key_vault" "vault" {
  # tf-snag:ignore reason: owned by the security baseline
  name = "x"
}
`
	s := &Set{}
	scanFile(strings.NewReader(src), "main.tf", s)

	got := s.Exceptions()
	if len(got) != 1 {
		t.Fatalf("Exceptions() = %d entries, want 1: %+v", len(got), got)
	}
	if want := []string{"drift", "deprecation"}; !reflect.DeepEqual(got[0].Scopes, want) {
		t.Errorf("scopes = %v, want %v", got[0].Scopes, want)
	}
	if got[0].Reason != "owned by the security baseline" {
		t.Errorf("reason = %q", got[0].Reason)
	}
	if !got[0].Stale() {
		t.Error("a rule that has suppressed nothing should read as stale")
	}
}

func TestExceptionsRecordsWhatEachRuleSuppressed(t *testing.T) {
	src := `
# tf-snag:ignore-drift reason: reconciled by the ingest job
resource "azurerm_storage_container" "test" {
  name = "x"
}

resource "azurerm_key_vault" "vault" {
  # tf-snag:ignore-drift reason: nothing drifts here
  name = "y"
}
`
	s := &Set{}
	scanFile(strings.NewReader(src), "main.tf", s)

	rep := &report.Report{Drift: []report.ResourceReport{
		{Address: "module.sa[0].azurerm_storage_container.test"},
	}}
	s.Apply(rep)

	got := s.Exceptions()
	if len(got) != 2 {
		t.Fatalf("Exceptions() = %d entries, want 2", len(got))
	}
	if got[0].Stale() {
		t.Errorf("rule that matched should not be stale: %+v", got[0])
	}
	if len(got[0].Hits) != 1 || got[0].Hits[0].Address != "module.sa[0].azurerm_storage_container.test" {
		t.Errorf("hits = %+v", got[0].Hits)
	}
	if got[0].Hits[0].Kind != "drift" {
		t.Errorf("hit kind = %q, want drift", got[0].Hits[0].Kind)
	}
	if !got[1].Stale() {
		t.Errorf("rule that matched nothing should be stale: %+v", got[1])
	}
}

// Merge renumbers, or the file set and the source set - both numbered from 0 -
// would collide and report each other's hits.
func TestMergeKeepsRuleIdentitiesDistinct(t *testing.T) {
	a := &Set{}
	scanFile(strings.NewReader("# tf-snag:ignore-drift reason: a\nresource \"t\" \"one\" {\n}\n"), "a.tf", a)
	b := &Set{}
	scanFile(strings.NewReader("# tf-snag:ignore-drift reason: b\nresource \"t\" \"two\" {\n}\n"), "b.tf", b)

	a.Merge(b)
	rep := &report.Report{Drift: []report.ResourceReport{{Address: "t.two"}}}
	a.Apply(rep)

	got := a.Exceptions()
	if len(got) != 2 {
		t.Fatalf("Exceptions() = %d, want 2", len(got))
	}
	if !got[0].Stale() {
		t.Errorf("rule a matched nothing but reports hits: %+v", got[0].Hits)
	}
	if got[1].Stale() || got[1].Reason != "b" {
		t.Errorf("rule b should own the hit, got %+v", got[1])
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"azurerm_role_assignment.foo", "azurerm_role_assignment.foo", true},
		{"azurerm_role_assignment.foo", "azurerm_role_assignment.bar", false},
		{"azurerm_role_assignment.*", "azurerm_role_assignment.bar", true},
		{"*.azurerm_key_vault.vault", "module.kv[0].azurerm_key_vault.vault", true},
		{"module.storage[*].azurerm_storage_account.sa", "module.storage[3].azurerm_storage_account.sa", true},
		{"module.storage[*].azurerm_storage_account.sa", "module.storage[].azurerm_storage_account.sa", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestTypeName(t *testing.T) {
	for in, want := range map[string]string{
		"azurerm_x.y":                        "azurerm_x.y",
		"module.a.azurerm_x.y":               "azurerm_x.y",
		"module.a[0].azurerm_x.y[3]":         "azurerm_x.y",
		"module.a.module.b.azurerm_x.y":      "azurerm_x.y",
		"data.azurerm_client_config.current": "azurerm_client_config.current",
		"single":                             "single",
	} {
		if got := typeName(in); got != want {
			t.Errorf("typeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".tf-snag-ignore.yml")
	os.WriteFile(p, []byte(`
drift:
  - address: "azurerm_storage_account.data"
    reason: "temp tag"
  - address: "azurerm_role_assignment.*"
deprecations:
  - match: "LIVE_TRACE_ENABLED"
    reason: "4.0 upgrade"
`), 0o644)

	s, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.drift) != 2 || len(s.depr) != 1 {
		t.Fatalf("drift=%d depr=%d", len(s.drift), len(s.depr))
	}
	if s.drift[0].Reason != "temp tag" || s.drift[0].Src != ".tf-snag-ignore.yml" {
		t.Errorf("drift[0] = %+v", s.drift[0])
	}
	if s.depr[0].Match != "live_trace_enabled" { // lower-cased on load
		t.Errorf("match = %q, want lower-cased", s.depr[0].Match)
	}
}

func TestLoadFileEmptyAndMissing(t *testing.T) {
	if s, err := LoadFile(""); err != nil || !s.Empty() {
		t.Errorf("empty path: %+v %v", s, err)
	}
	if _, err := LoadFile(filepath.Join(t.TempDir(), "nope.yml")); err == nil {
		t.Error("missing explicit file should error")
	}
}

func TestFromSource(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`
# tf-snag:ignore-drift  reason: managed by PIM
resource "azurerm_role_assignment" "admin" {
  scope = "x"
}

resource "azurerm_signalr_service" "legacy" {
  # tf-snag:ignore-deprecation reason: 4.0 backlog
  live_trace_enabled = true
}

resource "azurerm_storage_account" "sa" {
  # tf-snag:ignore
  min_tls_version = "TLS1_0"
}
`), 0o644)

	s, err := FromSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	// drift rules: role_assignment (above-decl), storage_account (bare ignore)
	if len(s.drift) != 2 {
		t.Fatalf("drift rules = %+v", s.drift)
	}
	// depr rules: signalr (typed), storage_account (bare ignore)
	if len(s.depr) != 2 {
		t.Fatalf("depr rules = %+v", s.depr)
	}
	var ra *Rule
	for i := range s.drift {
		if s.drift[i].Addr == "azurerm_role_assignment.admin" {
			ra = &s.drift[i]
		}
	}
	if ra == nil || !ra.InSrc || ra.Reason != "managed by PIM" || !strings.HasPrefix(ra.Src, "main.tf:") {
		t.Errorf("role_assignment rule = %+v", ra)
	}
}

func TestApplyDriftAndDeprecations(t *testing.T) {
	fileSet, _ := LoadFile(writeTmp(t, `
drift:
  - address: "azurerm_storage_account.*"
    reason: "temp tag"
deprecations:
  - match: "foo"
    reason: "tracked in JIRA"
`))

	r := &report.Report{
		Drift: []report.ResourceReport{
			{Address: "module.s[0].azurerm_storage_account.data", Action: "update"},
			{Address: "azurerm_key_vault.vault", Action: "delete"},
		},
		Deprecations: []report.Deprecation{
			{Summary: "Argument is deprecated", Detail: `The "foo" argument is deprecated.`},
			{Summary: "Deprecated attribute", Detail: "unrelated"},
		},
	}
	fileSet.Apply(r)

	if !r.Drift[0].Suppressed || r.Drift[0].SuppressKind != "external" || r.Drift[0].SuppressReason != "temp tag" {
		t.Errorf("drift[0] = %+v", r.Drift[0])
	}
	if r.Drift[1].Suppressed {
		t.Errorf("drift[1] should not be suppressed")
	}
	if !r.Deprecations[0].Suppressed || r.Deprecations[1].Suppressed {
		t.Errorf("depr suppression = %v / %v", r.Deprecations[0].Suppressed, r.Deprecations[1].Suppressed)
	}
	if !r.HasGatingFindings() { // key_vault + "Deprecated attribute" still gate
		t.Error("expected key_vault drift + 'Deprecated attribute' to still gate")
	}
}

func TestApplyInlineByLineProximity(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`resource "azurerm_x" "y" {
  # tf-snag:ignore-deprecation reason: here
  bad_arg = true
}
`), 0o644)
	s, _ := FromSource(dir)

	r := &report.Report{Deprecations: []report.Deprecation{{
		Summary: "Argument is deprecated",
		Sites:   []report.DeprecationSite{{Address: "azurerm_x.y", File: "main.tf", Line: 3}},
	}}}
	s.Apply(r)
	if !r.Deprecations[0].Suppressed || r.Deprecations[0].SuppressKind != "inSource" {
		t.Errorf("inline depr not suppressed: %+v", r.Deprecations[0])
	}
}

func writeTmp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".tf-snag-ignore.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
