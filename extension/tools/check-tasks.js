/* Lints the pipeline task manifests against vss-extension.json.
 *
 *   node tools/check-tasks.js
 *
 * None of this is caught by `tfx extension create`, and all of it fails at
 * agent runtime instead - which means a publish, an install and a pipeline run
 * before you find out. Cheapest place to catch it is here, on every PR.
 */
"use strict";

var fs = require("fs");
var path = require("path");

var ROOT = path.join(__dirname, "..");
var TASK_CONTRIBUTION = "ms.vss-distributed-task.task";
var GUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

var problems = [];

function problem(where, message) {
  problems.push(where + ": " + message);
}

function readJSON(file) {
  return JSON.parse(fs.readFileSync(path.join(ROOT, file), "utf8"));
}

var manifest = readJSON("vss-extension.json");

// packagedInto maps a task folder to the extra files the manifest copies into it
// (tasks/common/vso.js -> tasks/tf-snag/vso.js), which is how both tasks share
// one helper without a build step. A require() has to resolve against these too.
var packagedInto = {};
(manifest.files || []).forEach(function (entry) {
  if (!entry.packagePath) return;
  var dir = path.posix.dirname(entry.packagePath);
  (packagedInto[dir] = packagedInto[dir] || []).push(path.posix.basename(entry.packagePath));
  // Only the task sources: this runs without `npm ci`, so the node_modules the
  // tab is packaged with are legitimately absent, and tfx catches those anyway.
  if (dir.indexOf("tasks/") === 0 && !fs.existsSync(path.join(ROOT, entry.path))) {
    problem("vss-extension.json", "files entry " + entry.path + " does not exist");
  }
});

var contributions = (manifest.contributions || []).filter(function (c) { return c.type === TASK_CONTRIBUTION; });
if (!contributions.length) {
  problem("vss-extension.json", "no " + TASK_CONTRIBUTION + " contribution");
}

contributions.forEach(function (contribution) {
  var dir = contribution.properties && contribution.properties.name;
  if (!dir) {
    problem(contribution.id, "contribution has no properties.name naming its task folder");
    return;
  }
  if (!(manifest.files || []).some(function (f) { return f.path === dir && !f.packagePath; })) {
    problem(contribution.id, "task folder " + dir + " is not in the manifest's files, so it will not be packaged");
  }
  checkTask(dir);
});

function checkTask(dir) {
  var file = path.posix.join(dir, "task.json");
  if (!fs.existsSync(path.join(ROOT, file))) {
    problem(dir, "no task.json");
    return;
  }
  var task = readJSON(file);

  if (!GUID.test(task.id || "")) problem(file, "id is not a GUID");
  if (task.name !== path.posix.basename(dir)) {
    problem(file, "name " + JSON.stringify(task.name) + " does not match the folder " + path.posix.basename(dir));
  }
  ["Major", "Minor", "Patch"].forEach(function (part) {
    if (typeof (task.version || {})[part] !== "number") problem(file, "version." + part + " is not a number");
  });

  checkIcon(dir);
  checkInputs(file, task);
  checkBlankFilePaths(file, dir, task);
  checkExecution(file, dir, task);
}

// A filePath input left blank does not reach the task as "": Azure DevOps
// resolves it against the default working directory, so the task is handed the
// repo root. Reading one with plain input() means an unset path silently becomes
// the source directory - which is how `toolPath` once turned into "execute the
// repo root". vso.fileInput() is the guard, so require it.
function checkBlankFilePaths(file, dir, task) {
  var source = fs.readdirSync(path.join(ROOT, dir))
    .filter(function (name) { return /\.js$/.test(name); })
    .map(function (name) { return fs.readFileSync(path.join(ROOT, dir, name), "utf8"); })
    .join("\n");

  (task.inputs || []).forEach(function (input) {
    if (input.type !== "filePath" || input.defaultValue) return;
    if (!new RegExp('fileInput\\(\\s*"' + input.name + '"').test(source)) {
      problem(file, input.name + " is a filePath with no default, so it arrives as the repo root when blank - " +
        "read it with vso.fileInput(), or give it a default");
    }
  });
}

