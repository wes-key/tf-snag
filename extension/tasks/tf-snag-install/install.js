/* tf-snag-install task.
 *
 * Downloads the tf-snag CLI from a GitHub release of wes-key/tf-snag and puts
 * it on PATH for the rest of the job, replacing the `gh release download` script
 * step every drift pipeline used to carry.
 *
 * The binary is cached under the agent tool directory (the same layout the
 * Microsoft tool installers use, <tool>/<version>/<arch> plus an <arch>.complete
 * marker), so a self-hosted agent downloads a given release once.
 *
 * CI publishes linux/amd64 (asset "tf-snag") and windows/amd64 ("tf-snag.exe")
 * only - see .github/workflows/ci.yml. Anything else fails here, with the reason,
 * rather than as "cannot execute binary file" two steps later.
 */
"use strict";

var fs = require("fs");
var http = require("http");
var https = require("https");
var os = require("os");
var path = require("path");
var url = require("url");

var vso = require("./vso");

var API = "https://api.github.com";
var USER_AGENT = "tf-snag-install-task";

vso.run(main);

function main() {
  var repository = vso.input("repository") || "wes-key/tf-snag";
  var version = vso.input("version") || "latest";
  var includePrerelease = vso.boolInput("includePrerelease", false);
  var token = vso.input("githubToken");
  // A PAT reaching us from a plain variable is not masked by the agent. Ask for
  // masking before it can appear in an error body we echo.
  vso.setSecret(token);

  var target = assetName();

  return resolveRelease(repository, version, includePrerelease, token).then(function (release) {
    vso.log("tf-snag release: " + release.tag + " (" + repository + ")");

    var cached = toolCacheDir(release.tag);
    var binary = path.join(cached.dir, target.asset);
    if (fs.existsSync(cached.marker) && fs.existsSync(binary)) {
      vso.log("using the cached copy at " + binary);
      return finish(cached.dir, binary, release.tag);
    }

    var asset = (release.assets || []).filter(function (a) { return a.name === target.asset; })[0];
    if (!asset) {
      throw new vso.TaskError(
        "release " + release.tag + " has no asset named " + target.asset +
        " (it publishes: " + ((release.assets || []).map(function (a) { return a.name; }).join(", ") || "nothing") + ")"
      );
    }

    fs.mkdirSync(cached.dir, { recursive: true });
    vso.log("downloading " + target.asset + " -> " + binary);
    return download(asset.url, binary, token).then(function () {
      fs.chmodSync(binary, 0o755);
      fs.writeFileSync(cached.marker, release.tag + "\n");
      return finish(cached.dir, binary, release.tag);
    });
  });
}

function finish(dir, binary, tag) {
  // Prepending puts it ahead of any tf-snag already on the image, so a pinned
  // version in the YAML is the one that runs.
  vso.prependPath(dir);
  vso.setOutput("tfSnagPath", binary);
  vso.setOutput("tfSnagVersion", tag);
  var code = vso.exec(binary, ["-version"]);
  if (code !== 0) {
    throw new vso.TaskError("`tf-snag -version` exited " + code + " - the download looks unusable");
  }
}

// --- platform -------------------------------------------------------------

function assetName() {
  if (process.arch !== "x64") {
    throw new vso.TaskError(
      "tf-snag releases carry linux/amd64 and windows/amd64 only; this agent is " +
      process.platform + "/" + process.arch
    );
  }
  switch (process.platform) {
    case "linux":
      return { asset: "tf-snag" };
    case "win32":
      return { asset: "tf-snag.exe" };
    default:
      throw new vso.TaskError(
        "tf-snag releases carry linux and windows binaries only; this agent is " + process.platform
      );
  }
}

function toolCacheDir(tag) {
  // AGENT_TOOLSDIRECTORY is the cache Microsoft's tool installers share and is
  // preserved between runs on a self-hosted agent. Hosted agents have it too;
  // it just starts empty every time.
  var root = vso.variable("Agent.ToolsDirectory") ||
    path.join(vso.variable("Agent.WorkFolder") || os.tmpdir(), "_tool");
  var base = path.join(root, "tf-snag", tag.replace(/^v/, ""));
  return { dir: path.join(base, process.arch), marker: path.join(base, process.arch + ".complete") };
}

