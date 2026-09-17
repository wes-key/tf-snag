/* tf-snag task.
 *
 * Runs the tf-snag CLI over a Terraform plan and publishes everything the run
 * page can show: the JSON attachment the tf-snag tab renders, the SARIF that
 * becomes the next run's provenance baseline, an optional markdown summary and
 * Scans-tab artifact, work items, and a Teams card.
 *
 * This is the script every drift pipeline used to inline. The order matters and
 * is the same as it was there:
 *
 *   1. SARIF   - written first; it is next run's -baseline, nothing reads it here
 *   2. summary - markdown, when publishSummary is on
 *   3. JSON    - the tab attachment, and the ONE invocation carrying the -ado
 *                flags, so a work item is raised once and its reference is
 *                stamped onto the report the tab renders
 *   4. console - the coloured report, so the log carries the findings even if
 *                posting them later fails. Its exit code is the verdict
 *   5. Teams   - last, and the only call that sees the webhook
 *
 * Report-writing calls all pass -exit-code=false: emitting a report must never
 * fail the step. Findings are accounted for once, by the console call.
 */
"use strict";

var fs = require("fs");
var path = require("path");

var vso = require("./vso");

var ATTACHMENT_TYPE = "tf-snag.report";
var SCANS_ARTIFACT = "CodeAnalysisLogs";

vso.run(main);

function main() {
  var cfg = readInputs();

  fs.mkdirSync(cfg.outDir, { recursive: true });
  var out = {
    json: path.join(cfg.outDir, "tf-snag.json"),
    sarif: path.join(cfg.outDir, "tf-snag.sarif"),
    markdown: path.join(cfg.outDir, "tf-snag.md"),
  };

  var common = commonArgs(cfg);
  var baseline = baselineArgs(cfg);
  var env = execEnv(cfg);

  vso.section("Drift reports");
  // The SARIF carries the stable finding guids, so it is what lets the next run
  // tell a finding it has seen before from a new one. -run-url is on this call
  // as well as the notification: a finding first seen today records the run that
  // caught it here, and later runs carry that forward.
  writeReport(cfg, out.sarif, common.concat(baseline, ["-format", "sarif"]), env, "SARIF (next run's baseline)");

  if (cfg.publishSummary) {
    writeReport(cfg, out.markdown, common.concat(["-format", "markdown"]), env, "markdown summary");
  }

  // The JSON call is the one carrying the -ado flags, so a failure here can be
  // the work items or the wiki rather than the report itself - and the report
  // will already have been written by the time either runs. Publish whatever was
  // produced before re-raising, so an unreachable Azure DevOps costs you the
  // side-effect, not the tab.
  try {
    writeReport(cfg, out.json, common.concat(baseline, cfg.adoArgs, ["-format", "json"]), env, "JSON (tf-snag tab)");
  } finally {
    publish(cfg, out);
  }

  vso.section("Drift check");
  vso.group("tf-snag report");
  var driftCode = vso.exec(cfg.bin, common.concat(["-color=always"]), { cwd: cfg.cwd, env: env });
  vso.endGroup();

  if (driftCode !== 0 && driftCode !== 2) {
    throw new vso.TaskError("tf-snag exited " + driftCode + " - see the report above");
  }
  vso.setOutput("driftDetected", driftCode === 2 ? "true" : "false");
  vso.setOutput("reportJson", out.json);
  vso.setOutput("reportSarif", out.sarif);

  notifyTeams(cfg, common, baseline, env);
  return verdict(cfg, driftCode);
}

// writeReport runs one format into a file. -exit-code=false keeps a finding from
// failing the call, so a non-zero status here is a real tool error.
function writeReport(cfg, file, args, env, what) {
  var full = args.concat(["-exit-code=false"]);
  vso.log("writing " + what + " -> " + file);
  // The exact command, so a failure here can be reproduced by hand rather than
  // reasoned about from the inputs.
  vso.log("  " + vso.describe(cfg.bin, full));
  var code = vso.execToFile(cfg.bin, full, file, { cwd: cfg.cwd, env: env });
  if (code !== 0) {
    throw new vso.TaskError("tf-snag exited " + code + " writing " + what + ": " + vso.describe(cfg.bin, full));
  }
}

