<!--
  Before opening this PR, check that:
  - there is an issue for this change (see CONTRIBUTING.md)
  - the branch is named <type>/<issue-number>-<short-description>
  - the title is a short imperative summary, e.g. "Add retry to Teams notifications"
  Fill in every section below; write "n/a" where one does not apply.
-->

## Issue

Closes #

## What changed

<!-- What this PR does and why, in a few sentences or bullets. -->

## How it was tested

<!--
  Commands run, fixtures added, pipelines exercised. For extension changes, link
  the dev-channel pipeline run that shows it working.
-->

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] Breaking change (users must change their pipelines, flags or ignore rules)
- [ ] Documentation
- [ ] CI, tooling or refactoring

## Release

<!-- A maintainer applies the label; suggest one here. -->

- [ ] `release:patch`
- [ ] `release:minor`
- [ ] `release:major`
- [ ] No CLI release
- [ ] Touches the extension package, so merging publishes it to the Marketplace

## Checklist

- [ ] `go vet ./...` and `go test ./...` pass
- [ ] Tests added or updated for changed behaviour
- [ ] Extension changes pass `node tools/check-tasks.js` and were tried on the `dev` channel
- [ ] Report schema bumped (with `SCHEMA_SUPPORTED`) if the JSON shape changed
- [ ] README / wiki updated, or the pages needing an edit are listed above
