# tf-snag

[![CI](https://github.com/wes-key/tf-snag/actions/workflows/ci.yml/badge.svg)](https://github.com/wes-key/tf-snag/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.23-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

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
  -format string      text | json | markdown | junit | sarif | teams
                      (default "text")
  -color string       colorize text output: auto | always | never (default "auto")
  -source dir         Terraform source dir; sarif/json findings carry the .tf
                      file+line declaring each resource (best effort, first
                      match wins)
  -baseline file      previous run's tf-snag.sarif; stamps each finding new /
                      updated for -format sarif, json and the Teams card (sarif
                      also re-emits what has gone absent)
  -ignore file        tf-snag ignore YAML (default: .tf-snag-ignore.yml in cwd
                      or -source); matched findings are suppressed, not gated
  -teams-webhook url  Teams Power Automate Workflows URL to POST the card to
                      (default: $TF_SNAG_TEAMS_WEBHOOK). Treat as a secret
  -teams-notify when  findings (anything un-suppressed), new (only findings
                      absent from -baseline) or always (default "findings")
  -teams-context line subtle line under the card headline, e.g. pipeline,
                      branch and run number
  -run-url url        this CI run's URL: recorded against findings first seen
                      in this run so later ones can link back, and used for the
                      Teams card's "View run" button
  -ado-url url        Azure DevOps project URL to raise work items in
                      (https://dev.azure.com/org/project)
  -ado-token tok      PAT or System.AccessToken (default: $TF_SNAG_ADO_TOKEN)
  -ado-type type      work item type to raise (default "Task")
  -ado-area path      area path for new work items
  -ado-raise when     new (absent from -baseline) or findings (default "new")
  -ado-close          close work items whose finding is no longer reported
  -ado-closed-state s state a resolved finding's item moves to; default: the
                      work item type's own completed state
  -ado-dry-run        report what would be raised or closed, change nothing
  -exit-code          exit 2 when an un-suppressed drift or deprecation is
                      detected (default true)
  -version            print version and exit
```

`-check deprecations` reads the newline-delimited JSON that `terraform plan
-json` writes (not the `terraform show -json` plan representation, which carries
no diagnostics) and reports the deprecation warnings in it. It supports `-format
sarif`, `json`, `text` and `markdown` (not `junit`, which is drift-only).
`-check all` runs both analyses; drift then reads `-plan` and deprecations read
`-plan-log` (only one may come from stdin).

`-color=auto` colours the `text` output when stdout is a terminal (and
`NO_COLOR` is unset); Azure DevOps logs render ANSI, so the pipeline passes
`-color=always`.

`tf-snag -version` prints e.g. `tf-snag 0.1.7 (a1b2c3d4e5f6) linux/amd64
go1.23.4`. The version is stamped by `.github/workflows/ci.yml`
(`-ldflags "-X main.version=0.1.<run>"`); a plain `go build` falls back to the
module version plus the embedded git revision.

`-version` and `-help` also draw the tf-snag wordmark, in the same purple as the
extension's artwork. It goes to **stderr**, so `tf-snag -version` remains exactly
one parseable line on stdout, and it appears nowhere else — every other run
writes a report to stdout that a pipeline redirects straight to a file. It
follows `-color` / `NO_COLOR`, auto-detecting against stderr rather than stdout.

Formats: `text` for humans/console, `json` for the tf-snag run-tab extension
(carries a `schema` version — currently **2**: drift, deprecations, ignore-rule
suppression, `-baseline` provenance, and per-drift `file`/`line` with `-source`),
`markdown` for
`##vso[task.uploadsummary]`, `sarif` for the "SARIF SAST Scans Tab" extension,
`junit` for `PublishTestResults@2`, `teams` for a Microsoft Teams Adaptive Card
(see below). Resources created/destroyed outside
Terraform are summarised in one line rather than diffed attribute-by-attribute
against null. Pending changes appear in `text`, `json` and `markdown` only.
Deprecations (`-check deprecations`) appear in
`sarif`, `json`, `text` and `markdown`. `junit` is drift-only.

The `sarif` output is one flat result per drifted resource:

| Scans column | From | e.g. |
|---|---|---|
| severity icon | `level` | ⛔ delete/create · ⚠ update |
| Result | `message` | `Deleted — name=kv01` · `Updated — tags.Owner: null → "Wes"` |
| Path (link) | `physicalLocation` (needs `-source`) | `terraform/modules/kv/main.tf` |
| Baseline | `baselineState` | `New` unless `-baseline` is given — see below |

Every result carries a deterministic `guid` (RFC 4122 v5 of the finding's kind +
identity) and a `partialFingerprints` entry (`driftAddress/v1`,
`deprecation/v1`). Pass **`-baseline <previous run's tf-snag.sarif>`** and
tf-snag diffs against it — matching on `guid` — and stamps each result:

- `new` — not in the previous run
- `updated` — was also in the previous run (whether or not its message changed)
- `absent` — in the previous run, gone now (drift remediated, deprecation fixed);
  re-emitted as its own result

`unchanged` is deliberately never emitted: the Azure DevOps Scans tab's default
Baseline filter is `new` / `updated` / `absent`, so an `unchanged` result would
just vanish while the drift is still live. `updated` covers everything carried
over.

Each baselined result also gets `provenance.firstDetectionTimeUtc` — forwarded
from the matched prior result, or "now" for a new one — and, since that tab has
no Age column, the first line of the message ends with a
`(first seen <date>, <n> days ago)` note, e.g. `Argument is deprecated (first
seen 2026-08-10, 20 days ago)`. The date is only accurate from the second
baselined run on (the first has no prior timestamp to carry).

Without `-baseline`, `baselineState` and `provenance` are left unset and the
Scans tab shows every row as `New`.

`-format json -baseline` does the same guid diff on the report model: every
drift and deprecation gets `baseline_state` (`new` / `updated`) and `first_seen`
(RFC 3339), which the tf-snag tab renders as a "new" pill or a "first seen …"
note. `absent` findings are SARIF-only (JSON lists live findings).

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

Exit codes: `0` clean, `2` an un-suppressed drift or deprecation detected, `2`
on any error.

Try it against the bundled fixture:

```
go run . -plan testdata/plan-drift.json
```

## Ignoring findings

A finding you've accepted stays in the report — SARIF `result.suppressions` (the
Scans tab hides suppressed results by default; switch the **Suppression** filter
to see them), an **ignored** section in `text`, an `### Ignored` table in
`markdown`, a `<skipped>` case in `junit` — but no longer trips `-exit-code`.

**Ignore file** — `-ignore <file>`, or an auto-discovered `.tf-snag-ignore.yml`
(`.yaml`) in the working directory or the `-source` root:

```yaml
drift:
  - address: "module.storage_account[0].azurerm_storage_account.sa"
    reason: "temp tag from the deploy script — JIRA-123"
  - address: "azurerm_role_assignment.*"     # * matches any run of characters
    reason: "managed by PIM outside Terraform"
deprecations:
  - match: "live_trace_enabled"              # case-insensitive substring of
    reason: "provider 4.0 upgrade tracked separately"   # summary + detail
```

A drift `address` is matched against both the full address and its trailing
`type.name` (so `azurerm_role_assignment.*` catches
`module.x.azurerm_role_assignment.foo`).

**Inline comments** — active when `-source` points at the `.tf` tree. Put the
directive inside a resource block or on the line(s) directly above a `resource`
declaration:

```hcl
# tf-snag:ignore-drift  reason: managed by PIM
resource "azurerm_role_assignment" "admin" { ... }

resource "azurerm_signalr_service" "legacy" {
  # tf-snag:ignore-deprecation reason: 4.0 upgrade in backlog
  live_trace_enabled = true
}
```

`# tf-snag:ignore` (both), `# tf-snag:ignore-drift`,
`# tf-snag:ignore-deprecation`; the `reason:` (or `reason=`) trailer is
optional. Inline rules are emitted as SARIF suppression `kind: "inSource"`, file
rules as `"external"`.

## Microsoft Teams

tf-snag can post the run's findings to a Teams channel as an
[Adaptive Card](https://adaptivecards.io):

```
tf-snag -check all -plan plan.json -plan-log plan.jsonl \
  -teams-context "nightly-drift · main · run 20260901.3" \
  -run-url "$RUN_URL"
```

The card leads with a tinted banner carrying the verdict — red for drift, amber
for deprecations alone, green for a clean run — then a fact list of the counts.
Drift and deprecations each get their own **collapsed** group below that: a
tinted header row showing the kind and its count, which expands in place when
clicked (`Action.ToggleVisibility`). A channel post should be glanceable, so the
detail stays folded away until someone wants it. Inside a group each finding
gets its plan sign in the matching colour (green create, red delete, amber
update) with the attribute changes beneath.

Ignored findings are never listed — they are summarised as an "Ignored" count so
the card stays about what needs attention.

**When it posts** — `-teams-notify`:

| | |
|---|---|
| `findings` (default) | whenever anything un-suppressed was found |
| `new` | only when a finding was **absent from `-baseline`**, or has just come out from under an ignore rule |

Under `new`, a finding whose ignore rule has just been removed counts too — it
was invisible here yesterday and is actionable today.

| `always` | every run, clean or not — use it to prove the webhook works |

`new` is the one that matters on a schedule. Drift nobody has fixed is still
drift, but re-posting the same card every morning is how a channel learns to
ignore an alert; `new` stays quiet until something actually appears. It needs
`-baseline` to tell a fresh finding from a long-standing one — without one it
falls back to `findings` and says so on stderr, because a notifier that quietly
never fires is the worst failure mode available to it.

Given `-baseline`, the card also marks each finding: new ones sort to the top and
carry a **New** badge; everything else shows how long it has been there ("first
seen 2026-08-20, 13 days ago"). Since new findings lead, they are the ones that
survive the per-section cap.

Pass **`-run-url`** and that age becomes a link to the run that first surfaced
the finding. The URL is recorded against anything new in this run and travels
forward with it, so on the tenth morning a long-standing drift still points at
the run that caught it. New findings are not linked — the card's own "View run"
button is already that run.

The "New" fact counts them **relative to the previous check, not the previous
message** — under `-teams-notify new` those are not the same thing, since a
quiet week means no card at all.

**Setting up the webhook.** Microsoft has retired the Office 365 connectors
("Incoming Webhook"), so tf-snag targets their replacement, a **Power Automate
Workflows** trigger. In Teams: channel **⋯ → Workflows → "Post to a channel when
a webhook request is received"**, pick the team and channel, and copy the HTTP
POST URL it generates. The legacy MessageCard shape is deliberately not
supported.

**The URL is a credential** — anyone holding it can post to the channel. Prefer
the environment over a command line, which ends up in process listings and CI
logs:

```yaml
- script: tf-snag -check all -plan plan.json -plan-log plan.jsonl
  env:
    TF_SNAG_TEAMS_WEBHOOK: $(TEAMS_WEBHOOK)   # secret pipeline variable
```

`-teams-webhook` takes precedence over `$TF_SNAG_TEAMS_WEBHOOK`. The URL is
stripped from any error tf-snag prints, so a failure is safe to log.

A failed POST exits 2 with the status and response body on stderr — a
notification you believe is going out but is not is worse than a loud failure.
Retries are automatic on 429/5xx and connection errors (three attempts, 1s then
2s, honouring `Retry-After`).

`-format teams` writes the same card to stdout instead of posting it, which is
how you inspect or diff the payload; the two can be combined to log exactly what
was sent. Cards are capped at five findings per section (and two attribute
changes per resource) with an "…and N more" line, so a large drift set cannot
blow past the webhook's payload limit.

## Work items

tf-snag can raise an Azure DevOps work item per finding and close it again when
the finding goes away:

```
tf-snag -check all -plan plan.json -plan-log plan.jsonl -baseline prev.sarif \
  -ado-url https://dev.azure.com/wes-key/tf-snag -ado-type Task -run-url "$RUN_URL"
```

**Identity.** Every item tf-snag creates is tagged `tf-snag` plus
`tf-snag-id-<finding id>`, where the id is the same stable guid the SARIF result
carries. Each run queries those tags back out, so **Azure DevOps** — not a build
artifact — is the record of what has already been raised. That matters on a
schedule: artifact retention expires and pipelines get rebuilt, and neither
should turn into a second work item for drift already being tracked. The id is
derived from the resource address alone, so a finding keeps its item even as the
drifted values change.

**What gets raised.** `-ado-raise new` (the default) raises only for findings
absent from `-baseline`, so adopting this against a drifty estate does not open
fifty items on day one. `-ado-raise findings` raises for anything un-suppressed.
Either way an ignored finding never gets one — somebody has already decided not
to act on it. A finding that already has an item is always linked back to it
regardless of the mode, so `work_item` / `work_item_url` are on the report for
every downstream format to show.

**Closing.** `-ado-close` moves items whose finding is no longer reported to
`-ado-closed-state` and records why in the item's history. That state defaults to
whichever one the work item type files under the **Completed** category, so Agile
projects close into `Closed` and Scrum and Basic into `Done` with nothing to
configure. A value you supply is checked against the type's real states before
anything is closed — the raw API failure only says the value is unsupported,
without saying what is supported, and it does not surface until the first run
where a finding disappears.
It is opt-in on purpose: creating items is additive, but closing them mutates
work someone may have triaged, re-assigned or linked.

**Ignoring a finding does not close its item.** Closing would move it into the
template's *Completed* state — telling the board the work was delivered when
nobody did any — and by then the item may have been triaged, assigned or pulled
into a sprint, which is not tf-snag's to unwind. Instead it comments once, saying
the drift is still there and tf-snag has stopped reporting it, and tags the item
`tf-snag-ignored` so the note is not repeated on every later run. Whether to
close it is left to a person.

**Removing the rule resumes on the same item.** It was never closed, so there is
nothing to reopen and no second item: the note is retracted, the tag removed, and
the finding is badged **No longer ignored** rather than *New* — it keeps its real
first-detected date, because the tracking is what changed, not the drift.
`-teams-notify new` and `-ado-raise new` both fire for it, and both surfaces sort
it to the top.

**One finding, one open work item.** That is what the id tag is for: while an
item for a finding is open, every run links to it and none raises a second,
whoever moved it wherever on the board.

If a finding turns out to have **more than one** open item — duplicates raised
before this was tightened — tf-snag tracks the oldest, names the others in the
run output, and touches none of them. They are open items on somebody's board;
which to keep is a decision, not a cleanup.

**A closed item is history, not a slot to reuse.** Drift that was fixed, closed,
and later comes back is a *new* occurrence, so it gets its own item rather than
resurrecting somebody's completed work — and that item opens with a comment
naming the closed one it follows, so the recurrence is visible from the item
itself. "Closed" here means any state the process template counts as **Completed
or Removed**, not just the one `-ado-closed-state` names, so an item someone
finished their own way is still finished.

**Permissions.** The token needs **Work Items (Read & Write)** — read as well,
since the dedup query is a read. Before processing anything, tf-snag issues a
`validateOnly` create: a bad token, project or work item type fails immediately
and says which, rather than halfway through raising items. Azure DevOps answers
an unauthenticated API call with `203` and a sign-in page rather than `401`, so
that case is reported as "not authenticated" instead of a bare status.

`-ado-token` also accepts a pipeline's `System.AccessToken` — the auth scheme is
detected from the token's shape, so a PAT and an OAuth bearer both work without
a flag. Prefer `$TF_SNAG_ADO_TOKEN` so it never reaches a command line.

**`-ado-dry-run`** reports what would be raised and closed without touching
anything. Worth doing on first adoption. All work-item output goes to stderr, so
it never contaminates `-format json`/`sarif` on stdout.

## Exceptions register (wiki)

`-wiki-page` publishes a register of every ignore rule to an Azure DevOps wiki
page — what is waived, why, where it is defined, and what it is currently
suppressing:

```
tf-snag -check all -plan plan.json -plan-log plan.jsonl -baseline prev.sarif   -ado-url https://dev.azure.com/wes-key/tf-snag -wiki-page /tf-snag/Exceptions
```

Rules are split into two tables. **In effect** lists the ones suppressing a
finding in this run, with how long that finding has been there (`-baseline`
supplies the age; without one the column reads `—`). **Suppressing nothing**
lists the rest — a waiver whose drift has since been fixed is a rule nobody
needs and nobody will think to remove, and it is the whole reason the page is
worth visiting.

Inline rules are marked as such: an inline comment is reviewed with the
Terraform it sits in, a file rule is reviewed on its own, and the register says
which is which. A rule with no `reason` is flagged rather than left blank.

**The page is generated.** Editing it is pointless — the next run replaces it.
Edit the rules.

**It only writes when something changed.** The page is read for its ETag
regardless, so comparing is free, and a daily check that rewrites an identical
page buries the revisions that mean something. The generated-at footer is
excluded from that comparison, or every run would differ.

**Which wiki.** Discovered, not assumed: tf-snag lists the project's wikis and
picks the **project wiki**, or the only one when a project has a single code
wiki. `-wiki` names one explicitly (by name or id) when there are several. A
project with no wiki at all is reported as such rather than as a missing page —
and note a project wiki's *name* is not `<project>.wiki`, that is only its
backing repository, so nothing here constructs an identifier.

**Flags.** `-wiki` names the wiki;
`-wiki-dry-run` prints the page and reports what would happen without writing.
`-wiki-page` needs `-ado-url` to say which project. All output goes to stderr,
so it never contaminates `-format json`/`sarif` on stdout.

**How it is written.** As a **commit to the wiki's Git repository**, not through
the Wiki API. A wiki is a repository of markdown files and both routes reach the
same pages, but they need different scopes: the Wiki API wants `vso.wiki_write`,
which a pipeline's `System.AccessToken` does not appear to carry, while the Git
API wants `vso.code_write`, which it plainly does — it clones the repository
every run. Going through Git is what lets the build service publish without a PAT.

**Permissions.** The build service needs **Contribute** on the wiki's repository:
`<Project>.wiki` for a project wiki, or the repository it was published from for
a code wiki. Grant it from the wiki's own **⋯ → Wiki security**, or under
*Project settings → Repos → Repositories* when the wiki repo is listed there.

**Naming it.** `-wiki` is the wiki's **repository**, and takes a name or an id.
The name does not always resolve through the Git API; the repository id always
does, and is worth pinning in a pipeline. tf-snag logs which repository and
branch it is writing to.

**The pipeline must reference the wiki repository.** This is the one that costs
people a day. Azure DevOps has a setting — *Limit job authorization scope to
referenced Azure DevOps repositories* — which, when on, scopes a job's token to
only the repositories the pipeline names. The wiki is a *separate* repository, so
unless it is referenced the token never reaches it, **whatever permissions the
build service has been granted**: the scope is applied before any ACL is
consulted. Two ways out.

*Declare the repository in the calling pipeline* — least privilege, and visible
to whoever reads the YAML next:

```yaml
resources:
  repositories:
    - repository: wiki
      type: git
      name: <Project>.wiki      # the wiki's repository

jobs:
  - job: drift
    uses:
      repositories: [wiki]      # brings it into this job's token scope
    steps: ...
```

Declared, not checked out — `uses` grants the access, and tf-snag writes through
the Git API rather than from a working copy.

*Or turn the setting off* — **Project settings → Pipelines → Settings → Limit job
authorization scope to referenced Azure DevOps repositories** (there is an
organisation-level equivalent, which locks the project one when enforced). One
toggle instead of four lines, but it restores that reach for **every pipeline in
the project**, not just this one, and leaves nothing in the YAML to explain why
the wiki suddenly works. Prefer the first unless you have a reason not to.

**A permission problem does not look like one.** Azure DevOps hides what an
identity cannot see rather than refusing it, so the failure arrives as a **404**
rather than a 403 — as a missing wiki, repository or page rather than a missing
permission. Comparing what you can see against what the pipeline can see is the
quickest way to tell them apart.

Work items are a separate scope, so an identity that raises items happily can
still be refused here.

Set `-ado-work-items=false` to use `-ado-url` purely as the project locator when
you want the register without raising anything.

## Azure DevOps

The [`extension/`](extension/) ships two pipeline tasks alongside the run tab, so
a drift pipeline does not have to inline any of this:

```yaml
- task: tf-snag-install@0            # binary from a GitHub release, onto PATH
  inputs:
    githubToken: $(GITHUB_TOKEN)

- task: tf-snag@0                    # plan -> every surface below
  inputs:
    planLogDir: terraform            # `terraform -chdir=terraform plan`
    teamsWebhook: $(teamsWebhook)    # secret variable; absent = post nothing
    workItems: dry-run
```

That writes the reports into `$(Build.ArtifactStagingDirectory)`, attaches the
JSON to the run, publishes the directory as the `tf-snag` artifact for the next
run to use as its `-baseline`, raises work items and posts the card. The full
scheduled pipeline, and every input, is in
[`extension/tasks/README.md`](extension/tasks/README.md).

Without the extension it is all still just the CLI. Capture the `-json` plan log
alongside the plan file to feed the deprecation check:

```yaml
- script: |
    terraform -chdir=terraform plan -out tfplan -json | tee plan.jsonl
    terraform -chdir=terraform show -json tfplan > plan.json
    ./tf-snag -plan plan.json -format json -exit-code=false > tf-snag.json
    # -plan-log-dir terraform: plan ran with -chdir=terraform, so its diagnostic
    # paths are relative to terraform/ - prepend it for working links.
    ./tf-snag -check all -plan plan.json -plan-log plan.jsonl -plan-log-dir terraform \
      -format markdown -exit-code=false > tf-snag.md
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

| Surface | Mechanism | `tf-snag@0` input | Needs |
|---|---|---|---|
| dedicated **tf-snag** tab | `json` attachment + [`extension/`](extension/) | `publishAttachment` (on) | the extension published & installed — see `extension/README.md` |
| **Scans** tab | `sarif` + `CodeAnalysisLogs` artifact | `publishScansTab` | the "SARIF SAST Scans Tab" extension installed in the org |
| **Summary** tab section | `markdown` + `task.uploadsummary` | `publishSummary` | nothing (built in) |
| **Boards** | `-ado-url`, one work item per finding | `workItems` | a token with Work Items (Read & Write) — see [Work items](#work-items) |
| **Teams** | Adaptive Card to a channel webhook | `teamsWebhook` | a Power Automate Workflows trigger — see [Microsoft Teams](#microsoft-teams) |
| **Wiki** | exceptions register on a wiki page | — (`-wiki-page`) | a token with Wiki (Read & Write) — see [Exceptions register](#exceptions-register-wiki) |

The tf-snag tab (schema 2) splits findings across a **Drift** / **Deprecations** /
**Pending changes** pivot, each tab carrying its count, its own collapsed
"Ignored" group, and per-finding first-seen provenance — the same coverage as the
Scans tab, in the org's own theme. The scheduled pipeline in
`../tf-snag-test-resources` uses it as the sole surface;
`../tf-drift-test-resources` still exercises the Scans/Summary path.

`junit` is still emitted by the tool for anyone who prefers the built-in Tests
tab (`PublishTestResults@2`); the scheduled pipeline uses the Scans tab instead.

**Next.** PR thread comments; a scheduled per-stack dashboard.

## Status

Early. Parser + text/JSON/markdown/JUnit/SARIF/Teams report + exit codes, covered
by tests. Deprecation check (`-check deprecations`, from the `terraform plan
-json` log) surfaces in SARIF, JSON, Teams, text and markdown. Ignore rules (file
+ inline) and `-baseline` provenance flow into JSON (schema 2). Scans-tab,
Summary, the tf-snag run tab (`extension/`), Teams notifications and Azure DevOps
work items are live.

The extension (manifest `0.6.0`) needs republishing alongside a release carrying
the work item feature: the run tab shows the work item reference and the "no
longer ignored" badge, and the `tf-snag-install` / `tf-snag` pipeline tasks are
new.

CI is GitHub Actions (`.github/workflows/ci.yml`): vet + test on every PR and on
`main`. Releases are tag-driven — push `vX.Y.Z` (or `vX.Y.Z-dev.N` / `-rc.N`,
which publish as pre-releases) and CI builds the linux + windows binaries and
attaches them to a GitHub Release. The `tf-drift-test-resources` drift pipeline
pulls the linux binary from the latest non-pre-release.
