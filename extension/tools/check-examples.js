/* Lints the pipeline examples in examples/ against the task manifests.
 *
 *   node tools/check-examples.js
 *
 * An example is documentation somebody copies verbatim, so a stale input is
 * worse than a stale sentence: they find out when a pipeline fails on an agent.
 * Renaming a task input without updating the examples should fail here, on the
 * PR that renames it.
 *
 * This deliberately does not parse YAML. Azure Pipelines YAML carries
 * `${{ }}` template expressions that a strict parser rejects, and the question
 * here is narrow: which tasks do the examples call, and which inputs do they
 * pass. A regex over `- task:` blocks answers exactly that and nothing else.
 */
"use strict";

var fs = require("fs");
var path = require("path");

var ROOT = path.join(__dirname, "..", "..");
var EXAMPLES = path.join(ROOT, "examples");
var TASKS = path.join(__dirname, "..", "tasks");

var problems = [];

function problem(where, message) {
  problems.push(where + ": " + message);
}

// tasks maps "tf-snag" -> the set of input names its manifest declares.
function loadTasks() {
  var out = {};
  fs.readdirSync(TASKS).forEach(function (dir) {
    var file = path.join(TASKS, dir, "task.json");
    if (!fs.existsSync(file)) return;
    var task = JSON.parse(fs.readFileSync(file, "utf8"));
    out[task.name] = {
      major: task.version.Major,
      inputs: (task.inputs || []).map(function (i) { return i.name; }),
    };
  });
  return out;
}

function yamlFiles(dir) {
  return fs.readdirSync(dir, { withFileTypes: true }).reduce(function (acc, entry) {
    var full = path.join(dir, entry.name);
    if (entry.isDirectory()) return acc.concat(yamlFiles(full));
    return /\.ya?ml$/.test(entry.name) ? acc.concat(full) : acc;
  }, []);
}

// steps pulls every `- task: name@major` out of a file, with the input keys
// indented under its `inputs:` block.
function steps(text) {
  var lines = text.split(/\r?\n/);
  var found = [];
  for (var i = 0; i < lines.length; i++) {
    var m = lines[i].match(/^(\s*)-\s*task:\s*([A-Za-z0-9_.-]+)@(\d+)/);
    if (!m) continue;
    var indent = m[1].length;
    var step = { name: m[2], major: Number(m[3]), line: i + 1, inputs: [] };

    var inInputs = false;
    for (var j = i + 1; j < lines.length; j++) {
      var line = lines[j];
      if (!line.trim() || /^\s*#/.test(line)) continue;
      var lead = line.search(/\S/);
      if (lead <= indent) break;               // next step, or out of the block
      if (/^\s*inputs:\s*$/.test(line)) {
        inInputs = true;
        continue;
      }
      if (!inInputs) continue;
      var input = line.match(/^\s*([A-Za-z][A-Za-z0-9_]*)\s*:/);
      if (input) step.inputs.push({ name: input[1], line: j + 1 });
    }
    found.push(step);
  }
  return found;
}

function main() {
  if (!fs.existsSync(EXAMPLES)) {
    console.error("check-examples: no examples/ directory");
    process.exit(1);
  }
  var tasks = loadTasks();
  var files = yamlFiles(EXAMPLES);
  if (!files.length) problem("examples", "no pipeline files found");

  var checked = 0;
  files.forEach(function (file) {
    var rel = path.relative(ROOT, file).replace(/\\/g, "/");
    steps(fs.readFileSync(file, "utf8")).forEach(function (step) {
      var task = tasks[step.name];
      if (!task) return;                        // TerraformInstaller and friends
      checked++;
      if (step.major !== task.major) {
        problem(rel + ":" + step.line,
          step.name + "@" + step.major + " but the task ships major " + task.major);
      }
      step.inputs.forEach(function (input) {
        if (task.inputs.indexOf(input.name) === -1) {
          problem(rel + ":" + input.line,
            step.name + " has no input " + JSON.stringify(input.name));
        }
      });
    });
  });

  if (problems.length) {
    console.error("check-examples: " + problems.length + " problem(s)");
    problems.forEach(function (p) { console.error("  " + p); });
    process.exit(1);
  }
  console.log("check-examples: " + checked + " tf-snag step(s) in " + files.length + " file(s) OK");
}

main();
