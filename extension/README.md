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
tf-snag.yml step:
  tf-snag -check all -plan plan.json -plan-log plan.jsonl -plan-log-dir terraform \
    -source "$(Build.SourcesDirectory)" [-baseline prev/tf-snag.sarif] \
    -format json -exit-code=false > tf-snag.json
  echo "##vso[task.addattachment type=tf-snag.report;name=tf-snag;]tf-snag.json"

this extension:
  build-results-tab  ->  BuildHttpClient.getAttachment(type="tf-snag.report")
                     ->  parse + render
```

No custom pipeline task — it reads a run **attachment**, so any job that
publishes one of type `tf-snag.report` lights up the tab. The JSON shape is
owned by `internal/report/report.go`; `report.schema` is the contract version
(`SCHEMA_SUPPORTED` in `tab/drift.js`, currently **2**).

## Build

Requires Node 18+ and Go (for the placeholder logo only).

```
cd extension
npm ci
npm run logo         # writes images/logo.png (skip if you committed real art)
npm run package      # -> dist/<publisher>.tf-snag-tab-<version>.vsix
```

The tasks need no build of their own — they are plain Node 20 scripts with no
dependencies. See [tasks/README.md](tasks/README.md#building) for how the shared
helper reaches both task folders, and for the version/channel stamping the
publish workflow does.

## Publish (private to this org)

The extension is **private** (`"public": false` in `vss-extension.json`): it is
never listed in Marketplace search and is only installable by orgs it has been
explicitly shared with.

**One-time setup**

1. Create a Marketplace **publisher** at
   <https://marketplace.visualstudio.com/manage> whose ID matches
   `vss-extension.json` (`"publisher": "wes-key"`).
2. Create an Azure DevOps **PAT**: *All accessible organizations*, scope
   *Marketplace → Manage*, from an account that owns that publisher.
3. Add it as the GitHub repo secret **`TFX_MARKETPLACE_TOKEN`**.
4. Set repo variable **`ADO_ORG`** to the Azure DevOps org to share the private
   extension with (required for a real publish; a dry run doesn't need it).

**Publish** — run the **Publish ADO extension** workflow
(`.github/workflows/publish-extension.yml`) from the Actions tab:

- `channel: prod` → the `tf-snag-tab` id teams install; `channel: dev` →
  a separate `tf-snag-tab-dev` id (via `configs/dev.json`), with its own task ids
  and `-dev` task names (via `tools/stamp-tasks.js`), so you can iterate without
  bumping the version teams have installed.
- `dry_run: true` → builds the `.vsix` and uploads it as a run artifact without
  publishing.
- Version published is `0.<minor>.<run_number>` (minor from the manifest); bump
  the minor in `vss-extension.json` for a deliberate step.

The workflow shares the extension with `ADO_ORG` on every publish. Install it:
**Organization settings → Extensions → Shared → tf-snag drift report →
Install**.

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