function publish(cfg, out) {
  // Size-checked because publish also runs on the failure path: an empty file
  // would give the tab an attachment it can only render as an error.
  if (cfg.publishAttachment && nonEmpty(out.json)) {
    // The type is the contract with the tab (tab/drift.js). Keep it exact.
    vso.addAttachment(ATTACHMENT_TYPE, "tf-snag", out.json);
  }
  if (cfg.publishSummary) {
    vso.uploadSummary(out.markdown);
  }
  if (cfg.publishScansTab) {
    // The Scans tab is the "SARIF SAST Scans Tab" extension reading an artifact
    // with this exact name.
    vso.uploadArtifact(SCANS_ARTIFACT, out.sarif);
  }
  if (cfg.artifactName) {
    // The whole output directory, because what next run wants back is the SARIF
    // and what a human wants back is the markdown.
    vso.uploadArtifact(cfg.artifactName, cfg.outDir);
  }
}

function nonEmpty(file) {
  try {
    return fs.statSync(file).size > 0;
  } catch (e) {
    return false;
  }
}

function notifyTeams(cfg, common, baseline, env) {
  if (!cfg.teamsWebhook) {
    vso.log("no Teams webhook configured - skipping the notification");
    return;
  }
  vso.section("Teams notification");
  var teamsEnv = Object.assign({}, env, { TF_SNAG_TEAMS_WEBHOOK: cfg.teamsWebhook });
  // The baseline matters here as much as it does for the reports: it is what
  // marks a finding new, and so what lets "notify: new" stay quiet about drift
  // nobody has fixed yet. -exit-code=false so the only thing that can make this
  // non-zero is a failed POST.
  var args = common.concat(baseline, [
    "-exit-code=false",
    "-teams-notify", cfg.teamsNotify,
    "-teams-context", cfg.teamsContext,
  ]);
  var code = vso.exec(cfg.bin, args, { cwd: cfg.cwd, env: teamsEnv });
  if (code === 0) return;
  // A notification you believe is going out but is not is worse than a loud
  // failure, so this fails the step unless the pipeline says otherwise.
  var message = "tf-snag could not post to Microsoft Teams (exit " + code + ")";
  if (cfg.failOnTeamsError) {
    throw new vso.TaskError(message);
  }
  vso.logIssue("warning", message);
}

// verdict turns the console call's exit code into the task result. Drift is a
// warning by default: the run did its job, and the finding is the point.
function verdict(cfg, driftCode) {
  if (driftCode !== 2) return;
  var message = "tf-snag found drift, a deprecation or a retirement - see the tf-snag tab";
  if (cfg.onDrift === "none") {
    vso.log(message);
    return;
  }
  if (cfg.onDrift === "failure") {
    vso.logIssue("error", message);
    vso.setResult("Failed", message);
    return;
  }
  vso.logIssue("warning", message);
  vso.setResult("SucceededWithIssues", message);
}

// --- inputs ---------------------------------------------------------------

