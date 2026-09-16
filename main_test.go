package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/wes-key/tf-snag/internal/report"
)

// The Teams webhook is picked up from the environment, so a developer (or an
// agent) with TF_SNAG_TEAMS_WEBHOOK exported would otherwise have every test
// here try to post. Tests that want it set it themselves with t.Setenv.
func TestMain(m *testing.M) {
	os.Unsetenv("TF_SNAG_TEAMS_WEBHOOK")
	os.Exit(m.Run())
}

func TestRunWithPlanFileReportsDriftAndExits2(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json"}, strings.NewReader(""), &out, &errb)

	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	for _, want := range []string{
		"changed outside Terraform",
		"azurerm_storage_account.data",
		"min_tls_version",
		"1 to add, 0 to change, 1 to destroy",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout missing %q\n---\n%s", want, out.String())
		}
	}
}

func TestRunExitCodeFalseStillZeroOnDrift(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-exit-code=false"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
}

func TestRunJSONFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "json"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Errorf("expected JSON object, got: %s", out.String())
	}
}

func TestRunJUnitFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "junit"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{`<testsuites>`, `name="tf-snag"`, `<failure message=`, "azurerm_storage_account.data"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("junit output missing %q\n---\n%s", want, out.String())
		}
	}
}

func TestRunMarkdownFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "markdown"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "🔴") {
		t.Errorf("expected markdown to open with the status line, got: %s", out.String())
	}
}

func TestRunSARIFFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "sarif"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	for _, want := range []string{`"version": "2.1.0"`, `"ruleId": "resource-drift"`, `"name": "tf-snag"`, `"guid"`, `"driftAddress/v1"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("sarif output missing %q\n---\n%s", want, out.String())
		}
	}
	// tf-snag leaves baselineState for the downstream Multitool pass.
	if strings.Contains(out.String(), `"baselineState"`) {
		t.Errorf("baselineState should not be emitted by tf-snag\n%s", out.String())
	}
}

func TestRunVersion(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-version"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, "tf-snag ") || !strings.Contains(got, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("version line = %q", got)
	}
}

// The wordmark must never reach stdout: -version's single line is a contract,
// and every other run puts a machine-readable report there.
func TestRunVersionBannerOnStderrOnly(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"-version"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if lines := strings.Count(strings.TrimSpace(out.String()), "\n"); lines != 0 {
		t.Errorf("stdout should be one line, got %d extra:\n%s", lines, out.String())
	}
	if strings.Contains(out.String(), "█") {
		t.Errorf("wordmark leaked onto stdout:\n%s", out.String())
	}
	if !strings.Contains(errb.String(), "█") {
		t.Errorf("wordmark missing from stderr:\n%s", errb.String())
	}
}

func TestRunUsageShowsBanner(t *testing.T) {
	var out, errb bytes.Buffer
	// An unparseable flag is what sends most people to the usage text.
	if code := run([]string{"-not-a-flag"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "█") {
		t.Errorf("wordmark missing from usage:\n%s", errb.String())
	}
	if !strings.Contains(errb.String(), "usage: terraform show -json") {
		t.Errorf("usage text missing:\n%s", errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("usage wrote to stdout: %q", out.String())
	}
}

func TestRunVersionBannerColor(t *testing.T) {
	// Buffers are never terminals, so auto must stay plain - that is what keeps
	// the banner clean in a pipeline log.
	for _, tc := range []struct {
		color string
		want  bool
	}{{"always", true}, {"never", false}, {"auto", false}} {
		var out, errb bytes.Buffer
		if code := run([]string{"-version", "-color", tc.color}, strings.NewReader(""), &out, &errb); code != 0 {
			t.Fatalf("-color %s: exit = %d", tc.color, code)
		}
		if got := strings.Contains(errb.String(), "\x1b["); got != tc.want {
			t.Errorf("-color %s: coloured = %v, want %v", tc.color, got, tc.want)
		}
	}
}

func TestRunUnknownFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "yaml"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown format") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunStdin(t *testing.T) {
	in := `{"format_version":"1.2","terraform_version":"1.9.6","resource_drift":[],"resource_changes":[]}`
	var out, errb bytes.Buffer
	code := run(nil, strings.NewReader(in), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), "no drift detected") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestRunBadInput(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(nil, strings.NewReader(`{"nope":true}`), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "format_version") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunDeprecationsSARIF(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "deprecations", "-plan-log", "testdata/plan-log-deprecations.jsonl", "-format", "sarif"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{`"id": "resource-drift"`, `"id": "deprecation"`, `"ruleId": "deprecation"`, "Argument is deprecated"} {
		if !strings.Contains(s, want) {
			t.Errorf("sarif missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, `"ruleId": "resource-drift"`) {
		t.Errorf("did not expect any resource-drift results\n%s", s)
	}
	// Without -plan-log-dir the fixture's paths are used verbatim.
	if !strings.Contains(s, `"uri": "main.tf"`) || strings.Contains(s, `"uri": "terraform/main.tf"`) {
		t.Errorf("expected bare fixture paths\n%s", s)
	}
}

func TestRunDeprecationsPlanLogDirPrefixesPaths(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{
		"-check", "deprecations",
		"-plan-log", "testdata/plan-log-deprecations.jsonl",
		"-plan-log-dir", "terraform",
		"-format", "sarif",
	}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{`"uri": "terraform/main.tf"`, `"uri": "terraform/modules/a/main.tf"`} {
		if !strings.Contains(s, want) {
			t.Errorf("sarif missing prefixed path %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, `"uri": "main.tf"`) {
		t.Errorf("unprefixed path leaked through\n%s", s)
	}
	// Message lines carry the prefixed location too.
	if !strings.Contains(s, "(terraform/main.tf:5)") {
		t.Errorf("message location not prefixed\n%s", s)
	}
}

func TestRunPlanLogDirWithoutDeprecations(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan-log-dir", "terraform", "-plan", "testdata/plan-drift.json"},
		strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "-plan-log-dir set but") {
		t.Errorf("exit=%d stderr=%q", code, errb.String())
	}
}

func TestRunBothChecksSARIF(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{
		"-check", "all",
		"-plan", "testdata/plan-drift.json",
		"-plan-log", "testdata/plan-log-deprecations.jsonl",
		"-format", "sarif",
	}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	drift := strings.Index(s, `"ruleId": "resource-drift"`)
	depr := strings.Index(s, `"ruleId": "deprecation"`)
	if drift < 0 || depr < 0 {
		t.Fatalf("want both rule ids in results\n%s", s)
	}
	if drift > depr {
		t.Errorf("resource-drift results should precede deprecation results (%d vs %d)", drift, depr)
	}
}

func TestRunDeprecationsStdin(t *testing.T) {
	raw, err := os.ReadFile("testdata/plan-log-deprecations.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run([]string{"-check", "deprecations", "-format", "sarif"}, bytes.NewReader(raw), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	if !strings.Contains(out.String(), `"ruleId": "deprecation"`) {
		t.Errorf("sarif missing deprecation result\n%s", out.String())
	}
}

func TestRunDeprecationsCleanExitsZero(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "deprecations", "-plan-log", "testdata/plan-log-clean.jsonl"},
		strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "no drift detected") || strings.Contains(s, "deprecation warning(s)") {
		t.Errorf("unexpected text output:\n%s", s)
	}
}

func TestRunDeprecationsRejectsJUnitFormat(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "deprecations", "-plan-log", "testdata/plan-log-clean.jsonl", "-format", "junit"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "sarif, json, teams, text or markdown") {
		t.Errorf("stderr = %q", errb.String())
	}
}

// -check deprecations -format json is now allowed (schema 2 carries them); the
// tf-snag extension consumes it.
func TestRunDeprecationsJSON(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "all", "-plan", "testdata/plan-drift.json",
		"-plan-log", "testdata/plan-log-deprecations.jsonl", "-format", "json"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	var doc struct {
		Schema       int `json:"schema"`
		Deprecations []struct {
			Summary string `json:"summary"`
		} `json:"deprecations"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out.String())
	}
	if doc.Schema != report.ReportSchema {
		t.Errorf("schema = %d, want %d", doc.Schema, report.ReportSchema)
	}
	if len(doc.Deprecations) == 0 {
		t.Errorf("deprecations not carried in JSON: %s", out.String())
	}
}

