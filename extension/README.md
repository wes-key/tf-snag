# tf-snag drift report — Azure DevOps tab

Adds a **Drift** tab to the pipeline run summary (next to Tests, Code Coverage,
Trivy, Mend, …) that renders the report produced by the
[`tf-snag`](../README.md) CLI:

- resources Terraform found changed **outside** Terraform, with the exact
  attribute diffs (`min_tls_version: "TLS1_2" → "TLS1_0"`);
- pending changes from configuration, for context;
- a status banner and an add/change/destroy tally.

## How it works

```
tf-snag.yml step:
  tf-snag -plan plan.json -format json -exit-code=false > tf-snag.json
  echo "##vso[task.addattachment type=tf-snag.report;name=tf-snag;]tf-snag.json"

this extension:
  build-results-tab  ->  BuildHttpClient.getAttachment(type="tf-snag.report")
                     ->  parse + render
```

No custom pipeline task — it reads a run **attachment**, so any job that
publishes one of type `tf-snag.report` lights up the tab. The JSON shape is
owned by `internal/report/report.go`; `report.schema` is the contract version
(`SCHEMA_SUPPORTED` in `tab/drift.js`).

## Build

Requires Node 18+ and Go (for the placeholder logo only).

```
cd extension
npm ci
npm run logo         # writes images/logo.png (skip if you committed real art)
npm run package      # -> dist/<publisher>.tf-snag-tab-<version>.vsix
```

## Publish (private to this org)

One-time: create a Marketplace **publisher** at
<https://marketplace.visualstudio.com/manage> and put its ID in
`vss-extension.json` (`"publisher"`).

```
# PAT: All accessible orgs, Marketplace (Publish). Store it, don't inline it.
export TFX_PAT=xxxxxxxx

npx tfx-cli extension publish \
  --manifest-globs vss-extension.json \
  --share-with danieljamesconstruction \
  --auth-type pat --token "$TFX_PAT"
```

Then install it into the org: **Organization settings → Extensions → Shared →
tf-snag drift report → Install**.

Bump `"version"` in `vss-extension.json` on every republish (or pass
`--rev-version`).

### Dev build alongside the released one

`npm run publish:dev` uses `configs/dev.json` to publish a separate
`tf-snag-tab-dev` extension id, so you can iterate without touching the version
teams have installed.

## Local iteration

`tfx extension publish` + a pipeline re-run is the only faithful test (the tab
needs a real build attachment and the `VSS` host). For quick DOM/CSS work, open
`tab/drift.html` with a stubbed `VSS` object and a sample `tf-snag.json`.

## Verifying end to end

1. Run **`tf-snag.yml`**. Confirm the drift step logs
   `##vso[task.addattachment …type=tf-snag.report…]`.
2. Open the run → **Drift** tab.
   - drift present → amber banner + the "Changed outside Terraform" table,
     rows expandable to attribute diffs;
   - no drift → green banner, both tables show their empty state;
   - no attachment (e.g. the plan step failed first) → neutral "No drift
     report for this run".
