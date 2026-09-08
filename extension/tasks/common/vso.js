/* Agent-side helpers shared by the tf-snag pipeline tasks.
 *
 * Deliberately dependency-free. An Azure DevOps task ships with whatever is in
 * its own folder, so the alternative is vendoring azure-pipelines-task-lib (and
 * its tree) twice into the .vsix for the handful of primitives below: reading
 * inputs, writing logging commands and running a process. This file is listed
 * once in vss-extension.json and mapped into both task folders with
 * `packagePath`, so each task still gets a self-contained copy in the package.
 *
 * References:
 *   inputs           -> INPUT_<NAME>, uppercased with non-alphanumerics as "_"
 *   logging commands -> https://aka.ms/vso-commands
 */
"use strict";

var child_process = require("child_process");
var fs = require("fs");
var path = require("path");

// Azure DevOps leaves an unresolvable macro in place rather than substituting an
// empty string, so "$(System.AccessToken)" arriving verbatim means "there is no
// such variable" - not a token. Every value that can come from a macro is
// filtered through this.
function isSet(value) {
  return typeof value === "string" && value !== "" && value.indexOf("$(") !== 0;
}

function envKey(name) {
  return "INPUT_" + name.replace(/[^a-zA-Z0-9]/g, "_").toUpperCase();
}

// input returns a trimmed input value, or "" when it is unset or arrived as an
// unexpanded macro. Pass {required: true} to fail the task instead.
function input(name, opts) {
  var raw = process.env[envKey(name)];
  var value = typeof raw === "string" ? raw.trim() : "";
  if (!isSet(value)) value = "";
  if (!value && opts && opts.required) {
    throw new TaskError("input " + JSON.stringify(name) + " is required");
  }
  return value;
}

// fileInput is input() for a task.json "filePath" input that names a FILE.
//
// An empty filePath input does not arrive empty. Azure DevOps resolves the value
// against the default working directory, so a filePath left blank reaches the
// task as the repo root - and code that only checks for "" then treats the
// source directory as a plan, an ignore file, or a binary to execute. A
// directory is never the answer for these, so it means "not set".
//
// Left alone deliberately: a path that does not exist is passed through, so the
// resulting error names what was asked for rather than silently ignoring it.
function fileInput(name) {
  var value = input(name);
  if (!value) return "";
  try {
    if (fs.statSync(value).isDirectory()) return "";
  } catch (e) { /* not there - let whatever needs it say so */ }
  return value;
}

function boolInput(name, fallback) {
  var value = input(name).toLowerCase();
  if (!value) return !!fallback;
  return value === "true" || value === "1" || value === "yes";
}

// pickInput enforces the task.json options agent-side too: a pipeline can feed
// any string in through a variable, and a typo is better caught here than as an
// opaque flag error from the CLI three invocations later.
function pickInput(name, allowed, fallback) {
  var value = input(name) || fallback;
  if (allowed.indexOf(value) === -1) {
    throw new TaskError(
      "input " + JSON.stringify(name) + " must be one of " + allowed.join(", ") + " (got " + JSON.stringify(value) + ")"
    );
  }
  return value;
}

function variable(name) {
  var value = process.env[name.replace(/\./g, "_").toUpperCase()];
  return isSet(value) ? value : "";
}

// TaskError is a failure the pipeline author can act on: reported as the task
// result message with no stack trace. Anything else that escapes is a bug in
// the task and keeps its stack.
function TaskError(message) {
  Error.call(this, message);
  this.message = message;
  this.name = "TaskError";
}
TaskError.prototype = Object.create(Error.prototype);
TaskError.prototype.constructor = TaskError;

function escapeValue(value) {
  return String(value === undefined || value === null ? "" : value)
    .replace(/%/g, "%AZP25")
    .replace(/\r/g, "%0D")
    .replace(/\n/g, "%0A")
    .replace(/]/g, "%5D")
    .replace(/;/g, "%3B");
}

function escapeMessage(value) {
  return String(value === undefined || value === null ? "" : value)
    .replace(/%/g, "%AZP25")
    .replace(/\r/g, "%0D")
    .replace(/\n/g, "%0A");
}

function command(name, properties, message) {
  var props = "";
  Object.keys(properties || {}).forEach(function (key) {
    var value = properties[key];
    if (value === undefined || value === null || value === "") return;
    props += key + "=" + escapeValue(value) + ";";
  });
  process.stdout.write("##vso[" + name + (props ? " " + props : "") + "]" + escapeMessage(message) + "\n");
}