func TestRunDeprecationsMarkdown(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "all", "-plan", "testdata/plan-drift.json",
		"-plan-log", "testdata/plan-log-deprecations.jsonl", "-format", "markdown"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"deprecation warning", "### Deprecation warnings", "Argument is deprecated"} {
		if !strings.Contains(s, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, s)
		}
	}
}

func TestRunUnknownCheck(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "bogus", "-plan", "testdata/plan-drift.json"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "unknown check") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunPlanLogWithoutDeprecations(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "drift", "-plan", "testdata/plan-drift.json", "-plan-log", "testdata/plan-log-clean.jsonl"},
		strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "plan-log set but") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunBothChecksBothStdin(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-check", "all"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "both the plan and the plan log from stdin") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunDefaultCheckIsDrift(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "sarif"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	s := out.String()
	if !strings.Contains(s, `"ruleId": "resource-drift"`) {
		t.Errorf("expected resource-drift results\n%s", s)
	}
	if strings.Contains(s, `"ruleId": "deprecation"`) {
		t.Errorf("plain drift run should have no deprecation results\n%s", s)
	}
}

func TestRunIgnoreFileSuppressesDrift(t *testing.T) {
	var out, errb bytes.Buffer
	// tf-snag-ignore.yml ignores azurerm_storage_account.data (the only real drift).
	code := run([]string{"-plan", "testdata/plan-drift.json", "-format", "sarif",
		"-ignore", "testdata/tf-snag-ignore.yml"}, strings.NewReader(""), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (all drift suppressed) — stderr: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, `"suppressions"`) || !strings.Contains(s, `"kind": "external"`) {
		t.Errorf("sarif missing suppression on the ignored result:\n%s", s)
	}
}

func TestRunIgnoreFileBadPath(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-plan", "testdata/plan-drift.json", "-ignore", "testdata/nope.yml"},
		strings.NewReader(""), &out, &errb)
	if code != 2 || errb.Len() == 0 {
		t.Errorf("exit=%d stderr=%q, want 2 + an error", code, errb.String())
	}
}

func TestRunInlineIgnoreViaSource(t *testing.T) {
	// A plan whose only drift is on azurerm_role_assignment, which
	// testdata/tfsrc/main.tf marks `# tf-snag:ignore-drift`.
	plan := `{"format_version":"1.2","terraform_version":"1.9.6","resource_drift":[` +
		`{"address":"module.rbac.azurerm_role_assignment.admin[0]","type":"azurerm_role_assignment",` +
		`"change":{"actions":["update"],"before":{"role":"Reader"},"after":{"role":"Owner"}}}],` +
		`"resource_changes":[]}`
	var out, errb bytes.Buffer
	code := run([]string{"-format", "text", "-source", "testdata/tfsrc"}, strings.NewReader(plan), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (inline-ignored) — stderr: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "ignored: 1 drift") ||
		!strings.Contains(out.String(), "managed by PIM") {
		t.Errorf("text missing inline-ignored section:\n%s", out.String())
	}
}

