/* Docs viewer: fetches the repo's own markdown (copied into content/ at
   deploy time) and renders it client-side, so the site never drifts from
   the docs. Hash routing: #page or #page/section.
   ponytail: hand-rolled renderer scoped to the markdown these docs actually
   use (headings, fences, tables, nested lists, quotes, inline code/bold/
   italic/links). If the docs outgrow it, swap in a vendored marked.min.js. */

"use strict";

var REPO = "https://github.com/eiserv/easySFTP";

/* dir: what links inside the source file are relative to, in the repo */
var PAGES = [
  { id: "configuration", title: "configuration", group: "guide", dir: "docs/" },
  { id: "strategies", title: "strategies", group: "guide", dir: "docs/" },
  { id: "examples", title: "examples", group: "guide", dir: "docs/" },
  { id: "security", title: "security", group: "guide", dir: "docs/" },
  { id: "troubleshooting", title: "troubleshooting", group: "guide", dir: "docs/" },
  { id: "contributing", title: "contributing", group: "project", dir: "" },
  { id: "migration-v3", title: "migrating v2 => v3", group: "project", dir: "docs/" },
  { id: "migrating-v1-to-v2", title: "migrating v1 => v2", group: "project", dir: "docs/" }
];

/* ---------- helpers ---------- */

