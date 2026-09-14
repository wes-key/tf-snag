/* Prints the next extension version to publish, from what the Marketplace has.
 *
 *   npx tfx-cli extension show ... --json | node tools/next-version.js --minor 6
 *
 * The Marketplace rejects a version it already has, and an agent only picks up a
 * task whose version went up, so every publish needs a higher 0.<minor>.<patch>.
 * This used to be the publish workflow's run number - which breaks as soon as a
 * second workflow (release.yml) publishes too, since each has its own counter.
 * Asking the Marketplace works for any number of publishers:
 *
 *   newest published 0.<minor>.N   -> 0.<minor>.N+1
 *   nothing published on <minor>   -> 0.<minor>.0
 *
 * The major stays 0 on purpose: pipelines reference the tasks as tf-snag@0, and
 * the task major follows the extension's (tools/stamp-tasks.js).
 *
 * Fails when <minor> is behind a minor already published: that version could
 * never be installed over the one teams have.
 */
"use strict";

var fs = require("fs");

function main(argv) {
  var minor = parseArgs(argv);
  var raw = fs.readFileSync(0, "utf8");
  // tfx can print a banner or an update notice ahead of the JSON.
  var start = raw.indexOf("{");
  if (start < 0) fail("no JSON on stdin (got " + JSON.stringify(raw.slice(0, 200)) + ")");
  var extension;
  try {
    extension = JSON.parse(raw.slice(start));
  } catch (e) {
    fail("could not parse the tfx output: " + e.message);
  }

  var published = (extension.versions || [])
    .map(function (v) { return String(v.version || ""); })
    .filter(function (v) { return /^\d+\.\d+\.\d+$/.test(v); })
    .map(function (v) { return v.split(".").map(Number); });

  var ahead = published.filter(function (v) { return v[0] > 0 || v[1] > minor; });
  if (ahead.length) {
    fail("the Marketplace already has " + ahead.map(function (v) { return v.join("."); }).join(", ") +
      ", ahead of minor " + minor + " - raise the minor in vss-extension.json");
  }

  var patches = published
    .filter(function (v) { return v[1] === minor; })
    .map(function (v) { return v[2]; });
  var next = patches.length ? Math.max.apply(null, patches) + 1 : 0;
  console.log("0." + minor + "." + next);
}

function parseArgs(argv) {
  if (argv.length !== 2 || argv[0] !== "--minor" || !/^\d+$/.test(argv[1])) {
    fail("usage: node tools/next-version.js --minor <n> < tfx-show.json");
  }
  return Number(argv[1]);
}

function fail(message) {
  console.error("next-version: " + message);
  process.exit(1);
}

main(process.argv.slice(2));
