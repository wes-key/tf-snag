# tf-snag pipeline tasks

Two Azure DevOps tasks, shipped by the same extension as the
[run tab](../README.md):

| Task | Does |
|---|---|
| **`tf-snag-install`** — *Install tf-snag* | downloads the CLI from a GitHub release and puts it on `PATH` for the rest of the job |
| **`tf-snag`** — *tf-snag drift check* | runs it over a plan and publishes every surface: the tab attachment, the provenance baseline, a summary, work items, a Teams card |

Together they replace the ~150 lines of inline bash a drift pipeline used to
carry. The whole check is now:

```yaml
- task: tf-snag-install@0
  inputs:
    githubToken: $(GITHUB_TOKEN)

- task: tf-snag@0
  inputs:
    planLogDir: terraform
```

## The full pipeline

A scheduled drift check against real infrastructure, with provenance, work items
and a Teams card — the shape
`../tf-snag-test-resources/pipelines/tf-snag.yml` uses:

```yaml
trigger: none
pr: none

schedules:
  - cron: "0 6 * * 1-5"
    displayName: "Weekday 06:00 UTC drift check"
    branches: { include: [main] }
    always: true

pool:
  vmImage: Ubuntu-latest

parameters:
  # Parameters rather than variables so they appear as dropdowns when queuing a
  # run - a variable declared in YAML is fixed at compile time.
  - name: teamsNotify
    displayName: 'Post to Teams'
    type: string
    default: new
    values: [new, findings, always]
  - name: adoWorkItems
    displayName: 'Raise work items'
    type: string
    default: dry-run
    values: [dry-run, 'on', 'off']

steps:
  - checkout: self
    clean: true

  - task: TerraformInstaller@1
    inputs:
      terraformVersion: latest

  - task: tf-snag-install@0
    displayName: 'Install tf-snag'
    inputs:
      githubToken: $(GITHUB_TOKEN)      # read-only Contents on wes-key/tf-snag

  # The previous run's reports. tf-snag.sarif from it is the default baseline,
  # which is what stamps each finding new / updated with a first-seen time.
  - task: DownloadPipelineArtifact@2
    displayName: 'Fetch the previous run reports'
    continueOnError: true               # the first run on a branch has none
    inputs:
      source: specific
      project: $(System.TeamProjectId)
      pipeline: $(System.DefinitionId)
      runVersion: latestFromBranch      # newest *completed* run = the previous one
      runBranch: $(Build.SourceBranch)
      allowPartiallySucceededBuilds: true   # drift makes a run "SucceededWithIssues"
      artifact: tf-snag
      path: $(Pipeline.Workspace)/prev

  - task: AzureCLI@2
    displayName: 'Terraform init + plan'
    inputs:
      scriptType: bash
      scriptLocation: inlineScript
      azureSubscription: tf-snag-sc
      addSpnToEnvironment: true
      inlineScript: |
        set -eu -o pipefail
        az account set --subscription tf-snag
        export ARM_CLIENT_ID=$servicePrincipalId
        export ARM_CLIENT_SECRET=$servicePrincipalKey
        export ARM_SUBSCRIPTION_ID=$(az account show --query id -o tsv)
        export ARM_TENANT_ID=$(az account show --query tenantId -o tsv)
        # script(1) gives Terraform a pseudo-tty, so it colours the log.
        script -qec "make plan" /dev/null

  - task: tf-snag@0
    displayName: 'Drift check'
    inputs:
      # `make plan` runs `terraform -chdir=terraform`, so the plan log's file
      # locations are relative to terraform/ - prepend it for working links.
      planLogDir: terraform
      teamsWebhook: $(teamsWebhook)     # secret variable; absent = post nothing
      teamsNotify: '${{ parameters.teamsNotify }}'
      # Quoted: unquoted `on` / `off` are YAML booleans, and the task wants the word.
      workItems: '${{ parameters.adoWorkItems }}'
```

Nothing else is needed: the task writes `tf-snag.json` / `tf-snag.sarif` into
`$(Build.ArtifactStagingDirectory)`, attaches the JSON to the run, and publishes
the directory as the `tf-snag` artifact — which is what the
`DownloadPipelineArtifact` step above fetches back on the next run.

Drift is reported as **succeeded with issues**, not a failure: the run did its
job, and the finding is the point. A tool or plan error still fails it.

## `tf-snag-install`

| Input | Default | |
|---|---|---|
| `version` | `latest` | a release tag (`v0.1.2`) to pin, or `latest` |
| `includePrerelease` | `false` | `latest` takes the newest stable release. Tick it to take the newest tag of any kind, `-dev.N` / `-rc.N` included |
| `repository` | `wes-key/tf-snag` | change only for a fork |
| `githubToken` | — | needs read-only **Contents**; required while the repository is private |

Sets `tfSnagPath` and `tfSnagVersion` as output variables, and prepends the
binary's directory to `PATH` so later steps — including a plain `script:` step —
can just say `tf-snag`.