function esc(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
          .replace(/"/g, "&quot;");
}

function slugify(text) {
  return text.toLowerCase().replace(/[`*_]/g, "").replace(/[^\w\- ]/g, "")
             .trim().replace(/ +/g, "-");
}

function pageById(id) {
  for (var k = 0; k < PAGES.length; k++) if (PAGES[k].id === id) return PAGES[k];
  return null;
}

/* Rewrite a markdown link target for the site. */
function rewriteHref(href, page) {
  if (/^(https?:)?\/\//.test(href) || href.indexOf("mailto:") === 0) return href;
  if (href.charAt(0) === "#") return "#" + page.id + "/" + href.slice(1);

  var frag = href.indexOf("#") >= 0 ? href.slice(href.indexOf("#") + 1) : "";
  var stack = [];
  (page.dir + href.split("#")[0]).split("/").forEach(function (p) {
    if (p === "..") stack.pop();
    else if (p !== "." && p !== "") stack.push(p);
  });
  var path = stack.join("/");

  var m = path.match(/^docs\/([\w.\-]+)\.md$/i);
  var id = m ? m[1].toLowerCase() : (path.toUpperCase() === "CONTRIBUTING.MD" ? "contributing" : null);
  if (id && pageById(id)) return "#" + id + (frag ? "/" + frag : "");

  return REPO + "/blob/main/" + path + (frag ? "#" + frag : "");
}

/* ---------- inline markdown ---------- */

function inline(text, page) {
  var codes = [];
  text = text.replace(/`([^`]+)`/g, function (_, c) {
    codes.push("<code>" + esc(c) + "</code>");
    return "\x00" + (codes.length - 1) + "\x00";
  });
  text = esc(text);
  text = text.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, function (_, label, href) {
    var url = rewriteHref(href, page);
    var ext = /^https?:\/\//.test(url) ? ' target="_blank" rel="noopener"' : "";
    return '<a href="' + url + '"' + ext + ">" + label + "</a>";
  });
  text = text.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  text = text.replace(/(^|[\s(])\*([^*\s][^*]*)\*/g, "$1<em>$2</em>");
  text = text.replace(/(^|[\s(])_([^_]+)_(?=[\s.,;:!?)]|$)/g, "$1<em>$2</em>");
  return text.replace(/\x00(\d+)\x00/g, function (_, n) { return codes[n]; });
}

/* ---------- block markdown ---------- */

function renderMarkdown(md, page) {
  var lines = md.split(/\r?\n/);
  var out = [];
  var slugCount = {};
  var i = 0;
  var itemRe = /^(\s*)(?:[-*]|\d+\.)\s+(.*)$/;

  function parseList(baseIndent) {
    var ordered = /^\s*\d+\./.test(lines[i]);
    var html = ordered ? "<ol>" : "<ul>";
    while (i < lines.length) {
      // tolerate blank lines between items of the same list
      if (/^\s*$/.test(lines[i])) {
        var j = i;
        while (j < lines.length && /^\s*$/.test(lines[j])) j++;
        var peek = j < lines.length ? lines[j].match(itemRe) : null;
        if (peek && peek[1].length >= baseIndent) { i = j; continue; }
        break;
      }
      var m = lines[i].match(itemRe);
      if (!m || m[1].length < baseIndent) break;
      var text = m[2];
      i++;
      // continuation lines wrapped under the same item
      while (i < lines.length && /^\s+\S/.test(lines[i]) && !itemRe.test(lines[i])) {
        text += " " + lines[i].trim();
        i++;
      }
      var child = "";
      var next = i < lines.length ? lines[i].match(itemRe) : null;
      if (next && next[1].length > baseIndent) child = parseList(next[1].length);
      html += "<li>" + inline(text, page) + child + "</li>";
    }
    return html + (ordered ? "</ol>" : "</ul>");
  }

  while (i < lines.length) {
    var line = lines[i];

    if (/^\s*$/.test(line)) { i++; continue; }

    var h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) {
      var slug = slugify(h[2]);
      if (slugCount[slug] !== undefined) slug += "-" + (++slugCount[slug]);
      else slugCount[slug] = 0;
      out.push("<h" + h[1].length + ' id="' + slug + '">' + inline(h[2], page) +
        ' <a class="anchor" href="#' + page.id + "/" + slug +
        '" aria-label="Link to this section">#</a></h' + h[1].length + ">");
      i++;
      continue;
    }

    if (/^```/.test(line)) {
      i++;
      var code = [];
      while (i < lines.length && !/^```/.test(lines[i])) code.push(lines[i++]);
      i++; // closing fence
      out.push('<pre><button class="copy-btn" type="button">copy</button><code>' +
        esc(code.join("\n")) + "</code></pre>");
      continue;
    }

    if (/^---+\s*$/.test(line)) { out.push("<hr>"); i++; continue; }

    if (/^\|/.test(line) && i + 1 < lines.length && /^\|?[\s:|\-]+\|/.test(lines[i + 1])) {
      var rows = [];
      while (i < lines.length && /^\|/.test(lines[i])) rows.push(lines[i++]);
      var cells = function (row) {
        return row.replace(/^\|/, "").replace(/\|\s*$/, "").split("|")
                  .map(function (c) { return c.trim(); });
      };
      var t = '<div class="table-wrap"><table><thead><tr>';
      cells(rows[0]).forEach(function (c) { t += "<th>" + inline(c, page) + "</th>"; });
      t += "</tr></thead><tbody>";
      for (var r = 2; r < rows.length; r++) {
        t += "<tr>";
        cells(rows[r]).forEach(function (c) { t += "<td>" + inline(c, page) + "</td>"; });
        t += "</tr>";
      }
      out.push(t + "</tbody></table></div>");
      continue;
    }

    if (/^>\s?/.test(line)) {
      var quote = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) {
        quote.push(lines[i++].replace(/^>\s?/, ""));
      }
      out.push("<blockquote><p>" + inline(quote.join(" "), page) + "</p></blockquote>");
      continue;
    }

    if (itemRe.test(line)) { out.push(parseList(line.match(/^(\s*)/)[1].length)); continue; }

    var para = [];
    while (i < lines.length && !/^\s*$/.test(lines[i]) &&
           !/^(#{1,6}\s|```|\||>\s?|\s*[-*]\s|\s*\d+\.\s|---+\s*$)/.test(lines[i])) {
      para.push(lines[i++].trim());
    }
    out.push("<p>" + inline(para.join(" "), page) + "</p>");
  }
  return out.join("\n");
}

/* ---------- app ---------- */

var content = document.getElementById("content");
var sidebar = document.getElementById("sidebar");

function buildSidebar() {
  var groups = {};
  PAGES.forEach(function (p) { (groups[p.group] = groups[p.group] || []).push(p); });
  var html = "";
  Object.keys(groups).forEach(function (g) {
    html += '<div class="group">' + g + "</div><ul>";
    groups[g].forEach(function (p) {
      html += '<li><a href="#' + p.id + '" data-page="' + p.id + '">' + esc(p.title) + "</a></li>";
    });
    html += "</ul>";
  });
  sidebar.innerHTML = html;
}

function parseHash() {
  var parts = location.hash.replace(/^#/, "").split("/");
  var page = pageById(parts[0]) ? parts[0] : PAGES[0].id;
  return { page: page, frag: parts.slice(1).join("/") };
}

var currentPage = null;

function route() {
  var r = parseHash();
  if (r.page === currentPage) { scrollToFrag(r.frag); return; }
  currentPage = r.page;
  var page = pageById(r.page);
  sidebar.querySelectorAll("a").forEach(function (a) {
    a.classList.toggle("active", a.dataset.page === r.page);
  });
  document.title = page.title.replace(" =>", " to") + " — easySFTP";
  content.innerHTML = '<p class="md-status">Loading&hellip;</p>';
  fetch("content/" + r.page + ".md")
    .then(function (res) {
      if (!res.ok) throw new Error(res.status);
      return res.text();
    })
    .then(function (md) {
      content.innerHTML = renderMarkdown(md, page);
      hookCopyButtons();
      scrollToFrag(r.frag);
    })
    .catch(function () {
      content.innerHTML = '<p class="md-status">Could not load this page. ' +
        'Read it <a href="' + REPO + '/tree/main/docs">on GitHub</a> instead.</p>';
    });
}

function scrollToFrag(frag) {
  if (!frag) { window.scrollTo(0, 0); return; }
  var el = document.getElementById(frag);
  if (el) el.scrollIntoView();
}

function hookCopyButtons() {
  content.querySelectorAll("pre .copy-btn").forEach(function (btn) {
    btn.addEventListener("click", function () {
      navigator.clipboard.writeText(btn.parentElement.querySelector("code").textContent)
        .then(function () {
          btn.textContent = "copied";
          setTimeout(function () { btn.textContent = "copy"; }, 1500);
        });
    });
  });
}

buildSidebar();
window.addEventListener("hashchange", route);
route();
