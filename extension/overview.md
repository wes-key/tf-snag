# tf-snag drift report

Find out which Azure resources changed **outside Terraform**: the storage account
someone loosened in the portal, the tag a script stripped, the NSG rule added by
hand. tf-snag reads the plan Terraform already produced, so it runs no extra
Terraform and needs no extra access to your cloud.

This extension adds two pipeline tasks and a **tf-snag** tab on the pipeline run.

| | |
|---|---|
| **Install tf-snag** (`tf-snag-install@0`) | downloads the [tf-snag CLI](https://github.com/wes-key/tf-snag) from its GitHub releases and puts it on `PATH` |
| **tf-snag drift check** (`tf-snag@0`) | runs it over your plan and publishes the results: the run tab, a baseline for the next run, and optionally a Summary section, work items, a Teams card and a wiki page |
| **tf-snag** run tab | the findings for that run, next to Tests and Code Coverage |

## Quick start

Produce a plan and its JSON log, then add the two tasks:

```yaml
- script: |
    terraform -chdir=terraform plan -out tfplan -json > plan.jsonl
    terraform -chdir=terraform show -json tfplan > plan.json
  displayName: Terraform plan

- task: tf-snag-install@0

- task: tf-snag@0
  inputs:
    planLogDir: terraform   # the plan ran with -chdir=terraform
```

Open the run and select the **tf-snag** tab.

## The run tab

Findings are split across three tabs, each with its count:

| Tab | Shows |
|---|---|
| **Drift** | resources changed outside Terraform, with the exact attribute changes, e.g. `min_tls_version: "TLS1_2" → "TLS1_0"` |
| **Deprecations** | deprecation warnings from the plan |
| **Pending changes** | configuration changes not yet applied, for context |

- **Provenance**: when the previous run's report is available, each finding is marked *new* or shows when it was first seen.
- **Ignore rules**: findings suppressed by a `.tf-snag-ignore.yml` rule or an inline `# tf-snag:ignore` comment are listed separately, with the reason given.
- **Source links**: each finding links to the `.tf` file that declares the resource, in Azure Repos or GitHub.
- **Work items**: a finding tracked by an Azure Boards work item links straight to it.
- The tab follows your organisation's light or dark theme.

## What else it can publish

All optional, and all set through `tf-snag@0` inputs:

- **Azure Boards**: one work item per finding, never duplicated across runs, and optionally closed once the drift is gone
- **Microsoft Teams**: a card posted to a channel when there are new findings
- **Wiki**: a register of every ignore rule, what it waives and why
- **Summary tab**: a markdown section on the run summary
- **Scans tab**: SARIF output, for the *SARIF SAST Scans Tab* extension

Drift marks the run as **succeeded with issues** rather than failed. A tool or
plan error still fails it.

## More

- [Full pipeline example and every task input](https://github.com/wes-key/tf-snag/blob/main/extension/tasks/README.md)
- [tf-snag CLI documentation](https://github.com/wes-key/tf-snag#readme)
- [Report an issue](https://github.com/wes-key/tf-snag/issues)

The install task supports Linux and Windows agents on x64.
