/* Stamps tasks/<task>/task.json before packaging.
 *
 *   node tools/stamp-tasks.js --version 0.6.42 [--channel dev]
 *
 * Two things the .vsix cannot express on its own:
 *
 *   version  Azure DevOps caches a task by id + version, so an agent keeps
 *            running the old copy until the version in task.json goes up. The
 *            extension version is already 0.<minor>.<run_number> (see
 *            .github/workflows/publish-extension.yml); the tasks follow it, so
 *            "which task ran" and "which extension is installed" are one answer.
 *
 *   channel  The dev channel publishes a separate extension id so it can be
 *            installed next to the real one - but task ids are global to an
 *            organisation, so shipping both with the same ids would collide.
 *            --channel dev derives a stable per-channel id from the prod one and
 *            suffixes the task name, leaving the two installable side by side.
 *
 * Run against a checkout that is about to be packaged and thrown away: it
 * rewrites the files in place.
 */
"use strict";

var crypto = require("crypto");
var fs = require("fs");
var path = require("path");

var TASKS_DIR = path.join(__dirname, "..", "tasks");

function main(argv) {
  var args = parseArgs(argv);
  if (!/^\d+\.\d+\.\d+$/.test(args.version)) {
    fail("--version must be X.Y.Z (got " + JSON.stringify(args.version) + ")");
  }
  if (args.channel !== "prod" && args.channel !== "dev") {
    fail("--channel must be prod or dev (got " + JSON.stringify(args.channel) + ")");
  }

  var parts = args.version.split(".").map(Number);
  taskDirs().forEach(function (dir) {
    var file = path.join(dir, "task.json");
    var task = JSON.parse(fs.readFileSync(file, "utf8"));

    task.version = { Major: parts[0], Minor: parts[1], Patch: parts[2] };
    if (args.channel === "dev") {
      task.id = derivedID(task.id);
      task.name = task.name + "-dev";
      task.friendlyName = task.friendlyName + " (dev)";
    }

    fs.writeFileSync(file, JSON.stringify(task, null, 2) + "\n");
    console.log(task.name + "@" + args.version + " (" + task.id + ")");
  });
}

function taskDirs() {
  return fs.readdirSync(TASKS_DIR)
    .map(function (name) { return path.join(TASKS_DIR, name); })
    .filter(function (dir) { return fs.existsSync(path.join(dir, "task.json")); });
}

// derivedID hashes the production id into another well-formed GUID, so the dev
// channel's ids are stable across runs without a second set to keep in the
// source - and an agent can never confuse the two.
function derivedID(id) {
  var hash = crypto.createHash("sha1").update(id + ":dev").digest("hex");
  return [
    hash.slice(0, 8),
    hash.slice(8, 12),
    "5" + hash.slice(13, 16),
    ((parseInt(hash.slice(16, 17), 16) & 0x3) | 0x8).toString(16) + hash.slice(17, 20),
    hash.slice(20, 32),
  ].join("-");
}

function parseArgs(argv) {
  var args = { version: "", channel: "prod" };
  for (var i = 0; i < argv.length; i++) {
    if (argv[i] === "--version") args.version = argv[++i] || "";
    else if (argv[i] === "--channel") args.channel = argv[++i] || "";
    else fail("unknown argument " + JSON.stringify(argv[i]));
  }
  if (!args.version) fail("--version is required");
  return args;
}

function fail(message) {
  console.error("stamp-tasks: " + message);
  process.exit(1);
}

main(process.argv.slice(2));
