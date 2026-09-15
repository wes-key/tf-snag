# Contributing to tf-snag

Thanks for helping. This page covers how a change gets from an idea to `main`.
Every change follows these steps, however small:

1. [Open an issue](#1-open-an-issue)
2. [Branch from `main`, named after the issue](#2-create-a-branch)
3. [Make the change, with tests](#3-make-the-change)
4. [Open a pull request using the template](#4-open-a-pull-request)
5. [Get a review and merge](#5-review-and-merge)

## 1. Open an issue

**Every pull request needs an issue.** A PR that has no issue will be asked to
open one before it gets a review.

With an issue, the problem and the approach get agreed before anyone writes
code. It also gives the change a number that ties together the branch, the PR
and the release notes.

- Search the [existing issues](https://github.com/wes-key/tf-snag/issues) first.
- Use one of the forms: **Bug report** or **Feature request**.
- For anything bigger than a small fix, wait until the issue has been discussed
  before you start. That way nobody spends time on a change that won't be
  accepted.
- Keep each issue to one problem. If a change has several independent parts,
  open an issue for each.

## 2. Create a branch

Branch from an up-to-date `main`. Name the branch like this:

```
<type>/<issue-number>-<short-description>
```

| Type       | Use for                                          |
|------------|--------------------------------------------------|
| `feature`  | new behaviour: flags, task inputs, report fields |
| `fix`      | bug fixes                                        |
| `docs`     | README, wiki or comment-only changes             |
| `chore`    | CI, dependencies, tooling, refactoring           |

Rules for the name:

- The issue number is required.
- The description is lowercase, kebab-case, and a few words at most.

```sh
git switch main
git pull
git switch -c fix/42-sarif-missing-rule-id
```

Examples: `feature/57-teams-retry-on-429`, `docs/61-ignore-rules-examples`,
`chore/64-bump-tfx-cli`.

## 3. Make the change

Keep each PR to the one issue it's for. Split unrelated fixes and refactors into
their own issues and PRs.

### The CLI (Go)

You need Go 1.23 or later.

```sh
go vet ./...
go test ./...
go run . -plan testdata/plan-drift.json
```

- Add or update tests for any behaviour you change. Plan fixtures live in
  [`testdata/`](testdata/).
- Run `gofmt` on your code.
- If the JSON report shape changes (`internal/report/report.go`), bump
  `report.schema` and update `SCHEMA_SUPPORTED` in
  [`extension/tab/drift.js`](extension/tab/drift.js) to match.

### The Azure DevOps extension

You need Node 20. See [`extension/README.md`](extension/README.md) for the full
details.

```sh
cd extension
npm ci
find tab tasks tools -name '*.js' -print0 | xargs -0 -n1 node --check
node tools/check-tasks.js
npm run tab:dev      # preview the run tab at http://127.0.0.1:8730/
```

- **Merging a PR that touches the extension package publishes it to the
  Marketplace.** This covers `tab/`, `tasks/`, `images/`,
  `vss-extension.json`, `overview.md` and the npm manifests. Before you ask for
  a review, test the change against a real pipeline with the `dev` channel. To
  do that, run **Publish ADO extension** with `channel: dev` from the Actions
  tab.
- Task inputs are a contract with users' pipelines. Renaming or removing an
  input breaks pipelines that pin `tf-snag@0`.

### Documentation

If you change user-facing behaviour, update the
[wiki](https://github.com/wes-key/tf-snag/wiki) and, where relevant, the README.
Do this in the same PR, or say in the PR which wiki pages need an edit.

### Commits

Write commit messages in the imperative, and describe what the change does:
"Add retry to Teams notifications", not "Added retry" or "fixes". PRs are
squash-merged, so the PR title becomes the commit on `main`. Getting the title
right matters more than tidying the individual commits.

## 4. Open a pull request

- Target `main`.
- **Fill in the pull request template.** Don't delete sections. Write "n/a" for
  any that don't apply.
- Link the issue with a closing keyword (`Closes #42`) so it closes on merge.
- The title should be a short imperative summary. The PR number is added on
  merge.
- Open the PR as a **draft** while work is still in progress.
- CI must pass: `go vet`, `go test`, the extension syntax check, the task lint
  and a packaging run.

### Release labels

Releases are cut when a PR is merged. For the CLI, the label on the PR decides
the release:

| Label           | Bump                | For                                              |
|-----------------|---------------------|--------------------------------------------------|
| `release:patch` | `v1.2.3` → `v1.2.4` | fixes                                            |
| `release:minor` | `v1.2.3` → `v1.3.0` | new flags, inputs, report fields                 |
| `release:major` | `v1.2.3` → `v2.0.0` | anything that makes users change their pipelines |
| *(none)*        | no CLI release      | docs, CI, refactoring                            |

Use **at most one** release label. A PR with more than one fails the release.
Suggest a label in the template; a maintainer adds it or corrects it. The
extension needs no label, because any change to its package publishes it. See
[Releasing](https://github.com/wes-key/tf-snag/wiki/Releasing) for details.

## 5. Review and merge

- Each PR needs an approving review from a maintainer before it's merged.
- Reply to every review comment, either with a change or with a reason. The
  person who opened a thread resolves it.
- Keep your branch up to date with `main`, and rebase if there are conflicts.
- A maintainer squash-merges the PR once it's approved and CI is green. After
  that, the branch is deleted.

## Reporting security issues

Don't open a public issue for a security vulnerability. Report it privately
through [GitHub security advisories](https://github.com/wes-key/tf-snag/security/advisories/new).

## Licence

tf-snag is licensed under [MIT](LICENSE). Contributions are accepted under the
same licence.
