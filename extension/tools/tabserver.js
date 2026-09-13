/* Static server for the tab dev harness.
 *
 *   npm run tab:dev        ->  http://127.0.0.1:8730/  (PORT=... to change)
 *
 * Serves extension/ so dev/index.html can reach ../tab/drift.js and
 * ../tab/drift.css as it would in the package. A server rather than opening the
 * file directly because the harness fetches its sample report, which a browser
 * refuses to do from file://.
 *
 * Dependency-free and local-only, in keeping with the rest of extension/.
 */
"use strict";

var fs = require("fs");
var http = require("http");
var path = require("path");

var ROOT = path.join(__dirname, "..");
var PORT = Number(process.env.PORT) || 8730;
var HOST = "127.0.0.1";

var TYPES = {
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".json": "application/json; charset=utf-8",
  ".png": "image/png",
  ".svg": "image/svg+xml",
};

http.createServer(function (req, res) {
  var rel = decodeURIComponent(req.url.split("?")[0]);

  // Redirect rather than serving the harness at "/": the page fetches its
  // sample report with a relative URL, which would otherwise resolve against
  // the server root instead of dev/.
  if (rel === "/" || rel === "/dev" || rel === "/dev/") {
    res.writeHead(302, { Location: "/dev/index.html" }).end();
    return;
  }

  // Resolve, then confirm the result is still inside ROOT: "/../.." in a URL is
  // the whole reason a five-line file server is not a five-line file server.
  var file = path.resolve(ROOT, "." + rel);
  if (file !== ROOT && !file.startsWith(ROOT + path.sep)) {
    res.writeHead(403).end("forbidden");
    return;
  }

  fs.readFile(file, function (err, body) {
    if (err) {
      res.writeHead(err.code === "ENOENT" ? 404 : 500, { "Content-Type": "text/plain" });
      res.end(err.code === "ENOENT" ? "not found: " + rel : String(err.message));
      return;
    }
    res.writeHead(200, {
      "Content-Type": TYPES[path.extname(file).toLowerCase()] || "application/octet-stream",
      // The point of the harness is editing drift.js and hitting reload.
      "Cache-Control": "no-store",
    });
    res.end(body);
  });
}).listen(PORT, HOST, function () {
  console.log("tf-snag tab harness: http://" + HOST + ":" + PORT + "/");
  console.log("serving " + ROOT + " - Ctrl+C to stop");
});