The download is cached under the agent tool directory
(`<tools>/tf-snag/<version>/<arch>`, the layout Microsoft's tool installers use),
so a self-hosted agent fetches a given release once. Hosted agents start empty
every run and download each time.

Only **linux/amd64** and **windows/amd64** are published
(`.github/workflows/ci.yml`); anything else fails here saying so, rather than as
"cannot execute binary file" two steps later. Self-hosted agents behind a
corporate proxy are handled — the task tunnels through `Agent.ProxyUrl` when the
job has one.

## `tf-snag`

Everything is optional. The defaults assume a plan at `plan.json`, its `-json`
log at `plan.jsonl`, and that you want the tab.

**Inputs**

| Input | Default | |
|---|---|---|
| `checks` | `all` | `all`, `drift` or `deprecations` |
| `plan` | `plan.json` | `terraform show -json` output |
| `planLog` | `plan.jsonl` | `terraform plan -json` NDJSON log — where the deprecation warnings are |
| `planLogDir` | — | repo-relative directory the plan ran in, e.g. `terraform` for `-chdir=terraform`. Makes the deprecation links resolve |
| `source` | `$(Build.SourcesDirectory)` | links findings to the `.tf` declaring them, and picks up inline `# tf-snag:ignore` comments |
| `ignoreFile` | — | empty auto-discovers `.tf-snag-ignore.yml` |
| `workingDirectory` | `$(System.DefaultWorkingDirectory)` | where tf-snag runs; relative paths resolve against it |

**Provenance**

| Input | Default | |
|---|---|---|
| `baseline` | `$(Pipeline.Workspace)/prev/tf-snag.sarif` | the previous run's SARIF. A missing file is fine and means every finding reads as new |
| `runUrl` | this run's results page | recorded against findings first seen in this run, so later runs can link one back to the run that caught it |
| `outputDirectory` | `$(Build.ArtifactStagingDirectory)` | where `tf-snag.json`, `tf-snag.sarif` and `tf-snag.md` land |
| `artifactName` | `tf-snag` | publishes that directory as a build artifact — next run's baseline. Empty publishes nothing |

**What to publish**

| Input | Default | Surface |
|---|---|---|
| `publishAttachment` | `true` | the **tf-snag** tab (a run attachment of type `tf-snag.report`) |
| `publishSummary` | `false` | a section on the run's **Summary** tab. Needs nothing installed |
| `publishScansTab` | `false` | the SARIF as a `CodeAnalysisLogs` artifact. Needs the *SARIF SAST Scans Tab* extension in the org |
| `wikiPage` | — | the exceptions register on a wiki page — see below |

**Work items** — `workItems` is `off`, `dry-run` or `on`. Start on `dry-run`: it
exercises auth, the dedup query and the permission pre-check without writing
anything. `adoUrl` defaults to this project and `adoToken` to
`$(System.AccessToken)`; the rest (`adoType`, `adoArea`, `adoRaise`, `adoClose`,
`adoClosedState`) map to the `-ado-*` flags — see
[Work Items](https://github.com/wes-key/tf-snag/wiki/Work-Items).

Using the pipeline's own identity needs the *&lt;Project&gt; Build Service*
account to have **Edit work items in this node** on the area path. Without a
usable token the task logs a warning and skips work items rather than failing on
the sign-in page Azure DevOps answers an unauthenticated call with.

**Exceptions register** — `wikiPage` publishes a register of every ignore rule to
an Azure DevOps wiki page: what is waived, why, where it is defined, and what it
currently suppresses. Rules that have stopped suppressing anything get their own
table — a waiver whose drift was fixed is one nobody needs and nobody will think
to remove. `wiki` names the wiki (default: the project wiki), `wikiDryRun` prints
the page without writing.

It shares `adoUrl` and `adoToken` with work items, so set `workItems: off` to use
them for the register alone.

The register is written as a **commit to the wiki's Git repository**, not through
the Wiki API — the build service identity has repository access and does not
appear to have wiki access, so this is what lets it publish without a PAT.

`wiki` therefore names the wiki's **repository** (`<Project>.wiki` for a project
wiki), by name or id; pin the id if the name does not resolve. The identity needs
**Contribute** on it, granted from the wiki's **⋯ → Wiki security**. Expect a
**404** rather than a 403 when it is missing: Azure DevOps hides what an identity
cannot see rather than refusing it.

**The pipeline has to reference the wiki repository**, or the job's token never
reaches it and every call returns 404 no matter what has been granted — *Limit
job authorization scope to referenced Azure DevOps repositories* scopes the token
before permissions are consulted, and the wiki is a separate repository. Either
name it in the pipeline:

```yaml
resources:
  repositories:
    - repository: wiki
      type: git
      name: <Project>.wiki

jobs:
  - job: drift
    uses:
      repositories: [wiki]
```

(declared, not checked out — the task writes through the Git API), or turn the
setting off under **Project settings → Pipelines → Settings**. The first grants
one repository to one pipeline and says so in the YAML; the second grants every
repository to every pipeline in the project and leaves no trace. The page is only written when its content has changed, so a daily run does
not bury the wiki revisions that mean something. One page per pipeline: two
pointed at the same path will overwrite each other.

**Teams** — `teamsWebhook` (or `TF_SNAG_TEAMS_WEBHOOK` in the step's `env`),
`teamsNotify` (`new` / `findings` / `always`), `teamsContext`,
`failOnTeamsError`. No webhook, no post. Pass it from a **secret** variable: it
is a credential, and anyone holding it can post to the channel. An absent
variable leaves `$(teamsWebhook)` unexpanded, which the task reads as "not
configured".

**Advanced** — `onDrift` (`warning` / `failure` / `none`) decides what a finding
does to the task result; `toolPath` points at a binary instead of using the one
on `PATH`.

**Output variables** — `driftDetected`, `reportJson`, `reportSarif`.

### What it runs, and in what order

The order is load-bearing, and is the one the inline script had:

1. **SARIF** → `tf-snag.sarif`. Written first; nothing here reads it. It carries
   the stable finding guids that make it the *next* run's baseline.
2. **markdown** → `tf-snag.md`, when `publishSummary` is on.
3. **JSON** → `tf-snag.json`. The tab attachment, and the one invocation
   carrying the `-ado-*` flags — so a finding gets one work item, and the
   reference is stamped onto the report the tab renders.
4. **console**, coloured, in a log group. Before the notification, so the log
   carries the findings even if posting them fails. Its exit code is the verdict.
5. **Teams**, last, and the only call that sees the webhook: tf-snag posts
   whenever `TF_SNAG_TEAMS_WEBHOOK` is set, so leaving it in the environment
   would make each of the four invocations post its own card.

Every report-writing call passes `-exit-code=false` — emitting a report must
never fail the step. Findings are accounted for once, by the console call.

## Building

No build step and no dependencies: the tasks are plain Node 20 scripts. An
Azure DevOps task ships with whatever is in its own folder, so vendoring
`azure-pipelines-task-lib` twice into the `.vsix` would buy little over the
~200 lines in [`common/vso.js`](common/vso.js) that read inputs, write logging
commands and run a process. That file is listed once in `vss-extension.json` and
mapped into both task folders with `packagePath`, so each task is still
self-contained in the package.

Each task folder also carries a **32x32 `icon.png`**, which is what Azure DevOps
shows beside the step in a run and in the task picker — without one it falls back
to the generic document-and-gears icon. Both are generated by
`npm run logo` alongside the Marketplace tile: the drift check wears the same
delta outline as the extension logo, so a step in the run reads as the same thing
as the tab, and the installer wears a download glyph.

`npm run package` builds the `.vsix`; see [../README.md](../README.md#build).

### A blank `filePath` input is not blank

Azure DevOps resolves a `filePath` input against the default working directory,
so one left empty reaches the task as **the repo root** — not `""`. Code that
only tests for empty then treats the source directory as a plan file, an ignore
file, or a binary to execute; `toolPath` blank once meant "execute
`/home/vsts/work/1/s`", which fails as `spawnSync … EACCES` with no output from
the child to explain it.

Read every file-valued input with `vso.fileInput()`, which treats a directory as
"not set" and passes a non-existent path through so the error names it. This is
also the one thing a local run cannot reproduce — the agent materialises those
defaults, a shell does not — so `tools/check-tasks.js` fails any `filePath` input
with no default that is read with plain `input()`.

`node tools/check-tasks.js` (run by CI on every PR) lints the pair against the
manifest: GUIDs, name/folder agreement, picklist defaults, `visibleRule` targets,
that every relative `require()` resolves in the package, and that each task has a
32x32 `icon.png`.

### Versions and channels

An agent caches a task by **id + version**, so a change does not reach it until
the version in `task.json` goes up. `tools/stamp-tasks.js` does that at publish
time, giving the tasks the same `0.<minor>.<patch>` the extension gets:

```
node tools/stamp-tasks.js --version 0.6.42 [--channel dev]
```

Task ids are global to an organisation, so the dev channel — which publishes a
separate extension id — needs separate task ids too, or installing both would
collide. `--channel dev` derives them from the production ids (stable, so they
do not change run to run) and suffixes the names, giving `tf-snag-dev@0` and
`tf-snag-install-dev@0` alongside the real ones.

**A pipeline has to name the channel it is testing.** The `-dev` suffix is part
of the task name, so YAML written against `tf-snag@0` fails on a dev-only install
with *"A task is missing. The pipeline references a task called 'tf-snag'"* — the
tasks are there, under other names. Publish `prod` for the pipelines people run,
and point a throwaway branch at `tf-snag-dev@0` when you want to try a change
without touching them.

The publish workflow runs this for you. It rewrites `task.json` in place, so
after a local dev-channel package, `git checkout extension/tasks` to put the
source back.

## Verifying end to end

1. Run the pipeline. The **Install tf-snag** step should log the release tag and
   the version line; the drift step should log `Drift reports`, a coloured
   report group and `##vso[task.addattachment …type=tf-snag.report…]`.
2. Open the run → **tf-snag** tab, and check it against the list in
   [../README.md](../README.md#verifying-end-to-end).
3. Drift present → the run is **succeeded with issues**, not failed.
4. Queue a second run: the **Fetch the previous run reports** step should find
   the `tf-snag` artifact, and findings should stop reading as new.
