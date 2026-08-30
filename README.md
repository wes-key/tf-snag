# tf-snag

Reads `terraform show -json` output and tells you which resources changed
**outside Terraform** — the storage account someone loosened in the portal, the
tag a script stripped, the NSG rule added by hand.

It is not a wrapper around `terraform plan`. It reads the plan Terraform already
produced and turns it into a short, reviewable summary, with an exit code CI can
gate on.

```
$ terraform plan -out tfplan
$ terraform show -json tfplan | tf-snag

tf-snag: 1 resource(s) changed outside Terraform

  ~ azurerm_storage_account.data
      allow_nested_items_to_be_public: false => true
      min_tls_version: "TLS1_2" => "TLS1_0"
      tags.temp: null => "true"

pending changes from configuration: 1 to add, 0 to change, 1 to destroy
  + azurerm_resource_group.new
  - azurerm_subnet.legacy  (module.network)
```

## Why the two sections

Terraform's JSON plan has two arrays:

- **`resource_drift`** — differences Terraform found while refreshing state
  against the real world. This is drift: nobody wrote this in `.tf`.
- **`resource_changes`** — what applying would do, driven by your configuration.

tf-snag leads with drift and lists pending changes underneath for context. An
in-place drift whose only differences are empty-form round-trips (`tags: null ->
{}` and the like, which some providers re-emit on every refresh) is dropped — a
clean `terraform plan` confirms it is not real drift.

## Install / build

Requires Go 1.23+.

```
go build -o tf-snag .
# or
go install github.com/wes-key/tf-snag@latest
```

## Usage

```
terraform show -json PLANFILE | tf-snag [flags]
tf-snag -plan plan.json [flags]

  -plan string        path to `terraform show -json` output (default: stdin)
  -plan-log string    path to `terraform plan -json` NDJSON log (required by
                      -check deprecations)
  -plan-log-dir dir   directory `terraform plan` ran in, relative to the repo
                      root; prepended to deprecation file locations so their
                      links resolve
  -check list         analyses to run: drift, deprecations, or a comma list
                      (also: all) (default "drift")
  -format string      text | json | markdown | junit | sarif (default "text")
  -color string       colorize text output: auto | always | never (default "auto")
  -source dir         Terraform source dir; sarif locations link to the .tf
                      declaring each resource (best effort, first match wins)
  -exit-code          exit 2 when drift or a deprecation is detected (default true)
  -version            print version and exit
```

`-check deprecations` reads the newline-delimited JSON that `terraform plan
-json` writes (not the `terraform show -json` plan representation, which carries
no diagnostics) and reports the deprecation warnings in it. It supports `-format
sarif` and `-format text` only. `-check all` runs both analyses; drift then reads
`-plan` and deprecations read `-plan-log` (only one may come from stdin).

`-color=auto` colours the `text` output when stdout is a terminal (and
`NO_COLOR` is unset); Azure DevOps logs render ANSI, so the pipeline passes
`-color=always`.

`tf-snag -version` prints e.g. `tf-snag 0.1.7 (a1b2c3d4e5f6) linux/amd64
go1.23.4`. The version is stamped by `.github/workflows/ci.yml`
(`-ldflags "-X main.version=0.1.<run>"`); a plain `go build` falls back to the
module version plus the embedded git revision.

Formats: `text` for humans/console, `json` for the run-tab extension (carries a
`schema` version), `markdown` for `##vso[task.uploadsummary]`, `sarif` for the
"SARIF SAST Scans Tab" extension, `junit` for `PublishTestResults@2`. Resources
created/destroyed outside Terraform are summarised in one line rather than
diffed attribute-by-attribute against null. Pending changes appear in `text`,
`json` and `markdown` only. Deprecations (`-check deprecations`) appear in
`sarif` and `text` only — `json`/`markdown`/`junit` do not carry them yet.

The `sarif` output is one flat result per drifted resource:

| Scans column | From | e.g. |
|---|---|---|
| severity icon | `level` | ⛔ delete/create · ⚠ update |
| Result | `message` | `Deleted — name=kv01` · `Updated — tags.Owner: null → "Wes"` |
| Path (link) | `physicalLocation` (needs `-source`) | `terraform/modules/kv/main.tf` |
| Baseline | `baselineState` | `absent` · `updated` · `new` |

