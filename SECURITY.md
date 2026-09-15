# Security policy

## Supported versions

Security fixes go into the **latest release** only. There are no backports: a
fix ships as a new patch release, so upgrade to get it.

| Component | Supported |
|---|---|
| tf-snag CLI | the [latest release](https://github.com/wes-key/tf-snag/releases/latest) |
| Azure DevOps extension (`tf-snag@0`, `tf-snag-install@0`, the run tab) | the latest version on the Visual Studio Marketplace |

## Reporting a vulnerability

**Don't open a public issue, discussion or pull request for a security
vulnerability.**

Report it privately through
[GitHub private vulnerability reporting](https://github.com/wes-key/tf-snag/security/advisories/new).
Only the maintainers can see the report.

Please include:

- the affected component and version (`tf-snag -version`, or the extension
  version)
- what the vulnerability is and what an attacker could do with it
- the steps, command or pipeline YAML needed to reproduce it
- any fix or workaround you have in mind

Remove secrets, tokens, subscription IDs and resource names from plans, logs or
screenshots before you attach them.

## What happens next

tf-snag is maintained by one person, so these are targets rather than
guarantees:

1. You get an acknowledgement within **5 working days**.
2. We agree whether it is a vulnerability, and how severe it is.
3. The fix is developed in a private fork on the advisory, and released as a
   patch release.
4. The advisory is published once the release is out. You are credited in it,
   unless you'd rather not be.

If you don't hear back within 5 working days, comment on your report to follow
it up.

## Scope

**In scope:**

- the tf-snag CLI: plan and log parsing, ignore rules, report output, and the
  Azure DevOps, wiki and Teams clients
- the Azure DevOps pipeline tasks and the run tab
- this repository's release and publish workflows, for example a way to publish
  a tampered binary or extension

**Out of scope:**

- vulnerabilities in Terraform, Azure DevOps, GitHub or Microsoft Teams
  themselves. Report those to the vendor.
- known advisories in tf-snag's dependencies or the Go standard library.
  govulncheck, npm audit and Dependabot already check for these on every pull
  request and every week. Do report one if tf-snag is affected and the checks
  have missed it.
- findings that need an attacker to already control the pipeline, the agent or
  the Terraform plan tf-snag is given.