// --- GitHub ---------------------------------------------------------------

// resolveRelease turns the `version` input into a release: a tag as given (with
// a "v" prefix retried, since the tags are vX.Y.Z), or the newest published one.
function resolveRelease(repository, version, includePrerelease, token) {
  if (version !== "latest") {
    return getRelease(repository, version, token).catch(function (err) {
      if (err.status !== 404 || version.indexOf("v") === 0) throw err;
      return getRelease(repository, "v" + version, token);
    });
  }
  if (!includePrerelease) {
    // /releases/latest is 404 when every release so far is a pre-release, which
    // reads as an auth failure unless we say otherwise.
    return apiJSON(API + "/repos/" + repository + "/releases/latest", token).catch(function (err) {
      if (err.status !== 404) throw err;
      throw new vso.TaskError(
        "no stable release published on " + repository + " - tick \"Include pre-releases\" to take the newest " +
        "-dev / -rc tag, or pin a version"
      );
    }).then(toRelease);
  }
  // /releases/latest skips pre-releases, and the dev/rc tags this repo pushes
  // are all pre-releases. The list endpoint is newest-first.
  return apiJSON(API + "/repos/" + repository + "/releases?per_page=1", token).then(function (releases) {
    if (!releases || !releases.length) {
      throw new vso.TaskError("no releases published on " + repository);
    }
    return toRelease(releases[0]);
  });
}

function getRelease(repository, tag, token) {
  return apiJSON(API + "/repos/" + repository + "/releases/tags/" + encodeURIComponent(tag), token).then(toRelease);
}

function toRelease(body) {
  return {
    tag: body.tag_name,
    assets: (body.assets || []).map(function (a) { return { name: a.name, url: a.url }; }),
  };
}

function apiJSON(target, token) {
  return request(target, headers(token, "application/vnd.github+json")).then(function (res) {
    return collect(res).then(function (body) {
      if (res.statusCode === 404) {
        throw fail(404,
          "GitHub returned 404 for " + target + ". Check the version exists - and if the repository is " +
          "private, set the githubToken input to a token with read-only Contents on it."
        );
      }
      // Anonymous calls get 60 an hour per IP, and Microsoft-hosted agents share
      // their IPs - so without a token, a 403 is the rate limit, not a permission.
      if (res.statusCode === 403 && !token) {
        throw fail(403,
          "GitHub returned 403 for " + target + " - most likely the anonymous rate limit (60 calls an hour, " +
          "shared by every job on this agent's IP address). Set the githubToken input to any GitHub token to lift it."
        );
      }
      if (res.statusCode === 401 || res.statusCode === 403) {
        throw fail(res.statusCode,
          "GitHub returned " + res.statusCode + " for " + target + " - the githubToken is missing, expired or " +
          "lacks read Contents on that repository. " + rateLimitNote(res)
        );
      }
      if (res.statusCode < 200 || res.statusCode >= 300) {
        throw fail(res.statusCode, "GitHub returned " + res.statusCode + " for " + target + ": " + body.slice(0, 500));
      }
      try {
        return JSON.parse(body);
      } catch (e) {
        throw new vso.TaskError("could not parse the GitHub response for " + target + ": " + e.message);
      }
    });
  });
}

// download streams a release asset to disk. Assets are fetched by their API url
// with an octet-stream Accept, which works anonymously for a public repository
// and, with a token, for a private fork's assets too.
function download(assetURL, dest, token) {
  return request(assetURL, headers(token, "application/octet-stream")).then(function (res) {
    if (res.statusCode < 200 || res.statusCode >= 300) {
      return collect(res).then(function (body) {
        throw fail(res.statusCode, "downloading the asset returned " + res.statusCode + ": " + body.slice(0, 500));
      });
    }
    return new Promise(function (resolve, reject) {
      var out = fs.createWriteStream(dest);
      res.pipe(out);
      res.on("error", reject);
      out.on("error", reject);
      out.on("finish", resolve);
    });
  });
}