function readInputs() {
  var cfg = {};

  cfg.cwd = vso.input("workingDirectory") || vso.variable("System.DefaultWorkingDirectory") || process.cwd();
  cfg.outDir = vso.input("outputDirectory") || vso.variable("Build.ArtifactStagingDirectory") || cfg.cwd;
  cfg.bin = resolveBinary();

  cfg.checks = vso.pickInput("checks",
    ["drift", "deprecations", "all", "retirements", "all,retirements", "drift,retirements"], "all");
  // The picklist values are -check lists, so membership decides which inputs a
  // run needs rather than a three-way switch.
  var selected = cfg.checks.split(",");
  cfg.drift = selected.indexOf("drift") >= 0 || selected.indexOf("all") >= 0;
  cfg.deprecations = selected.indexOf("deprecations") >= 0 || selected.indexOf("all") >= 0;
  cfg.retirements = selected.indexOf("retirements") >= 0;
  // Retirements read the same plan file drift does, so the plan is required
  // whenever either is on.
  cfg.needsPlan = cfg.drift || cfg.retirements;

  cfg.plan = cfg.needsPlan ? resolve(cfg.cwd, vso.fileInput("plan") || "plan.json") : "";
  cfg.planLog = cfg.deprecations ? resolve(cfg.cwd, vso.fileInput("planLog") || "plan.jsonl") : "";
  cfg.planLogDir = vso.input("planLogDir");
  cfg.retirementsFailWithin = cfg.retirements ? vso.input("retirementsFailWithin") : "";
  cfg.retirementsFile = cfg.retirements ? vso.fileInput("retirementsFile") : "";
  if (cfg.retirementsFile) cfg.retirementsFile = resolve(cfg.cwd, cfg.retirementsFile);
  cfg.source = vso.input("source") || vso.variable("Build.SourcesDirectory");
  cfg.ignoreFile = vso.fileInput("ignoreFile");
  cfg.runURL = vso.input("runUrl") || defaultRunURL();

  // These are read from stdin when the flag is absent, which on an agent means
  // the task hangs until the job times out. Fail now, saying which file.
  if (cfg.plan && !fs.existsSync(cfg.plan)) {
    throw new vso.TaskError("no plan at " + cfg.plan + " - is `terraform show -json` writing it before this task?");
  }
  if (cfg.planLog && !fs.existsSync(cfg.planLog)) {
    throw new vso.TaskError(
      "no plan log at " + cfg.planLog + " - the deprecation check reads `terraform plan -json` output; " +
      "capture it, or set checks to drift"
    );
  }

  cfg.baseline = "";
  var baseline = vso.fileInput("baseline");
  if (baseline) {
    baseline = resolve(cfg.cwd, baseline);
    if (fs.existsSync(baseline)) {
      cfg.baseline = baseline;
    } else {
      // The first run on a branch has no predecessor, and neither does a run
      // after artifact retention has expired. Neither is an error.
      vso.log("no baseline at " + baseline + " - every finding will read as new");
    }
  }

  cfg.publishAttachment = vso.boolInput("publishAttachment", true);
  cfg.publishSummary = vso.boolInput("publishSummary", false);
  cfg.publishScansTab = vso.boolInput("publishScansTab", false);
  cfg.artifactName = vso.input("artifactName");

  cfg.teamsWebhook = vso.input("teamsWebhook") || (vso.isSet(process.env.TF_SNAG_TEAMS_WEBHOOK) ? process.env.TF_SNAG_TEAMS_WEBHOOK : "");
  vso.setSecret(cfg.teamsWebhook);
  cfg.teamsNotify = vso.pickInput("teamsNotify", ["new", "findings", "always"], "new");
  cfg.teamsContext = vso.input("teamsContext") || defaultTeamsContext();
  cfg.failOnTeamsError = vso.boolInput("failOnTeamsError", true);

  cfg.onDrift = vso.pickInput("onDrift", ["warning", "failure", "none"], "warning");

  readAzureDevOpsInputs(cfg);
  return cfg;
}

// readAzureDevOpsInputs builds the flags for both Azure DevOps surfaces. They
// share a project URL and a token, and both ride on the one JSON invocation, so
// resolving them together is what keeps a single missing token from being
// reported twice - or worse, reported for one and silently skipped for the other.
function readAzureDevOpsInputs(cfg) {
  cfg.adoArgs = [];
  cfg.adoToken = vso.input("adoToken") || (vso.isSet(process.env.TF_SNAG_ADO_TOKEN) ? process.env.TF_SNAG_ADO_TOKEN : "");

  var mode = vso.pickInput("workItems", ["off", "dry-run", "on"], "off");
  var wikiPage = vso.input("wikiPage");
  var prComment = vso.pickInput("prComment", ["off", "findings", "new"], "off");
  var wants = [];
  if (mode !== "off") wants.push("workItems is " + mode);
  if (wikiPage) wants.push("wikiPage is set");
  if (prComment !== "off") wants.push("prComment is " + prComment);
  if (!wants.length) return;

  var url = vso.input("adoUrl") || defaultProjectURL();
  if (!url) {
    throw new vso.TaskError(wants.join(" and ") + " but no adoUrl could be worked out for this project");
  }
  // System.AccessToken arrives as an unexpanded macro when the job has no OAuth
  // token. Saying so beats letting tf-snag fail on the sign-in page Azure DevOps
  // answers an unauthenticated call with.
  if (!cfg.adoToken) {
    vso.logIssue("warning", wants.join(" and ") + " but no adoToken is available - skipping both");
    return;
  }

  cfg.adoArgs = ["-ado-url", url];

  if (mode === "off") {
    // -ado-url doubles as "raise work items", so the wiki alone has to say no.
    cfg.adoArgs.push("-ado-work-items=false");
  } else {
    cfg.adoArgs.push("-ado-type", vso.input("adoType") || "Task",
      "-ado-raise", vso.pickInput("adoRaise", ["new", "findings"], "new"));
    var area = vso.input("adoArea");
    if (area) cfg.adoArgs.push("-ado-area", area);
    if (mode === "dry-run") {
      cfg.adoArgs.push("-ado-dry-run");
    } else if (vso.boolInput("adoClose", true)) {
      cfg.adoArgs.push("-ado-close");
      if (vso.input("adoClosedState")) cfg.adoArgs.push("-ado-closed-state", vso.input("adoClosedState"));
    }
  }

  if (wikiPage) {
    cfg.adoArgs.push("-wiki-page", wikiPage);
    var wikiName = vso.input("wiki");
    if (wikiName) cfg.adoArgs.push("-wiki", wikiName);
    if (vso.boolInput("wikiDryRun", false)) cfg.adoArgs.push("-wiki-dry-run");
  }

  if (prComment !== "off") {
    cfg.adoArgs.push("-pr-comment", prComment);
    // The pull request and repository come from the agent. tf-snag reads the
    // same variables itself, but passing them keeps the command in the log
    // complete - and lets a job that knows better override them.
    var pr = vso.variable("System.PullRequest.PullRequestId");
    if (pr) cfg.adoArgs.push("-pr-id", pr);
    var repo = vso.variable("Build.Repository.ID");
    if (repo) cfg.adoArgs.push("-pr-repo", repo);
    if (vso.boolInput("prCommentDryRun", false)) cfg.adoArgs.push("-pr-comment-dry-run");
  }
}

