# Azure Pipelines examples

Pipelines that run as written. Copy one, change the service connection and the
Terraform directory, and you have a drift check.

| File | What it is |
|---|---|
| [`pipelines/minimal.yml`](pipelines/minimal.yml) | the smallest thing that works: plan, check, **tf-snag** tab |
| [`pipelines/scheduled.yml`](pipelines/scheduled.yml) | the real drift check: provenance across runs, retirements, work items, Teams and the exceptions register |
| [`pipelines/pull-request.yml`](pipelines/pull-request.yml) | a pull request check that comments its findings on the pull request |
| [`templates/tf-snag-steps.yml`](templates/tf-snag-steps.yml) | install + check as one step template, to reference instead of copying |

Start with `minimal.yml`. Move to `scheduled.yml` when you want findings tracked
over time, which is what makes "new since yesterday" mean anything.

## Before any of them run

**Install the extension.** These use the published tf-snag tasks, so the
extension has to be installed in your organisation: **Organization settings →
Extensions**. If a task name will not resolve, that is why.

**Point them at your Terraform.** Every example assumes a `terraform/`
directory and an `azureSubscription` service connection named
`my-service-connection`. Both are marked in the files.

## Permissions

Nothing but the tab needs anything granted. Each surface you turn on needs one
more thing, and all of them are refused as a **404** rather than a 403 when
missing, because Azure DevOps hides what an identity cannot see.

| Surface | Identity needs |
|---|---|
| **tf-snag** tab, Summary, Scans | nothing |
| Work items (`workItems`) | **Edit work items in this node** on the area path |
| Exceptions register (`wikiPage`) | **Contribute** on the wiki's repository, *and* the wiki repository declared in the pipeline — see `scheduled.yml` |
| Pull request comment (`prComment`) | **Contribute to pull requests** on the repository |

The identity is the *&lt;Project&gt; Build Service*, since the tasks default
`adoToken` to `$(System.AccessToken)`.

## Using the template instead of copying

`templates/tf-snag-steps.yml` is the install and check steps with parameters, so
a pipeline can reference tf-snag rather than carry its own copy:

```yaml
resources:
  repositories:
    - repository: tf-snag
      type: github
      name: wes-key/tf-snag
      endpoint: <your GitHub service connection>

steps:
  - template: examples/templates/tf-snag-steps.yml@tf-snag
    parameters:
      planLogDir: terraform
```

It needs a GitHub service connection, which is the trade-off against copying
twenty lines of YAML once and owning them.

## Documentation

- [Task inputs, in full](../extension/tasks/README.md)
- [CLI reference and everything the tasks wrap](https://github.com/wes-key/tf-snag/wiki)
