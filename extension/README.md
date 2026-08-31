# tf-snag — Azure DevOps run tab

Adds a **tf-snag** tab to the pipeline run summary (next to Tests, Code Coverage,
Trivy, Mend, …) that renders the report produced by the
[`tf-snag`](../README.md) CLI:

- resources Terraform found changed **outside** Terraform, with the exact
  attribute diffs (`min_tls_version: "TLS1_2" → "TLS1_0"`);
- deprecation warnings from the plan, with their source locations;
- findings suppressed by an ignore rule, under an **Ignored** section
  (reason + rule);
- per-finding **first seen** provenance when the pipeline passes `-baseline`
  (a "new" pill, or a date + age);
- pending changes from configuration, for context;
- a status banner and an add/change/destroy tally.

The tab follows the org's light/dark theme.

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
  a separate `tf-snag-tab-dev` id (via `configs/dev.json`) so you can iterate
  without bumping the version teams have installed.
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

## Verifying end to end

1. Run **`tf-snag.yml`**. Confirm the drift step logs
   `##vso[task.addattachment …type=tf-snag.report…]`.
2. Open the run → **tf-snag** tab.
   - findings present → amber banner + "Changed outside Terraform" (rows
     expandable to attribute diffs), "Deprecations", and — when an ignore rule
     matched — an "Ignored" table;
   - `-baseline` given → each row shows a "new" pill or a "first seen …" note;
   - nothing → green banner, sections show their empty state;
   - no attachment (e.g. the plan step failed first) → neutral "No tf-snag
     report for this run".
