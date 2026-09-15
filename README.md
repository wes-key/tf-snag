<p align="center">
  <img src=".github/assets/logo.png" width="128" height="128" alt="tf-snag logo: a magnifying glass with an amber lens showing a ~ change marker">
</p>

<h1 align="center">tf-snag</h1>

<p align="center">
  <strong>Terraform drift and deprecation detection</strong><br>
  A CLI with optional Azure DevOps integration
</p>

<p align="center">
  <a href="https://github.com/wes-key/tf-snag/actions/workflows/ci.yml"><img src="https://github.com/wes-key/tf-snag/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/wes-key/tf-snag/releases/latest"><img src="https://img.shields.io/endpoint?url=https://gist.githubusercontent.com/wes-key/b87c43da7b1a3f74ed71c32a47ebea47/raw/tf-snag-release.json" alt="Release"></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/go-1.23-00ADD8?logo=go&logoColor=white" alt="Go 1.23"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue" alt="License: MIT"></a>
</p>

tf-snag reads the plan Terraform already produced and tells you which resources
changed **outside Terraform**: the storage account someone loosened in the
portal, the tag a script stripped, the NSG rule added by hand. It also reports
the deprecation warnings buried in the plan log, and exits non-zero so CI can
gate on either.

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

**📖 Documentation lives in the [wiki](https://github.com/wes-key/tf-snag/wiki).**

## Features

- **Drift and deprecations** from the plan you already have. No extra Terraform
  run, no extra cloud access. [More](https://github.com/wes-key/tf-snag/wiki/Drift-and-Deprecations)
- **Reports for every surface**: text, JSON, markdown, SARIF, JUnit and Teams
  cards. [More](https://github.com/wes-key/tf-snag/wiki/Output-Formats)
- **Tracking across runs**: which findings are new, how long the rest have been
  there, and which have gone. [More](https://github.com/wes-key/tf-snag/wiki/Baselines-and-Provenance)
- **Ignore rules** in a file or inline in your `.tf`, with a reason for each.
  [More](https://github.com/wes-key/tf-snag/wiki/Ignoring-Findings)

**Optional Azure DevOps integration**

- **Pipeline tasks and a run tab** that show every finding on the pipeline run.
  [More](https://github.com/wes-key/tf-snag/wiki/Azure-DevOps-Pipelines)
- **One work item per finding**, never duplicated, updated as findings are
  ignored, reinstated or fixed. [More](https://github.com/wes-key/tf-snag/wiki/Work-Items)
- **An exceptions register** of every ignore rule, published to a wiki.
  [More](https://github.com/wes-key/tf-snag/wiki/Exceptions-Register)
- **Teams alerts** when something new appears.
  [More](https://github.com/wes-key/tf-snag/wiki/Microsoft-Teams)

## Install

Download `tf-snag` (Linux x64) or `tf-snag.exe` (Windows x64) from the
[latest release](https://github.com/wes-key/tf-snag/releases/latest), or build
it with Go 1.23+:

```sh
go install github.com/wes-key/tf-snag@latest
```

## Quick start

```sh
terraform plan -out tfplan -json > plan.jsonl
terraform show -json tfplan > plan.json
tf-snag -check all -plan plan.json -plan-log plan.jsonl
```

Exit code `0` means clean; `2` means an un-ignored finding, or an error. Every
flag is in the [CLI reference](https://github.com/wes-key/tf-snag/wiki/CLI-Reference).

**In Azure Pipelines**, with the extension installed:

```yaml
- task: tf-snag-install@0

- task: tf-snag@0
  inputs:
    planLogDir: terraform   # the plan ran with -chdir=terraform
```

See [Azure DevOps Pipelines](https://github.com/wes-key/tf-snag/wiki/Azure-DevOps-Pipelines)
for the full pipeline, or for running the CLI without the extension.

## Contributing

Every change starts with an issue. See [CONTRIBUTING.md](CONTRIBUTING.md) for
branch naming, the PR template and how releases are labelled.

```sh
go vet ./...
go test ./...
go run . -plan testdata/plan-drift.json
```

The Azure DevOps extension is in [`extension/`](extension/). Releases are cut by
merging labelled PRs; see [Releasing](https://github.com/wes-key/tf-snag/wiki/Releasing).

## Licence

[MIT](LICENSE)
