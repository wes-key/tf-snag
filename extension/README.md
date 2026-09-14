# tf-snag — Azure DevOps extension

Adds a **tf-snag** tab to the pipeline run summary (next to Tests, Code Coverage,
Trivy, Mend, …) that renders the report produced by the
[`tf-snag`](../README.md) CLI, and the two pipeline tasks that produce it:

| Contribution | |
|---|---|
| **tf-snag** run tab | the drift / deprecations / pending-changes pivot below |
| **`tf-snag-install`** task | downloads the CLI from a GitHub release onto `PATH` |
| **`tf-snag`** task | runs it and publishes the tab attachment, the baseline, work items and a Teams card — see [tasks/README.md](tasks/README.md) |

A run-level status banner sits at the top, and the findings are split across a
pivot of three tabs, each labelled with its count:

| Tab | Shows |
|---|---|
| **Drift** | resources Terraform found changed **outside** Terraform, with the exact attribute diffs (`min_tls_version: "TLS1_2" → "TLS1_0"`), plus a collapsed **Ignored drift** group |
| **Deprecations** | deprecation warnings from the plan, plus a collapsed **Ignored deprecations** group |
| **Pending changes** | config changes not yet applied, for context |

The tab opens on the first pivot that has findings. Ignored groups show the
suppression reason and the rule that matched. When the pipeline passes
`-baseline`, every finding carries **first seen** provenance (a "new" pill, or a
date + age).

Drift and deprecations use the same table layout, and each expandable row links
to the `.tf` that declares the resource (Azure Repos Git and GitHub), the same
way the Scans tab does — needs the pipeline to pass `-source`. The tab follows
the org's light/dark theme.

**Work items.** When the pipeline passes `-ado-url`, each finding's expanded row
carries a **Work item: #1234** link straight to the item tracking it.

**No longer ignored.** A finding whose ignore rule has just been removed is
badged as such rather than as *new*, and keeps its real first-detected date
underneath — the tracking is new, the drift is not.

## How it works

```
tf-snag@0 task (tasks/tf-snag):
  tf-snag -check all -plan plan.json -plan-log plan.jsonl -plan-log-dir terraform \
    -source "$(Build.SourcesDirectory)" [-baseline prev/tf-snag.sarif] \
    -format json -exit-code=false > tf-snag.json
  ##vso[task.addattachment type=tf-snag.report;name=tf-snag;]tf-snag.json

this extension:
  build-results-tab  ->  BuildHttpClient.getAttachment(type="tf-snag.report")
                     ->  parse + render
```

The tab reads a run **attachment**, not the task's output, so the two are not
coupled: any job that publishes one of type `tf-snag.report` lights the tab up,
including a plain `script:` step running the CLI itself. The JSON shape is owned
by `internal/report/report.go`; `report.schema` is the contract version
(`SCHEMA_SUPPORTED` in `tab/drift.js`, currently **2**).

## Build

Requires Node 18+ and Go (for the artwork only).

```
cd extension
npm ci
npm run logo         # writes images/logo.png + each task's 32x32 icon.png
npm run package      # -> dist/<publisher>.tf-snag-tab-<version>.vsix
```

`npm run logo` regenerates the artwork — the Marketplace tile and
the icon each task shows in the step list and the task picker. A task with no
`icon.png` beside its `task.json` gets the generic document-and-gears icon
instead; `tools/check-tasks.js` fails the build rather than let that ship. Skip
the step if you have replaced any of them with real art.

