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

    var gating = activeDrift.length + activeDeps.length;

    // status banner
    r.appendChild(el("div", { class: "tfd-banner " + (gating ? "tfd-banner-warn" : "tfd-banner-ok") }, [
      el("strong", { text: gating ? bannerHead(activeDrift.length, activeDeps.length) : "No drift or deprecations" }),
      el("div", { class: "tfd-muted", text: metaLine(report, pending, ignoredDrift.length + ignoredDeps.length, build) })
    ]));

    r.appendChild(driftSection("Changed outside Terraform", activeDrift, true,
      "Nothing has changed outside Terraform for this run."));

    r.appendChild(deprSection("Deprecations", activeDeps,
      "No deprecation warnings in the plan."));

    if (ignoredDrift.length || ignoredDeps.length) {
      r.appendChild(ignoredSection(ignoredDrift, ignoredDeps));
    }

    r.appendChild(driftSection("Pending changes from configuration", pending, false,
      "No pending changes — configuration matches state.",
      "Changes terraform plan would apply from your configuration to reach the " +
      "desired state. Shown for context — these are not drift and do not affect " +
      "the run outcome."));

    resize();
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
    if (row.baseline_state === "new") return el("span", { class: "tfd-pill tfd-pill-new", text: "new" });
    var t = ageText(row.first_seen);
    return el("span", { class: "tfd-muted", text: t || "—" });
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

  function hasProvenance(rows) {
    return rows.some(function (r) { return r.baseline_state || r.first_seen; });
  }

  // --- drift / pending tables -----------------------------------------

  function driftSection(title, rows, expandFirst, emptyText, infoText) {
    var wrap = el("section", { class: "tfd-section" }, [
      el("h3", { class: "tfd-h3" }, [title, el("span", { class: "tfd-count", text: String(rows.length) })])
    ]);

    if (infoText) {
      wrap.appendChild(el("div", { class: "tfd-banner tfd-banner-info" }, [
        el("strong", { text: "For context" }),
        el("div", { class: "tfd-muted", text: infoText })
      ]));
    }

    if (!rows.length) {
      wrap.appendChild(el("p", { class: "tfd-muted tfd-empty", text: emptyText }));
      return wrap;
    }

    var showProv = hasProvenance(rows);
    var head = [
      el("th", { class: "tfd-col-sign", text: "" }),
      el("th", { text: "Resource" }),
      el("th", { text: "Module" }),
      el("th", { text: "Change" })
    ];
    if (showProv) head.push(el("th", { text: "First seen" }));

    var table = el("table", { class: "tfd-table" }, [el("thead", null, [el("tr", null, head)])]);
    var tbody = el("tbody");
    var cols = showProv ? 5 : 4;

    rows.forEach(function (row) {
      var attrs = row.attributes || [];
      var expandable = attrs.length > 0;
      var open = expandable && expandFirst;

      var tr = el("tr", { class: "tfd-row" + (expandable ? " tfd-row-x" : "") });
      tr.appendChild(el("td", { class: "tfd-col-sign tfd-sign-" + (row.action || "noop"), text: SIGN[row.action] || "?" }));
      tr.appendChild(el("td", { class: "tfd-addr" }, [
        expandable ? el("span", { class: "tfd-caret", text: open ? "▾" : "▸" }) : el("span", { class: "tfd-caret-blank" }),
        el("code", { text: row.address || "(unknown)" })
      ]));
      tr.appendChild(el("td", { class: "tfd-muted", text: row.module || "—" }));
      tr.appendChild(el("td", { text: (row.action || "") + (expandable ? "  (" + attrs.length + " attr" + (attrs.length === 1 ? "" : "s") + ")" : "") }));
      if (showProv) tr.appendChild(el("td", null, [provCell(row)]));
      tbody.appendChild(tr);

      if (expandable) {
        var detail = el("tr", { class: "tfd-detail" + (open ? "" : " tfd-hidden") });
        var cell = el("td", { colspan: String(cols) });
        cell.appendChild(attrTable(attrs));
        detail.appendChild(cell);
        tbody.appendChild(detail);

        tr.addEventListener("click", function () {
          var hidden = detail.classList.toggle("tfd-hidden");
          tr.querySelector(".tfd-caret").textContent = hidden ? "▸" : "▾";
          resize();
        });
      }
    });

    table.appendChild(tbody);
    wrap.appendChild(table);
    return wrap;
  }

  function attrTable(attrs) {
    var t = el("table", { class: "tfd-attrs" }, [
      el("thead", null, [el("tr", null, [
        el("th", { text: "Attribute" }), el("th", { text: "From" }), el("th", { text: "To" })
      ])])
    ]);
    var body = el("tbody");
    attrs.forEach(function (a) {
      body.appendChild(el("tr", null, [
        el("td", null, [el("code", { text: a.path || "" })]),
        el("td", null, [el("code", { class: "tfd-old", text: valueOf(a.old) })]),
        el("td", null, [el("code", { class: "tfd-new", text: valueOf(a.new) })])
      ]));
    });
    t.appendChild(body);
    return t;
  }

  // --- deprecations --------------------------------------------------

  function deprSection(title, deps, emptyText) {
    var wrap = el("section", { class: "tfd-section" }, [
      el("h3", { class: "tfd-h3" }, [title, el("span", { class: "tfd-count", text: String(deps.length) })])
    ]);

    if (!deps.length) {
      wrap.appendChild(el("p", { class: "tfd-muted tfd-empty", text: emptyText }));
      return wrap;
    }

    deps.forEach(function (d) {
      var sev = (d.severity || "warning").toLowerCase();
      var hd = el("div", { class: "tfd-depr-hd" }, [
        el("span", null, [
          el("span", { class: "tfd-sev" + (sev === "error" ? " tfd-sev-error" : ""), text: sev === "error" ? "✖" : "⚠" }),
          el("strong", { text: d.summary || "Deprecated" })
        ]),
        (d.baseline_state || d.first_seen) ? provCell(d) : null
      ]);

      var block = el("div", { class: "tfd-depr" }, [hd]);
      if (d.detail) block.appendChild(el("div", { class: "tfd-depr-detail", text: d.detail }));

      var sites = d.sites || [];
      if (sites.length) {
        var ul = el("ul", { class: "tfd-depr-sites" });
        sites.forEach(function (s) {
          var loc = s.file ? s.file + (s.line ? ":" + s.line : "") : "";
          var kids = [el("code", { text: s.address || loc || "(unknown)" })];
          if (s.address && loc) kids.push(el("span", { class: "tfd-muted", text: "  " + loc }));
          ul.appendChild(el("li", null, kids));
        });
        block.appendChild(ul);
      }
      wrap.appendChild(block);
    });

    return wrap;
  }

  // --- ignored (suppressed by an ignore rule) ----------------------

  function ignoredSection(drift, deps) {
    var n = drift.length + deps.length;
    var wrap = el("section", { class: "tfd-section" }, [
      el("h3", { class: "tfd-h3" }, ["Ignored", el("span", { class: "tfd-count", text: String(n) })])
    ]);

    var table = el("table", { class: "tfd-table tfd-ignored" }, [
      el("thead", null, [el("tr", null, [
        el("th", { text: "Item" }), el("th", { text: "Reason" }), el("th", { text: "Rule" })
      ])])
    ]);
    var tbody = el("tbody");

    function row(item, r) {
      tbody.appendChild(el("tr", null, [
        el("td", null, [el("code", { text: item })]),
        el("td", { text: r.suppress_reason || "no reason given" }),
        el("td", { class: "tfd-muted", text: r.suppress_source || "" })
      ]));
    }
    drift.forEach(function (d) { row(d.address || "(unknown)", d); });
    deps.forEach(function (d) { row(d.summary || "deprecation", d); });

    table.appendChild(tbody);
    wrap.appendChild(table);
    return wrap;
  }

  function valueOf(v) {
    if (v === undefined || v === null) return "null";
    if (typeof v === "string") return JSON.stringify(v);
    try { return JSON.stringify(v); } catch (e) { return String(v); }
  }
})();