// describe renders a command for a log line or an error message. Arguments are
// only ever paths, URLs and flags - credentials reach tf-snag through the
// environment precisely so they cannot end up here.
function describe(file, args) {
  return [file].concat(args || []).map(function (part) {
    return /[\s"]/.test(part) ? JSON.stringify(part) : part;
  }).join(" ");
}

function log(message) {
  process.stdout.write(message + "\n");
}

function section(title) {
  log("##[section]" + title);
}

function group(title) {
  log("##[group]" + title);
}

function endGroup() {
  log("##[endgroup]");
}

function debug(message) {
  command("task.debug", {}, message);
}

function logIssue(type, message) {
  command("task.logissue", { type: type }, message);
}

function setResult(result, message) {
  command("task.complete", { result: result }, message);
}

function setOutput(name, value) {
  command("task.setvariable", { variable: name, isOutput: "true" }, value);
}

function prependPath(dir) {
  command("task.prependpath", {}, dir);
}

// setSecret asks the agent to mask a value in the log. Tokens reach a task
// through inputs, and an input fed from a plain (non-secret) variable is not
// masked for us.
function setSecret(value) {
  if (isSet(value)) command("task.setsecret", {}, value);
}

function addAttachment(type, name, file) {
  command("task.addattachment", { type: type, name: name }, file);
}

function uploadSummary(file) {
  command("task.uploadsummary", {}, file);
}

function uploadArtifact(artifactName, file) {
  command("artifact.upload", { containerfolder: artifactName, artifactname: artifactName }, file);
}

// exec runs a process with its output going straight to the build log, and
// returns its exit code. Arguments are passed as an array, so a path with a
// space in it needs no quoting and cannot be re-split by a shell.
function exec(file, args, opts) {
  var options = opts || {};
  var result = child_process.spawnSync(file, args, {
    cwd: options.cwd,
    env: options.env || process.env,
    stdio: ["ignore", options.stdout === undefined ? "inherit" : options.stdout, "inherit"],
    windowsHide: true,
  });
  if (result.error) {
    // ENOENT / EACCES / ENOEXEC land here and the child produces no output at
    // all, so the command has to be in the message or there is nothing to go on.
    throw new TaskError("could not run " + describe(file, args) + ": " + result.error.message);
  }
  if (result.signal) {
    throw new TaskError(describe(file, args) + " was killed by signal " + result.signal);
  }
  return result.status;
}

// execToFile is exec with stdout redirected to a file - how each report format
// is captured, since tf-snag writes the report to stdout and its progress and
// work-item log to stderr.
function execToFile(file, args, outFile, opts) {
  var fd = fs.openSync(outFile, "w");
  try {
    return exec(file, args, Object.assign({}, opts || {}, { stdout: fd }));
  } finally {
    fs.closeSync(fd);
  }
}

// which resolves a bare tool name against PATH, the way a shell would. The
// installer task prepends its cache dir to PATH, so by the time the report task
// runs "tf-snag" is on it.
function which(tool) {
  if (tool.indexOf("/") !== -1 || tool.indexOf(path.sep) !== -1) {
    return fs.existsSync(tool) ? path.resolve(tool) : "";
  }
  var exts = process.platform === "win32"
    ? (process.env.PATHEXT || ".EXE;.CMD;.BAT").split(";")
    : [""];
  var dirs = (process.env.PATH || "").split(path.delimiter);
  for (var i = 0; i < dirs.length; i++) {
    if (!dirs[i]) continue;
    for (var j = 0; j < exts.length; j++) {
      var candidate = path.join(dirs[i], tool + exts[j]);
      try {
        if (!fs.statSync(candidate).isFile()) continue;
        // A shell would skip a file it cannot execute and keep looking; without
        // this we would return it and fail on spawn with EACCES, which produces
        // no child output and so says nothing about what went wrong.
        if (process.platform !== "win32") fs.accessSync(candidate, fs.constants.X_OK);
        return candidate;
      } catch (e) { /* not on this entry, or not executable */ }
    }
  }
  return "";
}

// run wraps a task main so a TaskError becomes a clean
// "##vso[task.complete result=Failed]" rather than an unhandled rejection, which
// the agent reports as a crash with no usable message.
function run(main) {
  Promise.resolve()
    .then(main)
    .catch(function (err) {
      if (!(err instanceof TaskError)) {
        log(err && err.stack ? err.stack : String(err));
      }
      var message = "tf-snag: " + (err && err.message ? err.message : String(err));
      // Both, and in this order. The agent consumes ##vso[...] lines rather than
      // echoing them, so task.complete alone puts the reason in the task's result
      // - not in the log anyone is actually reading. logissue is what renders it
      // inline as ##[error]. Reporting a failure with no visible cause is worse
      // than the failure.
      logIssue("error", message);
      setResult("Failed", message);
      process.exitCode = 1;
    });
}

module.exports = {
  TaskError: TaskError,
  addAttachment: addAttachment,
  boolInput: boolInput,
  command: command,
  debug: debug,
  describe: describe,
  endGroup: endGroup,
  exec: exec,
  execToFile: execToFile,
  fileInput: fileInput,
  group: group,
  input: input,
  isSet: isSet,
  log: log,
  logIssue: logIssue,
  pickInput: pickInput,
  prependPath: prependPath,
  run: run,
  section: section,
  setOutput: setOutput,
  setResult: setResult,
  setSecret: setSecret,
  uploadArtifact: uploadArtifact,
  uploadSummary: uploadSummary,
  variable: variable,
  which: which,
};