// A task with no icon.png beside its task.json silently falls back to the
// generic document-and-gears icon, which nothing else here would catch.
function checkIcon(dir) {
  var icon = path.join(ROOT, dir, "icon.png");
  if (!fs.existsSync(icon)) {
    problem(dir, "no icon.png - the task will show the default gears icon (run `npm run logo`)");
    return;
  }
  // PNG header: 8-byte signature, then the IHDR length + type, then width and
  // height as big-endian uint32s.
  var head = fs.readFileSync(icon).subarray(0, 24);
  if (head.length < 24 || head.readUInt32BE(12) !== 0x49484452) {
    problem(dir, "icon.png is not a PNG");
    return;
  }
  var width = head.readUInt32BE(16);
  var height = head.readUInt32BE(20);
  if (width !== 32 || height !== 32) {
    problem(dir, "icon.png is " + width + "x" + height + ", but Azure DevOps asks for 32x32");
  }
}

function checkInputs(file, task) {
  var groups = (task.groups || []).map(function (g) { return g.name; });
  var names = {};

  (task.inputs || []).forEach(function (input) {
    if (names[input.name]) problem(file, "duplicate input " + input.name);
    names[input.name] = true;

    if (input.groupName && groups.indexOf(input.groupName) === -1) {
      problem(file, input.name + " is in undeclared group " + input.groupName);
    }
    if (input.type === "pickList") {
      var options = Object.keys(input.options || {});
      if (!options.length) problem(file, input.name + " is a pickList with no options");
      else if (options.indexOf(String(input.defaultValue)) === -1) {
        problem(file, input.name + " defaults to " + JSON.stringify(input.defaultValue) + ", which is not one of its options");
      }
    }
  });

  // A visibleRule naming an input that does not exist hides the control for
  // good, silently.
  (task.inputs || []).forEach(function (input) {
    if (!input.visibleRule) return;
    (input.visibleRule.match(/[A-Za-z_][A-Za-z0-9_]*(?=\s*(?:=|!=|<|>|Contains|StartsWith|EndsWith))/g) || [])
      .forEach(function (referenced) {
        if (!names[referenced]) problem(file, input.name + " has a visibleRule on unknown input " + referenced);
      });
  });
}

function checkExecution(file, dir, task) {
  var handlers = Object.keys(task.execution || {});
  if (!handlers.length) {
    problem(file, "no execution handler");
    return;
  }
  handlers.forEach(function (handler) {
    var target = task.execution[handler].target;
    if (!target) {
      problem(file, handler + " has no target");
      return;
    }
    if (!resolvesInPackage(dir, target)) {
      problem(file, handler + " target " + target + " will not be in the package");
      return;
    }
    checkRequires(dir, target);
  });
}

// checkRequires walks the relative require()s of a task's entry script and any
// module it pulls in, so the packagePath mapping that puts vso.js beside each
// task is verified rather than assumed.
function checkRequires(dir, entry, seen) {
  var visited = seen || {};
  if (visited[entry]) return;
  visited[entry] = true;

  var source = fs.readFileSync(path.join(ROOT, dir, entry), "utf8");
  (source.match(/require\("\.\/[^"]+"\)/g) || []).forEach(function (call) {
    var target = call.slice('require("./'.length, -2);
    var withExt = /\.js$/.test(target) ? target : target + ".js";
    if (!resolvesInPackage(dir, withExt)) {
      problem(path.posix.join(dir, entry), 'require("./' + target + '") will not resolve in the package');
      return;
    }
    if (fs.existsSync(path.join(ROOT, dir, withExt))) checkRequires(dir, withExt, visited);
  });
}

function resolvesInPackage(dir, file) {
  return fs.existsSync(path.join(ROOT, dir, file)) || (packagedInto[dir] || []).indexOf(file) !== -1;
}

if (problems.length) {
  console.error("check-tasks: " + problems.length + " problem(s)");
  problems.forEach(function (p) { console.error("  " + p); });
  process.exit(1);
}
console.log("check-tasks: " + contributions.length + " task(s) OK");