// -format json with -source carries the .tf file+line for each drifted resource
// (testdata/tfsrc/main.tf declares azurerm_signalr_service.legacy at line 6).
func TestRunJSONSourceLocations(t *testing.T) {
	plan := `{"format_version":"1.2","terraform_version":"1.9.6","resource_drift":[` +
		`{"address":"azurerm_signalr_service.legacy","type":"azurerm_signalr_service",` +
		`"change":{"actions":["update"],"before":{"a":1},"after":{"a":2}}}],"resource_changes":[]}`
	var out, errb bytes.Buffer
	code := run([]string{"-format", "json", "-source", "testdata/tfsrc"}, strings.NewReader(plan), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2 — stderr: %s", code, errb.String())
	}
	var doc struct {
		Drift []struct {
			Address string `json:"address"`
			File    string `json:"file"`
			Line    int    `json:"line"`
		} `json:"drift"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out.String())
	}
	if len(doc.Drift) != 1 || doc.Drift[0].File != "main.tf" || doc.Drift[0].Line != 6 {
		t.Errorf("drift[0] file/line = %q/%d, want main.tf/6 (%+v)", doc.Drift[0].File, doc.Drift[0].Line, doc.Drift)
	}
}

// --- Teams --------------------------------------------------------------

const driftPlanJSON = `{"format_version":"1.2","terraform_version":"1.9.6","resource_drift":[` +
	`{"address":"azurerm_storage_account.data","type":"azurerm_storage_account",` +
	`"change":{"actions":["update"],"before":{"min_tls_version":"TLS1_2"},"after":{"min_tls_version":"TLS1_0"}}}],` +
	`"resource_changes":[]}`

const cleanPlanJSON = `{"format_version":"1.2","terraform_version":"1.9.6","resource_drift":[],"resource_changes":[]}`

func TestRunFormatTeamsWritesCard(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-format", "teams", "-teams-context", "nightly · main",
		"-run-url", "https://example.invalid/run/1", "-exit-code=false"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	var doc struct {
		Type        string `json:"type"`
		Attachments []struct {
			ContentType string `json:"contentType"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out.String())
	}
	if doc.Type != "message" || len(doc.Attachments) != 1 {
		t.Errorf("not a Workflows message payload: %s", out.String())
	}
	for _, want := range []string{"nightly", "https://example.invalid/run/1", "AdaptiveCard"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("card missing %q:\n%s", want, out.String())
		}
	}
}

// -teams-webhook posts the card, and by default only when something was found.
func TestRunTeamsWebhookPostsOnFindings(t *testing.T) {
	var posts int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := run([]string{"-teams-webhook", srv.URL, "-exit-code=false"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1", posts)
	}
	if !strings.Contains(body, "changed outside Terraform") {
		t.Errorf("posted card looks wrong:\n%s", body)
	}
	// The default format still writes its own output.
	if !strings.Contains(out.String(), "azurerm_storage_account.data") {
		t.Errorf("text report missing:\n%s", out.String())
	}
}

func TestRunTeamsWebhookQuietWhenClean(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"-teams-webhook", srv.URL}, strings.NewReader(cleanPlanJSON), &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if posts != 0 {
		t.Errorf("posts = %d, want 0 (nothing to report)", posts)
	}

	// ...unless asked to post every run.
	out.Reset()
	errb.Reset()
	if code := run([]string{"-teams-webhook", srv.URL, "-teams-notify", "always"},
		strings.NewReader(cleanPlanJSON), &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1 with -teams-notify always", posts)
	}
}

// The webhook can come from the environment so it never lands in a command line.
func TestRunTeamsWebhookFromEnv(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("TF_SNAG_TEAMS_WEBHOOK", srv.URL)

	var out, errb bytes.Buffer
	if code := run([]string{"-exit-code=false"}, strings.NewReader(driftPlanJSON), &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1 from $TF_SNAG_TEAMS_WEBHOOK", posts)
	}
}

// A notification that silently fails to arrive is worse than a loud failure.
func TestRunTeamsPostFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such flow", http.StatusNotFound)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := run([]string{"-teams-webhook", srv.URL, "-exit-code=false"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "404") {
		t.Errorf("stderr should explain the failure: %s", errb.String())
	}
}

func TestRunTeamsNotifyRejectsGarbage(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-teams-notify", "sometimes"}, strings.NewReader(cleanPlanJSON), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "findings, new or always") {
		t.Errorf("stderr = %q", errb.String())
	}
}