function commonArgs(cfg) {
  var args = ["-check", cfg.checks];
  if (cfg.needsPlan) args.push("-plan", cfg.plan);
  if (cfg.retirementsFile) args.push("-retirements", cfg.retirementsFile);
  if (cfg.retirements && cfg.retirementsFailWithin) {
    args.push("-retirements-fail-within", cfg.retirementsFailWithin);
  }
  if (cfg.deprecations) {
    args.push("-plan-log", cfg.planLog);
    // `terraform plan -json` reports diagnostic paths relative to the directory
    // it ran in, so this is what makes the tab's and the Scans tab's links
    // resolve from the repo root.
    if (cfg.planLogDir) args.push("-plan-log-dir", cfg.planLogDir);
  }
  if (cfg.source) args.push("-source", cfg.source);
  if (cfg.ignoreFile) args.push("-ignore", cfg.ignoreFile);
  if (cfg.runURL) args.push("-run-url", cfg.runURL);
  return args;
}

function baselineArgs(cfg) {
  return cfg.baseline ? ["-baseline", cfg.baseline] : [];
}

// execEnv is the environment every call but the Teams one gets. tf-snag posts a
// card whenever it sees TF_SNAG_TEAMS_WEBHOOK, so leaving it in place would make
// each of the four invocations here post its own.
function execEnv(cfg) {
  var env = Object.assign({}, process.env);
  delete env.TF_SNAG_TEAMS_WEBHOOK;
  // The token is inert on its own: tf-snag only talks to Azure DevOps when
  // -ado-url is passed, which is the JSON call alone.
  if (cfg.adoToken) env.TF_SNAG_ADO_TOKEN = cfg.adoToken;
  return env;
}

function resolveBinary() {
  var explicit = vso.fileInput("toolPath");
  if (explicit) {
    if (!fs.existsSync(explicit)) {
      throw new vso.TaskError("no tf-snag binary at " + explicit);
    }
    return path.resolve(explicit);
  }
  var found = vso.which("tf-snag");
  if (found) {
    vso.log("using tf-snag at " + found);
  } else {
    throw new vso.TaskError(
      "tf-snag is not on PATH - add the \"Install tf-snag\" (tf-snag-install) task before this one, " +
      "or point toolPath at a binary"
    );
  }
  return found;
}

function resolve(cwd, file) {
  return path.isAbsolute(file) ? file : path.join(cwd, file);
}

function defaultRunURL() {
  var collection = vso.variable("System.CollectionUri");
  var project = vso.variable("System.TeamProjectId");
  var build = vso.variable("Build.BuildId");
  if (!collection || !project || !build) return "";
  return collection + project + "/_build/results?buildId=" + build;
}

function defaultProjectURL() {
  var collection = vso.variable("System.CollectionUri");
  var project = vso.variable("System.TeamProject");
  // System.CollectionUri already ends in "/".
  return collection && project ? collection + encodeURIComponent(project) : "";
}

function defaultTeamsContext() {
  var parts = [
    vso.variable("Build.DefinitionName"),
    vso.variable("Build.SourceBranchName"),
    vso.variable("Build.BuildNumber") ? "run " + vso.variable("Build.BuildNumber") : "",
  ].filter(Boolean);
  return parts.join(" · ");
}