The tasks need no build of their own — they are plain Node 20 scripts with no
dependencies. See [tasks/README.md](tasks/README.md#building) for how the shared
helper reaches both task folders, and for the version/channel stamping the
publish workflow does.

## Publish

**Merging a PR publishes it.** When a PR merged into `main` changed anything that
goes into the package — `tab/`, `tasks/`, `images/`, `vss-extension.json`,
`overview.md` or the npm manifests — `.github/workflows/release.yml` publishes the
`prod` channel. No label is needed. If the same PR carries a `release:*` label, the
CLI release goes out first and the extension follows it, so the tasks never reach
agents ahead of the binary they install. The PR gets a comment with the version.

CI packages the extension on every PR exactly as prod publishes it, so a manifest
the Marketplace would refuse fails before the merge rather than after.

`overview.md` is the Marketplace listing page. This README is not packaged.

**Version** is `0.<minor>.<patch>`: minor from `vss-extension.json`, patch one past
the newest the Marketplace already has for that channel (`tools/next-version.js`).
Bump the minor for a deliberate step. The major stays `0` — pipelines reference
the tasks as `tf-snag@0`, so it is not the CLI's version.

**Visibility.** `prod` publishes **public** once the repo variable
**`EXTENSION_PUBLIC`** is `true`, and private — shared with `ADO_ORG` — until then.
The Marketplace only accepts a public extension from a verified publisher, so set
the variable after verification. The `dev` channel is always private.

**One-time setup**

1. Create a Marketplace **publisher** at
   <https://marketplace.visualstudio.com/manage> whose ID matches
   `vss-extension.json` (`"publisher": "wes-key"`).
2. Create an Azure DevOps **PAT**: *All accessible organizations*, scope
   *Marketplace → Manage*, from an account that owns that publisher.
3. Add it as the GitHub repo secret **`TFX_MARKETPLACE_TOKEN`**.
4. Set repo variable **`ADO_ORG`** to the Azure DevOps org to share a private
   publish with.

**Going public**

1. Make `wes-key/tf-snag` public. The install task downloads the CLI from its
   releases, which nobody outside a private repo can reach.
2. Request **verification** for the publisher from the Marketplace manage page.
   Microsoft reviews the request, which can take a few days.
3. Once verified, set repo variable **`EXTENSION_PUBLIC`** to `true`. The next
   publish — a merged extension PR, or the workflow run by hand — goes public.

**By hand** — run **Publish ADO extension**
(`.github/workflows/publish-extension.yml`) from the Actions tab:

- `channel: dev` → a separate `tf-snag-tab-dev` id (via `configs/dev.json`),
  with its own task ids and `-dev` task names (via `tools/stamp-tasks.js`), so
  you can try a change on a real pipeline before merging it. Nothing publishes to
  dev automatically.
- `channel: prod` → the same publish a merge does, for when one failed.
- `dry_run: true` → builds the `.vsix` and uploads it as a run artifact without
  publishing.

A private publish is shared with `ADO_ORG`. Install it: **Organization settings →
Extensions → Shared → tf-snag drift report → Install**.

**Local publish** (fallback — needs the PAT in your shell):

```
export TFX_PAT=xxxxxxxx
npx tfx-cli extension publish \
  --manifest-globs vss-extension.json \
  --share-with <your-azure-devops-org> \
  --auth-type pat --token "$TFX_PAT" --rev-version
```

`npm run package` / `npm run package:dev` build a `.vsix` under `dist/` without
publishing.

## Local iteration

`tfx extension publish` + a pipeline re-run is the only faithful test (the tab
needs a real build attachment and the `VSS` host). For quick DOM/CSS work, open
`tab/drift.html` with a stubbed `VSS` object and a sample `tf-snag.json`.

### The tab

`dev/` is a harness that renders the real `tab/drift.css` + `tab/drift.js`
against a report file, with a stubbed `VSS` host — no publish, no pipeline run:

```
npm run tab:dev        # -> http://127.0.0.1:8730/
```

It opens on `dev/sample-report.json`, which carries every state the tab can
render: *no longer ignored*, *new*, long-standing findings with an age, an
ignored group, a work-item link, and each change action. **Load a tf-snag.json…**
swaps in the attachment from a real run — download the `tf-snag` build artifact
and point it at that — which is the quickest way to tell a data problem from a
rendering one.

The status bar shows the height the tab is asking the host for against the
window it actually has. Making the window shorter than the report is exactly
what an Azure DevOps host that will not grow the iframe looks like: the report
must scroll, not clip.

`dev/` is deliberately absent from `vss-extension.json` "files", so it is never
packaged.

### The tasks

The tasks are testable off an agent: they read their inputs from `INPUT_<NAME>`
environment variables and write logging commands to stdout, so

```
INPUT_PLAN=testdata/plan-drift.json INPUT_CHECKS=drift \
INPUT_TOOLPATH=./tf-snag INPUT_OUTPUTDIRECTORY=/tmp/out \
node tasks/tf-snag/report.js
```

runs the real thing and prints the `##vso[…]` commands an agent would act on.
`tasks/tf-snag/report.js` needs `vso.js` beside it, which the package build does
— copy `tasks/common/vso.js` in first.

## Verifying end to end

1. Run **`tf-snag.yml`**. Confirm the **tf-snag drift check** step logs
   `##vso[task.addattachment …type=tf-snag.report…]`.
2. Open the run → **tf-snag** tab.
   - findings present → amber banner, and the **Drift** / **Deprecations** /
     **Pending changes** pivot carries a count on each tab; rows expand to
     attribute diffs or deprecation detail;
   - an ignore rule matched → a collapsed "Ignored drift" / "Ignored
     deprecations" group inside the matching tab;
   - drift clean but deprecations present → the tab opens on **Deprecations**;
   - `-baseline` given → each row shows a "new" pill or a "first seen …" note;
   - nothing → green banner, each tab shows its empty state;
   - no attachment (e.g. the plan step failed first) → neutral "No tf-snag
     report for this run".
