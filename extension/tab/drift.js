/* tf-snag build-results tab.
 *
 * Reads the JSON report the drift pipeline publishes with
 *   ##vso[task.addattachment type=tf-snag.report;name=tf-snag;]<file>
 * and renders it. Report shape is owned by internal/report/report.go
 * (Report.Schema == SCHEMA_SUPPORTED here). Schema 2 adds deprecations,
 * ignore-rule suppression, and baseline provenance (first_seen / new).
 */
(function () {
  "use strict";

  var ATTACHMENT_TYPE = "tf-snag.report";
  var SCHEMA_SUPPORTED = 2;

  var SIGN = { create: "+", update: "~", delete: "−", replace: "±", read: " ", "no-op": " " };

  // usePlatformScripts: true makes the host inject its module loader so
  // VSS.require() below can resolve platform modules (TFS/Build/RestClient).
  // Without it VSS.require throws "window.require is not a function" and the
  // tab spins forever (notifyLoadSucceeded is never reached).
  VSS.init({ explicitNotifyLoaded: true, usePlatformScripts: true, usePlatformStyles: true, applyTheme: true });

  VSS.ready(function () {
    VSS.require(["VSS/Service", "TFS/Build/RestClient"], function (VSS_Service, BuildRestClient) {
      var cfg = VSS.getConfiguration();
      var client = VSS_Service.getCollectionClient(BuildRestClient.BuildHttpClient);
      var renderToken = 0;

      function handleBuild(build) {
        if (!build || !build.id) return;
        var mine = ++renderToken;
        loadReport(client, build).then(function (result) {
          if (mine !== renderToken) return; // superseded by a newer build event
          render(result, build);
          done();
        }, function (err) {
          if (mine !== renderToken) return;
          renderError(messageOf(err));
          done();
        });
      }

      if (cfg && typeof cfg.onBuildChanged === "function") {
        cfg.onBuildChanged(handleBuild);
      } else {
        renderError("This tab must be opened from a pipeline run.");
        done();
      }
    });
  });

  function done() {
    try { VSS.notifyLoadSucceeded(); } catch (e) { /* older host */ }
    resize();
  }

  function resize() {
    try { VSS.resize(); } catch (e) { /* no-op */ }
  }
  window.addEventListener("resize", resize);

  // --- data -----------------------------------------------------------------

  function loadReport(client, build) {
    var project = VSS.getWebContext().project.id;
    return client.getAttachments(project, build.id, ATTACHMENT_TYPE).then(function (attachments) {
      if (!attachments || !attachments.length) return { report: null };
      var att = attachments[0];
      var href = (att._links && att._links.self && att._links.self.href) || "";
      var ids = parseAttachmentHref(href);
      if (!ids) return Promise.reject(new Error("Could not locate the drift attachment for this run."));
      return client
        .getAttachment(project, build.id, ids.timelineId, ids.recordId, ATTACHMENT_TYPE, att.name)
        .then(function (buf) {
          var text = bufToString(buf);
          var report;
          try {
            report = JSON.parse(text);
          } catch (e) {
            return Promise.reject(new Error("Drift attachment is not valid JSON: " + e.message));
          }
          return { report: report };
        });
    });
  }

  // href: .../_apis/build/builds/{buildId}/{timelineId}/{recordId}/attachments/{type}/{name}
  function parseAttachmentHref(href) {
    if (!href) return null;
    var parts = href.split("?")[0].split("/");
    var i = parts.lastIndexOf("attachments");
    if (i < 3) return null;
    return { timelineId: parts[i - 2], recordId: parts[i - 1] };
  }

  function bufToString(buf) {
    if (typeof buf === "string") return buf;
    var bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
    if (typeof TextDecoder !== "undefined") {
      try { return new TextDecoder("utf-8").decode(bytes); } catch (e) { /* fall through */ }
    }
    var s = "";
    for (var j = 0; j < bytes.length; j++) s += String.fromCharCode(bytes[j]);
    try { return decodeURIComponent(escape(s)); } catch (e2) { return s; }
  }

  function messageOf(err) {
    if (!err) return "Unknown error.";
    if (err.serverError && err.serverError.message) return err.serverError.message;
    return err.message || String(err);
  }

  // --- rendering ----------------------------------------------------------

  function root() { return document.getElementById("root"); }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  function el(tag, attrs, kids) {
    var n = document.createElement(tag);
    if (attrs) Object.keys(attrs).forEach(function (k) {
      if (k === "class") n.className = attrs[k];
      else if (k === "text") n.textContent = attrs[k];
      else if (k === "onclick") n.addEventListener("click", attrs[k]);
      else n.setAttribute(k, attrs[k]);
    });
    (kids || []).forEach(function (c) { if (c != null) n.appendChild(typeof c === "string" ? document.createTextNode(c) : c); });
    return n;
  }

  function renderError(msg) {
    var r = root();
    clear(r);
    r.appendChild(el("div", { class: "tfd-banner tfd-banner-error" }, [
      el("strong", { text: "Could not load the tf-snag report." }),
      el("div", { class: "tfd-muted", text: msg })
    ]));
  }

  function render(result, build) {
    var r = root();
    clear(r);

    if (!result.report) {
      r.appendChild(el("div", { class: "tfd-banner" }, [
        el("strong", { text: "No tf-snag report for this run." }),
        el("div", {
          class: "tfd-muted",
          text: "The run did not publish a '" + ATTACHMENT_TYPE + "' attachment. " +
                "Check the 'Terraform init, plan and drift check' step."
        })
      ]));
      return;
    }

    var report = result.report;
    var pending = report.pending || [];

    if (typeof report.schema === "number" && report.schema > SCHEMA_SUPPORTED) {
      r.appendChild(el("div", { class: "tfd-banner tfd-banner-warn" }, [
        el("strong", { text: "Newer report format (schema " + report.schema + ")." }),
        el("div", { class: "tfd-muted", text: "Update the tf-snag extension. Showing a best effort below." })
      ]));
    }

    var drift = report.drift || [];
    var deps = report.deprecations || [];
    var activeDrift = drift.filter(notSuppressed);
    var activeDeps = deps.filter(notSuppressed);
    var ignoredDrift = drift.filter(isSuppressed);
    var ignoredDeps = deps.filter(isSuppressed);
    var linker = fileLinker(build);

    var gating = activeDrift.length + activeDeps.length;

    // status banner
    r.appendChild(el("div", { class: "tfd-banner " + (gating ? "tfd-banner-warn" : "tfd-banner-ok") }, [
      el("strong", { text: gating ? bannerHead(activeDrift.length, activeDeps.length) : "No drift or deprecations" }),
      el("div", { class: "tfd-muted", text: metaLine(report, pending, ignoredDrift.length + ignoredDeps.length, build) })
    ]));

    // One pivot tab per finding kind, each owning its own "ignored" group.
    var driftPanel = el("div", null, [
      section("Changed outside Terraform", activeDrift, {
        headers: ["Resource", "Module", "Change"],
        emptyText: "Nothing has changed outside Terraform for this run.",
        row: driftRowFn(linker)
      })
    ]);
    if (ignoredDrift.length) {
      driftPanel.appendChild(collapsedSection("Ignored drift", ignoredDrift, {
        headers: ["Resource", "Module", "Change", "Reason"],
        row: ignoredDriftRowFn(linker)
      }));
    }

    var deprPanel = el("div", null, [
      section("Deprecations", activeDeps, {
        headers: ["Deprecation", "Resource", "Severity"],
        emptyText: "No deprecation warnings in the plan.",
        row: deprRowFn(linker)
      })
    ]);
    if (ignoredDeps.length) {
      deprPanel.appendChild(collapsedSection("Ignored deprecations", ignoredDeps, {
        headers: ["Deprecation", "Resource", "Reason"],
        row: ignoredDeprRowFn(linker)
      }));
    }

    var pendingPanel = el("div", null, [
      section("Pending changes from configuration", pending, {
        headers: ["Resource", "Module", "Change"],
        emptyText: "No pending changes — configuration matches state.",
        infoHead: "Unapplied config changes",
        infoText: "Updates to the Terraform configuration that have not been applied yet — " +
          "a terraform apply would enact them. Shown for context: this is not drift " +
          "(a change made outside Terraform), and the drift check does not gate on it.",
        row: driftRowFn(null)
      })
    ]);

    r.appendChild(tabView([
      { label: "Drift", count: activeDrift.length, body: driftPanel },
      { label: "Deprecations", count: activeDeps.length, body: deprPanel },
      { label: "Pending changes", count: pending.length, body: pendingPanel }
    ]));

    resize();
  }

  // --- tab strip (pivot) ------------------------------------------

  // tabView renders an Azure DevOps-style pivot: a tablist of buttons over one
  // panel each. Every panel is built up front (a report is small) and hidden
  // rather than re-rendered, so expanded rows survive tab switches.
  // tabs: [{ label, count, body:Node }]. Opens on the first tab that has
  // findings, so a run whose only findings are deprecations doesn't land on an
  // empty Drift tab.
  function tabView(tabs) {
    var strip = el("div", { class: "tfd-tabs", role: "tablist" });
    var panels = el("div", { class: "tfd-panels" });
    var btns = [], pans = [];

    function select(i) {
      btns.forEach(function (b, j) {
        var on = i === j;
        b.className = "tfd-tab" + (on ? " tfd-tab-active" : "");
        b.setAttribute("aria-selected", on ? "true" : "false");
        b.setAttribute("tabindex", on ? "0" : "-1");
        pans[j].className = "tfd-panel" + (on ? "" : " tfd-hidden");
      });
      resize();
    }

    tabs.forEach(function (t, i) {
      var id = "tfd-tab-" + i;
      var btn = el("button", { class: "tfd-tab", type: "button", role: "tab", id: id }, [
        t.label, el("span", { class: "tfd-count", text: String(t.count) })
      ]);
      btn.addEventListener("click", function () { select(i); });
      btn.addEventListener("keydown", function (e) {
        var n = null;
        if (e.key === "ArrowRight") n = (i + 1) % tabs.length;
        else if (e.key === "ArrowLeft") n = (i - 1 + tabs.length) % tabs.length;
        else if (e.key === "Home") n = 0;
        else if (e.key === "End") n = tabs.length - 1;
        if (n === null) return;
        e.preventDefault();
        select(n);
        btns[n].focus();
      });
      btns.push(btn);
      strip.appendChild(btn);

      pans.push(el("div", { class: "tfd-panel", role: "tabpanel", "aria-labelledby": id }, [t.body]));
      panels.appendChild(pans[i]);
    });

    var initial = 0;
    for (var k = 0; k < tabs.length; k++) {
      if (tabs[k].count) { initial = k; break; }
    }
    select(initial);

    return el("div", { class: "tfd-tabview" }, [strip, panels]);
  }

  function isSuppressed(x) { return !!x.suppressed; }
  function notSuppressed(x) { return !x.suppressed; }

  function bannerHead(nDrift, nDeps) {
    var bits = [];
    if (nDrift) bits.push(nDrift + " resource" + (nDrift === 1 ? "" : "s") + " changed outside Terraform");
    if (nDeps) bits.push(nDeps + " deprecation" + (nDeps === 1 ? "" : "s"));
    return bits.join(", ");
  }

  function metaLine(report, pending, nIgnored, build) {
    var bits = [];
    if (report.terraform_version) bits.push("Terraform " + report.terraform_version);
    var t = tally(pending);
    bits.push(t.add + " to add, " + t.change + " to change, " + t.destroy + " to destroy");
    if (nIgnored) bits.push(nIgnored + " ignored");
    if (build && build.buildNumber) bits.push("run " + build.buildNumber);
    return bits.join("  ·  ");
  }

  function tally(rows) {
    var t = { add: 0, change: 0, destroy: 0 };
    (rows || []).forEach(function (r) {
      if (r.action === "create") t.add++;
      else if (r.action === "update") t.change++;
      else if (r.action === "delete") t.destroy++;
      else if (r.action === "replace") { t.add++; t.destroy++; }
    });
    return t;
  }

  // --- provenance (baseline) --------------------------------------------

  // provCell renders the "First seen" cell: a "new" pill for findings absent
  // from the previous run, otherwise the first-detection date + age. Empty when
  // the run had no -baseline (baseline_state / first_seen unset).
  function provCell(row) {
    // A finding whose ignore rule was just removed is newly *actionable*, not
    // newly detected — so it gets its own badge and keeps its real age beside
    // it. Calling it "new" would misreport how long the drift has been there.
    if (row.unsuppressed) {
      var t = ageText(row.first_seen);
      var cell = el("span", null, [badge("no longer ignored", "new")]);
      if (t) cell.appendChild(el("div", { class: "tfd-muted tfd-prov-age", text: t }));
      return cell;
    }
    if (row.baseline_state === "new") return badge("new", "new");
    var age = ageText(row.first_seen);
    return el("span", { class: "tfd-muted", text: age || "—" });
  }

  function ageText(firstSeen) {
    if (!firstSeen) return "";
    var d = new Date(firstSeen);
    if (isNaN(d.getTime())) return "";
    var date = d.toISOString().slice(0, 10);
    var days = Math.floor((Date.now() - d.getTime()) / 86400000);
    if (days <= 0) return date + " · today";
    return date + " · " + days + "d ago";
  }

  // --- source links -------------------------------------------------

  // fileLinker returns fn(file, line) -> URL into the run's repo, or null when
  // the repo type isn't linkable. Azure Repos Git and GitHub are handled.
  function fileLinker(build) {
    var repo = build && build.repository;
    if (!repo) return null;
    var type = (repo.type || "").toLowerCase();
    var sha = build.sourceVersion || "";

    if (type === "tfsgit" || type === "git") {
      var wc = VSS.getWebContext();
      var host = (wc.collection && wc.collection.uri) || (wc.host && wc.host.uri) || "";
      var proj = wc.project && wc.project.name;
      if (!host || !proj || !repo.name) return null;
      if (host.charAt(host.length - 1) !== "/") host += "/";
      return function (file, line) {
        var u = host + encodeURIComponent(proj) + "/_git/" + encodeURIComponent(repo.name) +
          "?path=" + encodeURIComponent("/" + file);
        if (sha) u += "&version=GC" + sha;
        if (line) u += "&line=" + line + "&lineEnd=" + (line + 1) +
          "&lineStartColumn=1&lineEndColumn=1&lineStyle=plain&_a=contents";
        return u;
      };
    }
    if (type === "github" || type === "githubenterprise") {
      var base = (repo.url || "").replace(/\.git$/, "");
      if (!/^https?:\/\//.test(base)) return null;
      var ref = sha || "HEAD";
      return function (file, line) {
        return base + "/blob/" + ref + "/" +
          file.split("/").map(encodeURIComponent).join("/") + (line ? "#L" + line : "");
      };
    }
    return null;
  }

  // locNode renders "path:line" as a repo link when possible, muted text if not.
  function locNode(linker, file, line) {
    if (!file) return null;
    var label = file + (line ? ":" + line : "");
    var href = linker && linker(file, line);
    if (!href) return el("span", { class: "tfd-loc tfd-muted", text: label });
    return el("a", { class: "tfd-loc", href: href, target: "_blank", rel: "noopener noreferrer", text: label });
  }

  // --- shared finding table --------------------------------------

  // section = a heading (+ count), optional info banner, empty-state line, and a
  // findingTable of the items.
  function section(title, items, opts) {
    var wrap = el("section", { class: "tfd-section" }, [
      el("h3", { class: "tfd-h3" }, [title, el("span", { class: "tfd-count", text: String(items.length) })])
    ]);
    if (opts.infoText) {
      wrap.appendChild(el("div", { class: "tfd-banner tfd-banner-info" }, [
        el("strong", { text: opts.infoHead || "For context" }),
        el("div", { class: "tfd-muted", text: opts.infoText })
      ]));
    }
    if (!items.length) {
      wrap.appendChild(el("p", { class: "tfd-muted tfd-empty", text: opts.emptyText }));
      return wrap;
    }
    wrap.appendChild(findingTable(items, opts));
    return wrap;
  }

  // collapsedSection = a section whose findingTable is hidden until the heading
  // is clicked. Used for the ignored-* groups.
  function collapsedSection(title, items, opts) {
    var wrap = el("section", { class: "tfd-section" });
    var body = el("div", { class: "tfd-hidden" }, [findingTable(items, opts)]);
    var caret = el("span", { class: "tfd-caret", text: "▸" });
    var h3 = el("h3", { class: "tfd-h3 tfd-h3-toggle" }, [
      caret, title, el("span", { class: "tfd-count", text: String(items.length) })
    ]);
    h3.addEventListener("click", function () {
      var hidden = body.classList.toggle("tfd-hidden");
      caret.textContent = hidden ? "▸" : "▾";
      resize();
    });
    wrap.appendChild(h3);
    wrap.appendChild(body);
    return wrap;
  }

  // findingTable renders items as one table: a sign column, opts.headers columns
  // (headers[0] labels the primary/expand column; the rest label opts.row's
  // cells[]), an optional "First seen" column, and an expandable detail row.
  // opts.row(item) -> { sign:{glyph,cls}, primary:Node, cells:[Node|string,...],
  // detail:Node|null }. All rows start collapsed.
  function findingTable(items, opts) {
    var showProv = items.some(function (x) { return x.baseline_state || x.first_seen; });
    var head = [el("th", { class: "tfd-col-sign", text: "" })];
    opts.headers.forEach(function (h) { head.push(el("th", { text: h })); });
    if (showProv) head.push(el("th", { text: "First seen" }));
    var cols = head.length;

    var table = el("table", { class: "tfd-table" }, [el("thead", null, [el("tr", null, head)])]);
    var tbody = el("tbody");

    items.forEach(function (item) {
      var d = opts.row(item);
      var expandable = !!d.detail;

      var tr = el("tr", { class: "tfd-row" + (expandable ? " tfd-row-x" : "") });
      tr.appendChild(el("td", { class: "tfd-col-sign " + (d.sign.cls || ""), text: d.sign.glyph || "" }));
      tr.appendChild(el("td", { class: "tfd-addr" }, [
        expandable ? el("span", { class: "tfd-caret", text: "▸" }) : el("span", { class: "tfd-caret-blank" }),
        d.primary
      ]));
      (d.cells || []).forEach(function (c, ci) {
        tr.appendChild(dataCell(ci === 0 ? "tfd-muted" : "", c, ci === 0 ? "—" : ""));
      });
      if (showProv) tr.appendChild(el("td", null, [provCell(item)]));
      tbody.appendChild(tr);

      if (expandable) {
        var drow = el("tr", { class: "tfd-detail tfd-hidden" });
        drow.appendChild(el("td", { colspan: String(cols) }, [d.detail]));
        tbody.appendChild(drow);
        tr.addEventListener("click", function () {
          var hidden = drow.classList.toggle("tfd-hidden");
          tr.querySelector(".tfd-caret").textContent = hidden ? "▸" : "▾";
          resize();
        });
      }
    });

    table.appendChild(tbody);
    return table;
  }

  function dataCell(cls, val, fallback) {
    var kid = (val == null || val === "")
      ? document.createTextNode(fallback || "")
      : (typeof val === "string" ? document.createTextNode(val) : val);
    return el("td", cls ? { class: cls } : null, [kid]);
  }

  // badge is a small pill with a category-tinted background + matching border
  // (change action, deprecation severity).
  function badge(text, kind) {
    return el("span", { class: "tfd-badge tfd-badge-" + (kind || "noop"), text: text });
  }

  function actionBadge(action) {
    var a = action || "noop";
    return badge(a, a.replace(/[^a-z]/g, ""));
  }

  // moduleOrFile fills the "Module" column: the module path when the resource is
  // in one, else a link to the .tf that declares it, else "—".
  function moduleOrFile(row, linker) {
    if (row.module) return row.module;
    return locNode(linker, row.file, row.line) || "—";
  }

  function srcLine(linker, file, line) {
    var loc = locNode(linker, file, line);
    if (!loc) return null;
    return el("div", { class: "tfd-srcline" }, [el("span", { class: "tfd-muted", text: "Source: " }), loc]);
  }

  // workItemLine links the finding to the work item tracking it, when the run
  // was given -ado-url. Both fields are already in the schema-2 attachment.
  function workItemLine(row) {
    if (!row.work_item) return null;
    var ref = "#" + row.work_item;
    var label = row.work_item_url
      ? el("a", { class: "tfd-loc", href: row.work_item_url, target: "_blank", rel: "noopener noreferrer", text: ref })
      : el("span", { class: "tfd-loc", text: ref });
    return el("div", { class: "tfd-srcline" }, [
      el("span", { class: "tfd-muted", text: "Work item: " }), label
    ]);
  }

  function ruleLine(item) {
    return el("div", { class: "tfd-srcline" }, [
      el("span", { class: "tfd-muted", text: "Rule: " }),
      el("code", { text: item.suppress_source || "(unknown)" })
    ]);
  }

  // --- drift / pending ------------------------------------------

  function driftRowFn(linker) {
    return function (row) {
      var attrs = row.attributes || [];
      // root resources show their file in the Module column; module resources
      // get it here (that column shows the module path for them).
      var src = row.module ? srcLine(linker, row.file, row.line) : null;
      var wi = workItemLine(row);
      var detail = null;
      if (attrs.length || src || wi) {
        detail = el("div", { class: "tfd-detail-body" });
        if (attrs.length) detail.appendChild(attrTable(attrs));
        if (src) detail.appendChild(src);
        if (wi) detail.appendChild(wi);
      }
      return {
        sign: { glyph: SIGN[row.action] || "?", cls: "tfd-sign-" + (row.action || "noop") },
        primary: el("code", { text: row.address || "(unknown)" }),
        cells: [moduleOrFile(row, linker), actionBadge(row.action)],
        detail: detail
      };
    };
  }

  function attrTable(attrs) {
    var t = el("table", { class: "tfd-attrs" }, [
      el("thead", null, [el("tr", null, [
        el("th", { text: "Attribute" }), el("th", { text: "From" }), el("th", { text: "To" })
      ])])
    ]);
    var body = el("tbody");
    attrs.forEach(function (a) {
      var from = valueOf(a.old), to = valueOf(a.new);
      var d = splitDiff(from, to);
      body.appendChild(el("tr", null, [
        el("td", null, [el("code", { text: a.path || "" })]),
        el("td", null, [diffCode("tfd-old", d.prefix, d.aMid, d.suffix, from)]),
        el("td", null, [diffCode("tfd-new", d.prefix, d.bMid, d.suffix, to)])
      ]));
    });
    t.appendChild(body);
    return t;
  }

  // splitDiff finds the shared prefix and suffix of a and b; the middles (aMid /
  // bMid) are the part that actually changed.
  function splitDiff(a, b) {
    var max = Math.min(a.length, b.length);
    var i = 0;
    while (i < max && a.charAt(i) === b.charAt(i)) i++;
    var j = 0;
    while (j < max - i && a.charAt(a.length - 1 - j) === b.charAt(b.length - 1 - j)) j++;
    return {
      prefix: a.slice(0, i),
      aMid: a.slice(i, a.length - j),
      bMid: b.slice(i, b.length - j),
      suffix: a.slice(a.length - j)
    };
  }

  // diffCode renders one From/To value with the changed span in a <mark>. If
  // nothing is shared (prefix and suffix both empty) the value is left plain —
  // the red/green colour already carries the signal.
  function diffCode(cls, prefix, mid, suffix, whole) {
    var code = el("code", { class: cls });
    if (prefix === "" && suffix === "") {
      code.textContent = whole;
      return code;
    }
    code.appendChild(document.createTextNode(prefix));
    if (mid !== "") code.appendChild(el("mark", { class: "tfd-diff", text: mid }));
    code.appendChild(document.createTextNode(suffix));
    return code;
  }

  // --- deprecations -----------------------------------------------

  function deprDetail(linker, d) {
    var sites = d.sites || [];
    var wi = workItemLine(d);
    if (!d.detail && !sites.length && !wi) return null;
    var body = el("div", { class: "tfd-detail-body" });
    if (d.detail) body.appendChild(el("div", { class: "tfd-depr-detail", text: d.detail }));
    if (sites.length) {
      var ul = el("ul", { class: "tfd-depr-sites" });
      sites.forEach(function (s) {
        var kids = [el("code", { text: s.address || "(unknown)" })];
        var loc = locNode(linker, s.file, s.line);
        if (loc) { kids.push(document.createTextNode("  ")); kids.push(loc); }
        ul.appendChild(el("li", null, kids));
      });
      body.appendChild(ul);
    }
    if (wi) body.appendChild(wi);
    return body;
  }

  function deprFirstResource(d) {
    var sites = d.sites || [];
    var s0 = sites[0] || {};
    var first = s0.address || (s0.file ? s0.file + (s0.line ? ":" + s0.line : "") : "");
    var more = sites.length > 1 ? "  +" + (sites.length - 1) : "";
    return first ? el("code", { text: first + more }) : "—";
  }

  function deprSign(sev) {
    return { glyph: sev === "error" ? "✖" : "⚠", cls: "tfd-sev" + (sev === "error" ? " tfd-sev-error" : "") };
  }

  function deprRowFn(linker) {
    return function (d) {
      var sev = (d.severity || "warning").toLowerCase();
      return {
        sign: deprSign(sev),
        primary: el("strong", { text: d.summary || "Deprecated" }),
        cells: [deprFirstResource(d), badge(sev, sev)],
        detail: deprDetail(linker, d)
      };
    };
  }

  // --- ignored (suppressed by an ignore rule) --------------------

  function ignoredDriftRowFn(linker) {
    return function (row) {
      var attrs = row.attributes || [];
      var detail = el("div", { class: "tfd-detail-body" });
      if (attrs.length) detail.appendChild(attrTable(attrs));
      var src = row.module ? srcLine(linker, row.file, row.line) : null;
      if (src) detail.appendChild(src);
      detail.appendChild(ruleLine(row));
      return {
        sign: { glyph: SIGN[row.action] || "?", cls: "tfd-sign-" + (row.action || "noop") },
        primary: el("code", { text: row.address || "(unknown)" }),
        cells: [moduleOrFile(row, linker), actionBadge(row.action), row.suppress_reason || "no reason given"],
        detail: detail
      };
    };
  }

  function ignoredDeprRowFn(linker) {
    return function (d) {
      var sev = (d.severity || "warning").toLowerCase();
      var detail = deprDetail(linker, d) || el("div", { class: "tfd-detail-body" });
      detail.appendChild(ruleLine(d));
      return {
        sign: deprSign(sev),
        primary: el("strong", { text: d.summary || "Deprecated" }),
        cells: [deprFirstResource(d), d.suppress_reason || "no reason given"],
        detail: detail
      };
    };
  }

  function valueOf(v) {
    if (v === undefined || v === null) return "null";
    if (typeof v === "string") return JSON.stringify(v);
    try { return JSON.stringify(v); } catch (e) { return String(v); }
  }
})();