// -teams-notify new posts only for findings absent from the baseline.
func TestRunTeamsNotifyNew(t *testing.T) {
	var posts int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	// A baseline that already knows about this exact drift: nothing is new.
	prevOut := &bytes.Buffer{}
	if code := run([]string{"-format", "sarif", "-exit-code=false"},
		strings.NewReader(driftPlanJSON), prevOut, &bytes.Buffer{}); code != 0 {
		t.Fatal("could not build a baseline")
	}
	prev := filepath.Join(t.TempDir(), "prev.sarif")
	if err := os.WriteFile(prev, prevOut.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := run([]string{"-teams-webhook", srv.URL, "-teams-notify", "new",
		"-baseline", prev, "-exit-code=false"}, strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if posts != 0 {
		t.Errorf("posts = %d, want 0 — the drift was already in the baseline", posts)
	}

	// Drift the baseline has never seen: post, and mark it new on the card.
	newPlan := strings.ReplaceAll(driftPlanJSON, "azurerm_storage_account.data", "azurerm_storage_account.fresh")
	out.Reset()
	errb.Reset()
	code = run([]string{"-teams-webhook", srv.URL, "-teams-notify", "new",
		"-baseline", prev, "-exit-code=false"}, strings.NewReader(newPlan), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1 for a finding absent from the baseline", posts)
	}
	// Marked with a highlighted "New" chip, the card's version of the run tab's
	// new pill. The padding is non-breaking, so match on the word alone.
	if !strings.Contains(body, `New `) || !strings.Contains(body, `"highlight": true`) {
		t.Errorf("card should mark the finding with a New chip:\n%s", body)
	}
	if !strings.Contains(body, `"title": "New"`) {
		t.Errorf("card should carry a New fact:\n%s", body)
	}
}

// The pipeline supplies the webhook through the environment and leaves -format
// at its default, so -baseline has to be accepted on that combination too — the
// guard must consult the resolved webhook, not just the flag.
func TestRunTeamsBaselineWithEnvWebhookAndDefaultFormat(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	t.Setenv("TF_SNAG_TEAMS_WEBHOOK", srv.URL)

	prevOut := &bytes.Buffer{}
	if code := run([]string{"-format", "sarif", "-exit-code=false"},
		strings.NewReader(cleanPlanJSON), prevOut, &bytes.Buffer{}); code != 0 {
		t.Fatal("could not build a baseline")
	}
	prev := filepath.Join(t.TempDir(), "prev.sarif")
	if err := os.WriteFile(prev, prevOut.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := run([]string{"-baseline", prev, "-teams-notify", "new", "-exit-code=false"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if strings.Contains(errb.String(), "-baseline applies to") {
		t.Errorf("-baseline rejected despite a webhook in the environment: %s", errb.String())
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1 (the drift is new against a clean baseline)", posts)
	}
}

// Without a baseline "new" cannot mean anything; falling silent would be the
// worst outcome, so it degrades to "findings" and says so.
func TestRunTeamsNotifyNewWithoutBaselineFallsBack(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := run([]string{"-teams-webhook", srv.URL, "-teams-notify", "new", "-exit-code=false"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1 (fall back rather than go silent)", posts)
	}
	if !strings.Contains(errb.String(), "needs -baseline") {
		t.Errorf("stderr should explain the fallback: %q", errb.String())
	}
}

// --- ADO work items -----------------------------------------------------

// adoStub is a minimal Azure DevOps stand-in for the main-level wiring tests.
func adoStub(t *testing.T, created *int) *httptest.Server {
	t.Helper()
	next := 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "wiql"):
			io.WriteString(w, `{"workItems":[]}`)
		case strings.Contains(r.URL.RawQuery, "validateOnly=true"):
			io.WriteString(w, `{"id":0,"fields":{}}`)
		case r.Method == http.MethodPost:
			*created++
			next++
			w.Write([]byte(`{"id":` + strconv.Itoa(next) + `,"fields":{"System.State":"New","System.Title":"t"}}`))
		default:
			io.WriteString(w, `{"id":0,"fields":{}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// With no baseline, -ado-raise new cannot mean anything; it degrades to raising
// for any un-suppressed finding rather than silently never raising.
func TestRunADORaisesAndStampsTheReport(t *testing.T) {
	var created int
	srv := adoStub(t, &created)

	var out, errb bytes.Buffer
	code := run([]string{"-format", "json", "-exit-code=false",
		"-ado-url", srv.URL + "/proj", "-ado-token", "pat"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if created != 1 {
		t.Errorf("created %d work items, want 1", created)
	}
	if !strings.Contains(errb.String(), "needs -baseline") {
		t.Errorf("stderr should explain the -ado-raise new fallback: %q", errb.String())
	}

	// The reference is stamped onto the report so every format can show it.
	var doc struct {
		Drift []struct {
			WorkItem    int    `json:"work_item"`
			WorkItemURL string `json:"work_item_url"`
		} `json:"drift"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, out.String())
	}
	if len(doc.Drift) != 1 || doc.Drift[0].WorkItem == 0 {
		t.Fatalf("finding not stamped with a work item: %s", out.String())
	}
	if !strings.Contains(doc.Drift[0].WorkItemURL, "_workitems/edit/") {
		t.Errorf("work item URL = %q, want a browser link", doc.Drift[0].WorkItemURL)
	}
}

// The pre-check is the point of issue #14's "clearly alert when permissions are
// missing": a 403 must fail before any finding is processed.
func TestRunADOReportsMissingPermissionUpFront(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		http.Error(w, `{"message":"Access denied."}`, http.StatusForbidden)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := run([]string{"-exit-code=false", "-ado-url", srv.URL + "/proj", "-ado-token", "pat"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Work Items (Read & Write)") {
		t.Errorf("stderr should name the missing permission: %q", errb.String())
	}
	// One call: the validateOnly pre-flight, which failed. Nothing further.
	if posts > 2 {
		t.Errorf("kept going after the permission check failed (%d calls)", posts)
	}
}

func TestRunADOMissingTokenIsAnError(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-exit-code=false", "-ado-url", "https://dev.azure.com/o/p"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "TF_SNAG_ADO_TOKEN") {
		t.Errorf("stderr should say where the token comes from: %q", errb.String())
	}
}

func TestRunADODryRunCreatesNothing(t *testing.T) {
	var created int
	srv := adoStub(t, &created)

	var out, errb bytes.Buffer
	code := run([]string{"-exit-code=false", "-ado-dry-run",
		"-ado-url", srv.URL + "/proj", "-ado-token", "pat"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if created != 0 {
		t.Errorf("dry run created %d work items for real", created)
	}
	// Narration goes to stderr: stdout is the report, and -format json/sarif is
	// redirected straight to a file.
	if !strings.Contains(errb.String(), "would create") {
		t.Errorf("dry run should report what it would do:\n%s", errb.String())
	}
	if strings.Contains(out.String(), "would create") {
		t.Errorf("work item narration leaked into stdout:\n%s", out.String())
	}
}

func TestRunADORejectsBadRaiseValue(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-exit-code=false", "-ado-url", "https://dev.azure.com/o/p",
		"-ado-token", "pat", "-ado-raise", "sometimes"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "new or findings") {
		t.Errorf("stderr = %q", errb.String())
	}
}

// Without -ado-url nothing touches Azure DevOps at all.
func TestRunWithoutADOURLIsUnchanged(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"-format", "json", "-exit-code=false"},
		strings.NewReader(driftPlanJSON), &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 — stderr: %s", code, errb.String())
	}
	if strings.Contains(out.String(), "work_item") {
		t.Errorf("work item fields leaked into a run with no -ado-url:\n%s", out.String())
	}
}