function headers(token, accept) {
  var h = { "User-Agent": USER_AGENT, Accept: accept, "X-GitHub-Api-Version": "2022-11-28" };
  if (token) h.Authorization = "Bearer " + token;
  return h;
}

function rateLimitNote(res) {
  return res.headers["x-ratelimit-remaining"] === "0" ? "(the GitHub rate limit is also exhausted)" : "";
}

function fail(status, message) {
  var err = new vso.TaskError(message);
  err.status = status;
  return err;
}

function collect(res) {
  return new Promise(function (resolve, reject) {
    var chunks = [];
    res.on("data", function (c) { chunks.push(c); });
    res.on("end", function () { resolve(Buffer.concat(chunks).toString("utf8")); });
    res.on("error", reject);
  });
}

// --- HTTP -----------------------------------------------------------------

// request GETs a URL and follows redirects, dropping the Authorization header
// when the redirect crosses to another host: release assets redirect to blob
// storage, which rejects a request carrying somebody else's credentials.
function request(target, hdrs, redirects) {
  var left = redirects === undefined ? 5 : redirects;
  var parsed = url.parse(target);
  return connect(parsed).then(function (options) {
    return new Promise(function (resolve, reject) {
      var req = https.request(Object.assign({}, options, {
        method: "GET",
        headers: Object.assign({ Host: parsed.hostname }, hdrs),
      }), resolve);
      req.on("error", reject);
      req.end();
    });
  }).then(function (res) {
    var location = res.headers.location;
    if (res.statusCode >= 300 && res.statusCode < 400 && location) {
      if (left <= 0) throw new vso.TaskError("too many redirects fetching " + target);
      res.resume();
      var next = url.resolve(target, location);
      var forwarded = Object.assign({}, hdrs);
      if (url.parse(next).hostname !== parsed.hostname) delete forwarded.Authorization;
      return request(next, forwarded, left - 1);
    }
    return res;
  });
}

// connect returns the https.request options for a host, tunnelling through the
// agent proxy when the job has one configured. Self-hosted agents behind a
// corporate proxy are the whole reason this exists; hosted agents set nothing
// and take the direct path.
function connect(parsed) {
  var proxy = vso.variable("Agent.ProxyUrl");
  var port = parsed.port || 443;
  if (!proxy || bypassesProxy(parsed.hostname)) {
    return Promise.resolve({ host: parsed.hostname, port: port, path: parsed.path, servername: parsed.hostname });
  }
  var p = url.parse(proxy);
  var proxyHeaders = { Host: parsed.hostname + ":" + port };
  var user = vso.variable("Agent.ProxyUsername");
  var pass = vso.variable("Agent.ProxyPassword");
  if (user) {
    proxyHeaders["Proxy-Authorization"] =
      "Basic " + Buffer.from(user + ":" + pass).toString("base64");
  }
  return new Promise(function (resolve, reject) {
    var req = http.request({
      host: p.hostname,
      port: p.port || (p.protocol === "https:" ? 443 : 80),
      method: "CONNECT",
      path: parsed.hostname + ":" + port,
      headers: proxyHeaders,
    });
    req.on("connect", function (res, socket) {
      if (res.statusCode !== 200) {
        reject(new vso.TaskError("the agent proxy refused a tunnel to " + parsed.hostname + ": " + res.statusCode));
        return;
      }
      // agent:false stops Node opening a second, un-tunnelled connection.
      resolve({ socket: socket, agent: false, path: parsed.path, servername: parsed.hostname });
    });
    req.on("error", function (err) {
      reject(new vso.TaskError("could not reach the agent proxy " + p.hostname + ": " + err.message));
    });
    req.end();
  });
}

function bypassesProxy(hostname) {
  var raw = vso.variable("Agent.ProxyBypassList");
  if (!raw) return false;
  var list;
  try {
    list = JSON.parse(raw);
  } catch (e) {
    list = raw.split(",");
  }
  return (list || []).some(function (pattern) {
    try {
      return new RegExp(String(pattern).trim(), "i").test(hostname);
    } catch (e) {
      return false;
    }
  });
}