Drift results share one rule, `resource-drift`; the kind (`Deleted` / `Updated`
/ `Created`) leads the message. No glyph on it — the severity icon in column 0
(from `level`) already carries the visual weight. One changed attribute stays on
the verb line; two or more break onto one line each (the Scans tab renders
`message.text` with `white-space: pre-line`, so the newlines show without
`message.markdown`). The tab renders the message as unstyled text with no
per-cell styling hooks, and column font sizes are the extension's own, not
something the SARIF can set. No `fullDescription`/`helpUri` on the rule and no
`message.markdown`, so rows have no expander. Without a `-source` match the Path
cell is the resource address as plain text.

With `-check deprecations` the log carries a second rule, `deprecation`, one
result per distinct notice (keyed on summary + detail). The Scans tab
de-duplicates nothing, so tf-snag collapses here: every source location that
trips the same deprecation becomes one row — summary on message line 1, detail
on line 2, then one `address (file:line)` line per location (always, even for a
single site). The first location is the result's `location`, the rest are
`relatedLocations`. Locations come from the diagnostic's `range`, which
`terraform plan -json` reports relative to the dir it ran in — pass
`-plan-log-dir` (e.g. `terraform` when the plan used `-chdir=terraform`) so the
paths are repo-root-relative and the Scans-tab links resolve. The two rules
render as two collapsible groups; the Scans tab orders groups by result count
(or alphabetically if the viewer re-sorts), so their vertical order is not
something the file controls. The deprecation filter is a substring match on
"deprecat" in the summary/detail — provider notices worded differently are
missed for now.

Exit codes: `0` clean, `2` drift or a deprecation detected, `2` on any error.

Try it against the bundled fixture:

```
go run . -plan testdata/plan-drift.json
```

## Azure DevOps

The scheduled job in `../tf-drift-test-resources/pipelines/tf-snag.yml` plans
against real infrastructure and emits all reports. Capture the `-json` plan log
alongside the plan file to feed the deprecation check:

```yaml
- script: |
    terraform -chdir=terraform plan -out tfplan -json | tee plan.jsonl
    terraform -chdir=terraform show -json tfplan > plan.json
    ./tf-snag -plan plan.json -format json     -exit-code=false > tf-snag.json
    ./tf-snag -plan plan.json -format markdown  -exit-code=false > tf-snag.md
    # -plan-log-dir terraform: plan ran with -chdir=terraform, so its diagnostic
    # paths are relative to terraform/ — prepend it for working Scans links.
    ./tf-snag -check all -plan plan.json -plan-log plan.jsonl -plan-log-dir terraform \
      -format sarif -source "$(Build.SourcesDirectory)" -exit-code=false > tf-snag.sarif
    echo "##vso[task.addattachment type=tf-snag.report;name=tf-snag;]$PWD/tf-snag.json"
    echo "##vso[task.uploadsummary]$PWD/tf-snag.md"
    ./tf-snag -check all -plan plan.json -plan-log plan.jsonl -plan-log-dir terraform  # console + exit 2
  displayName: Detect drift
- task: PublishBuildArtifacts@1
  condition: succeededOrFailed()
  inputs:
    PathtoPublish: tf-snag.sarif
    ArtifactName: CodeAnalysisLogs
```

Where each lands on the run page:

| Surface | Mechanism | Needs |
|---|---|---|
| **Scans** tab | `sarif` + `CodeAnalysisLogs` artifact | the "SARIF SAST Scans Tab" extension installed in the org |
| **Summary** tab section | `markdown` + `task.uploadsummary` | nothing (built in) |
| dedicated **Drift** tab | `json` attachment + [`extension/`](extension/) | the extension published & installed — see `extension/README.md` |

The Scans tab carries both the `resource-drift` and (with `-check all`)
`deprecation` groups from the one `tf-snag.sarif`.

`junit` is still emitted by the tool for anyone who prefers the built-in Tests
tab (`PublishTestResults@2`); the scheduled pipeline uses the Scans tab instead.

**Next.** PR thread comments; a scheduled per-stack dashboard.

## Status

Early. Parser + text/JSON/markdown/JUnit/SARIF report + exit codes, covered by
tests. Deprecation check (`-check deprecations`, from the `terraform plan -json`
log) surfaces in SARIF and text. Scans-tab and Summary integration live;
Drift-tab extension in place, not yet published. No releases yet.

CI is GitHub Actions (`.github/workflows/ci.yml`): vet + test on every PR and on
`main`. Releases are tag-driven — push `vX.Y.Z` (or `vX.Y.Z-dev.N` / `-rc.N`,
which publish as pre-releases) and CI builds the linux + windows binaries and
attaches them to a GitHub Release. The `tf-drift-test-resources` drift pipeline
pulls the linux binary from the latest non-pre-release.
