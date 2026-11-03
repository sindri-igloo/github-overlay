// Dashboard UI shared by the standalone page (index.html) and the browser
// extension. GithubOverlay.mount(root, opts) renders into root, which can sit
// in a document or a shadow root, and returns { destroy }.
//   opts.fetch          fetch-compatible function for /api/* (default: fetch)
//   opts.linkTarget     target for PR links (default: _blank)
//   opts.storagePrefix  localStorage key prefix
//   opts.unreachable    error shown when the server can't be reached
//   opts.favicon        update the page's #favicon link
(() => {
const TEMPLATE = `<div class="wrap">
  <header>
    <h1>Pull requests</h1>
    <div class="filters orgs" id="orgs"></div>
    <span class="meta" id="meta"></span>
    <button class="iconbtn" id="refresh" title="Refresh now (r)">↻</button>
  </header>
  <div id="error"></div>
  <div class="bar">
    <nav class="tabs" id="tabs"></nav>
    <div class="filters" id="filters"></div>
  </div>
  <div class="cols"><span class="selcol"></span><span></span><span></span><span title="CI">CI</span><span>Reviews</span><span title="Unresolved threads">Threads</span><span>Age</span></div>
  <div class="list" id="list"><div class="empty">Loading pull requests…</div></div>
</div>
<div class="selbar" id="selbar" hidden></div>`;

function mount(root, opts = {}) {
  const target = opts.linkTarget || "_blank";
  const prefix = opts.storagePrefix || "";
  const doFetch = opts.fetch || ((...a) => fetch(...a));
  const $ = id => root.querySelector("#" + id);
  // Listeners on document see events from a shadow root retargeted to its host.
  const evt = e => { const t = e.composedPath()[0]; return t instanceof Element ? t : document.documentElement; };
  const activeEl = () => root.getRootNode().activeElement;
  // Page position of the picker's offset parent; (0, 0) when that's the body.
  const origin = () => {
    const op = picker?.el.offsetParent;
    if (!op || op === document.body) return { x: 0, y: 0 };
    const b = op.getBoundingClientRect();
    return { x: b.left + window.scrollX + op.clientLeft - op.scrollLeft, y: b.top + window.scrollY + op.clientTop - op.scrollTop };
  };
  const listeners = [], timers = [];
  const on = (el, type, fn, capture) => { el.addEventListener(type, fn, capture); listeners.push([el, type, fn, capture]); };

  root.classList.add("gho");
  root.innerHTML = TEMPLATE;
  // Inline handlers don't run under GitHub's CSP, so broken avatars go here.
  root.addEventListener("error", e => { if (e.target.tagName === "IMG") e.target.remove(); }, true);

  const TABS = [
    { id: "mine", label: "Mine" },
    { id: "review", label: "To review" },
    { id: "closed", label: "Closed" },
  ];

  const isOpen = p => p.state === "OPEN";
  const isDone = p => p.state === "MERGED" || p.state === "CLOSED";
  const ciFailed = p => p.ci.state === "FAILURE" || p.ci.state === "ERROR";
  const humanReviews = p => p.reviews.filter(r => !r.bot);
  // A change-requester who has been asked to look again no longer blocks on the
  // author, so changesBy (needs action) and reReview (waiting) are kept apart.
  const changeRequesters = p => p.reviews.filter(r => r.state === "CHANGES_REQUESTED").map(r => r.login);
  const isRequested = (p, login) => p.requestedReviewers.some(l => l.toLowerCase() === login.toLowerCase());
  const changesBy = p => changeRequesters(p).filter(l => !isRequested(p, l));
  const reReview = p => changeRequesters(p).filter(l => isRequested(p, l));
  const noReviewer = p => isOpen(p) && !p.requestedReviewers.length && !humanReviews(p).length;

  // Shown on the Mine tab only; chips with no matches are hidden.
  const FILTERS = {
    all:     { label: "All", fn: () => true },
    action:  { label: "Needs action", fn: p => ciFailed(p) || changesBy(p).length > 0 || p.humanThreads > 0 || p.conflicting },
    waiting: { label: "Waiting on review", fn: p => p.requestedReviewers.length > 0 },
    noRev:   { label: "No reviewer", fn: noReviewer },
    ready:   { label: "Ready to merge", fn: p => p.reviewDecision === "APPROVED" && p.ci.state === "SUCCESS" && !p.conflicting && !p.humanThreads },
    drafts:  { label: "Drafts", fn: p => p.state === "DRAFT" },
  };

  const store = {
    get(k, d) { try { return localStorage.getItem(prefix + k) ?? d; } catch { return d; } },
    set(k, v) { try { localStorage.setItem(prefix + k, v); } catch {} },
  };

  let data = null;
  const expanded = new Set(); // bundles whose merged/closed PRs are unfolded
  let editing = null; // { gid, keys, auto, draft, drawn }: the group name being renamed
  const selectedPRs = new Set(); // Mine-tab rows ticked for a bulk reviewer request
  let shownSel = new Set(); // requestable PRs currently rendered
  let tab = store.get("tab", "mine");
  let filter = store.get("filter", "all");
  if (!TABS.some(t => t.id === tab)) tab = "mine";
  if (!FILTERS[filter]) filter = "all";
  // Repo owners to show, on every tab; empty shows all.
  const orgs = new Set((() => { try { return JSON.parse(store.get("orgs", "[]")); } catch { return []; } })());
  const ownerOf = p => p.repo.split("/")[0].toLowerCase();
  const inOrgs = p => !orgs.size || orgs.has(ownerOf(p));
  const section = id => (data.sections[id] || []).map(k => data.prs[k]).filter(inOrgs);

  const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  function ago(iso) {
    const s = (Date.now() - new Date(iso)) / 1000;
    if (s < 60) return "now";
    if (s < 3600) return Math.floor(s / 60) + "m";
    if (s < 86400) return Math.floor(s / 3600) + "h";
    return Math.floor(s / 86400) + "d";
  }

  // Octicon paths (MIT, GitHub).
  const P = {
    OPEN:   "M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354ZM3.75 2.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm0 9.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm8.25.75a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0Z",
    DRAFT:  "M3.25 1A2.25 2.25 0 0 1 4 5.372v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.251 2.251 0 0 1 3.25 1Zm9.5 14a2.25 2.25 0 1 1 0-4.5 2.25 2.25 0 0 1 0 4.5ZM2.5 3.25a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0ZM3.25 12a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm9.5 0a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5ZM14 7.5a1.25 1.25 0 1 1-2.5 0 1.25 1.25 0 0 1 2.5 0Zm0-4.25a1.25 1.25 0 1 1-2.5 0 1.25 1.25 0 0 1 2.5 0Z",
    MERGED: "M5.45 5.154A4.25 4.25 0 0 0 9.25 7.5h1.378a2.251 2.251 0 1 1 0 1.5H9.25A5.734 5.734 0 0 1 5 7.123v3.505a2.25 2.25 0 1 1-1.5 0V5.372a2.25 2.25 0 1 1 1.95-.218ZM4.25 13.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5Zm8.5-4.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5ZM5 3.25a.75.75 0 1 0 0 .005V3.25Z",
    CLOSED: "M3.25 1A2.25 2.25 0 0 1 4 5.372v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.251 2.251 0 0 1 3.25 1Zm9.5 5.5a.75.75 0 0 1 .75.75v3.378a2.251 2.251 0 1 1-1.5 0V7.25a.75.75 0 0 1 .75-.75Zm-2.03-5.273a.75.75 0 0 1 1.06 0l.97.97.97-.97a.748.748 0 0 1 1.265.332.75.75 0 0 1-.205.729l-.97.97.97.97a.751.751 0 0 1-.018 1.042.751.751 0 0 1-1.042.018l-.97-.97-.97.97a.749.749 0 0 1-1.275-.326.749.749 0 0 1 .215-.734l.97-.97-.97-.97a.75.75 0 0 1 0-1.06ZM2.5 3.25a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0ZM3.25 12a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm9.5 0a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Z",
    check:  "M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z",
    x:      "M3.72 3.72a.75.75 0 0 1 1.06 0L8 6.94l3.22-3.22a.749.749 0 0 1 1.275.326.749.749 0 0 1-.215.734L9.06 8l3.22 3.22a.749.749 0 0 1-.326 1.275.749.749 0 0 1-.734-.215L8 9.06l-3.22 3.22a.751.751 0 0 1-1.042-.018.751.751 0 0 1-.018-1.042L6.94 8 3.72 4.78a.75.75 0 0 1 0-1.06Z",
    dot:    "M8 4a4 4 0 1 1 0 8 4 4 0 0 1 0-8Z",
    link:   "m7.775 3.275 1.25-1.25a3.5 3.5 0 1 1 4.95 4.95l-2.5 2.5a3.5 3.5 0 0 1-4.95 0 .751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018 1.998 1.998 0 0 0 2.83 0l2.5-2.5a2.002 2.002 0 0 0-2.83-2.83l-1.25 1.25a.751.751 0 0 1-1.042-.018.751.751 0 0 1-.018-1.042Zm-4.69 9.64a1.998 1.998 0 0 0 2.83 0l1.25-1.25a.751.751 0 0 1 1.042.018.751.751 0 0 1 .018 1.042l-1.25 1.25a3.5 3.5 0 1 1-4.95-4.95l2.5-2.5a3.5 3.5 0 0 1 4.95 0 .751.751 0 0 1-.018 1.042.751.751 0 0 1-1.042.018 1.998 1.998 0 0 0-2.83 0l-2.5 2.5a1.998 1.998 0 0 0 0 2.83Z",
    comment:"M1 2.75C1 1.784 1.784 1 2.75 1h10.5c.966 0 1.75.784 1.75 1.75v7.5A1.75 1.75 0 0 1 13.25 12H9.06l-2.573 2.573A1.458 1.458 0 0 1 4 13.543V12H2.75A1.75 1.75 0 0 1 1 10.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h2a.75.75 0 0 1 .75.75v2.19l2.72-2.72a.749.749 0 0 1 .53-.22h4.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z",
  };
  const STATE_COLOR = { OPEN: "var(--green)", DRAFT: "var(--gray)", MERGED: "var(--purple)", CLOSED: "var(--red)" };
  const svg = (path, color, size = 16) =>
    `<svg width="${size}" height="${size}" viewBox="0 0 16 16" fill="${color}"><path d="${path}"/></svg>`;

  // Drop the owner when it's the one most PRs share, so rows read "rent-api #4071".
  let mainOwner = "";
  function repoLabel(repo) {
    const [owner, name] = repo.split("/");
    return owner.toLowerCase() === mainOwner ? name : repo;
  }
  const DEFAULT_BASES = new Set(["main", "master", "develop", "staging"]);

  function ciCell(p) {
    if (isDone(p)) return "";
    const ci = p.ci, url = p.url + "/checks";
    if (!ci.state) return `<span title="No checks">–</span>`;
    if (ciFailed(p)) {
      const names = ci.failed.map(c => "✕ " + c.name).join("\n");
      const href = ci.failed.length === 1 && ci.failed[0].url ? ci.failed[0].url : url;
      return `<a href="${esc(href)}" target="${target}" title="${esc(`${ci.failed.length} of ${ci.total} failed\n${names}`)}">${svg(P.x, "var(--red)")}</a>`;
    }
    if (ci.state === "PENDING" || ci.state === "EXPECTED")
      return `<a href="${esc(url)}" target="${target}" title="${ci.pending.length} of ${ci.total} running">${svg(P.dot, "var(--yellow)")}</a>`;
    return `<a href="${esc(url)}" target="${target}" title="${ci.total} checks passed">${svg(P.check, "var(--green)")}</a>`;
  }

  const REVIEW_CLASS = { APPROVED: "approved", CHANGES_REQUESTED: "changes", COMMENTED: "commented" };
  const REVIEW_TEXT = { APPROVED: "approved", CHANGES_REQUESTED: "requested changes", COMMENTED: "commented", requested: "review requested" };

  const BADGE = { approved: P.check, changes: P.x, requested: P.dot, commented: P.comment };

  // login is a user login or "@team-slug"; kind is a ring class (or "" for none).
  function avatar(login, kind = "", title = "") {
    const team = login.startsWith("@");
    // The initial sits under the image and shows if the avatar fails to load.
    const img = team ? "" : `<img src="https://avatars.githubusercontent.com/${encodeURIComponent(login)}?s=44" alt="" loading="lazy">`;
    const label = team ? "@" : login[0].toUpperCase();
    const badge = BADGE[kind] ? `<span class="badge">${svg(BADGE[kind], "#fff", 8)}</span>` : "";
    return `<span class="av ${kind}"${title ? ` title="${esc(title)}"` : ""}>${label}${img}${badge}</span>`;
  }

  const lc = s => s.toLowerCase();
  const canRequest = p => tab === "mine" && (p.state === "OPEN" || p.state === "DRAFT");

  function reviewCell(p) {
    const requested = new Set(p.requestedReviewers.map(lc));
    const reviewed = Object.fromEntries(humanReviews(p).map(r => [lc(r.login), r.state]));
    // A reviewer who was re-requested shows once, as requested.
    const people = [
      ...humanReviews(p).filter(r => REVIEW_CLASS[r.state] && !requested.has(lc(r.login)))
        .map(r => [r.login, REVIEW_CLASS[r.state], `${r.login}: ${REVIEW_TEXT[r.state]}`]),
      ...p.requestedReviewers.map(l => {
        const prev = reviewed[lc(l)];
        return [l, "requested", `${l}: review requested${prev ? ` (previously ${REVIEW_TEXT[prev]})` : ""}`];
      }),
    ];
    let inner = "";
    if (people.length) {
      const shown = people.slice(0, 4).map(([l, k, t]) => avatar(l, k, t)).join("");
      const more = people.length > 4 ? `<span class="more">+${people.length - 4}</span>` : "";
      inner = `<div class="avatars">${shown}${more}</div>`;
    } else if (noReviewer(p)) {
      inner = `<span class="none">none</span>`;
    }
    if (!canRequest(p)) return inner;
    const open = picker && !picker.bulk && picker.key === p.key ? " open" : "";
    return `<button class="revbtn${open}" data-reviewers="${esc(p.key)}" title="Request reviewers">${inner}<span class="plus">+</span></button>`;
  }

  function threadCell(p) {
    if (!p.humanThreads && !p.botThreads) return "";
    const title = [p.humanThreads && `${p.humanThreads} from people`, p.botThreads && `${p.botThreads} from bots`].filter(Boolean).join(", ");
    const n = p.humanThreads ? `${p.humanThreads}${p.botThreads ? `<span class="bot">+${p.botThreads}</span>` : ""}` : `<span class="bot">${p.botThreads}</span>`;
    return `<a class="threads ${p.humanThreads ? "hot" : ""}" href="${esc(p.url)}/files" target="${target}" title="Unresolved: ${title}">${svg(P.comment, "currentColor", 12)}${n}</a>`;
  }

  function row(p, bundleMembers) {
    const sub = [`<span>${esc(repoLabel(p.repo))} #${p.number}</span>`];
    if (tab === "review") sub.push(`<span>${esc(p.author)}</span>`);
    if (!DEFAULT_BASES.has(p.base)) sub.push(`<span title="Base branch">on ${esc(p.base)}</span>`);
    sub.push(`<span><span class="add">+${p.additions}</span> <span class="del">−${p.deletions}</span></span>`);
    const by = changesBy(p), again = reReview(p);
    if (by.length) sub.push(`<span class="warn">changes requested by ${by.map(esc).join(", ")}</span>`);
    else if (again.length) sub.push(`<span>re-review requested from ${again.map(esc).join(", ")}</span>`);
    if (p.conflicting) sub.push(`<span class="warn">conflicts</span>`);
    // Links already visible in this bundle are noise; show only the rest.
    const ext = p.links.filter(k => !bundleMembers.has(k) && data.prs[k]);
    if (ext.length) sub.push(`<span>→ ${ext.map(k => {
      const t = data.prs[k];
      return `<a href="${esc(t.url)}" target="${target}">${esc(repoLabel(t.repo))} #${t.number}</a> ${t.state.toLowerCase()}`;
    }).join(", ")}</span>`);
    const when = isDone(p) ? p.closedAt : p.updatedAt;
    // Rows that can't be requested get an empty cell so the columns line up.
    const pick = tab !== "mine" ? "" : canRequest(p)
      ? `<span><input type="checkbox" class="rowsel" data-sel="${esc(p.key)}" ${selectedPRs.has(p.key) ? "checked" : ""} aria-label="Select ${esc(repoLabel(p.repo))} #${p.number}"></span>`
      : `<span></span>`;
    const cls = (isDone(p) && tab !== "closed" ? " done" : "") + (!isDone(p) && by.length ? " changes" : "");
    return `<div class="row${cls}">
      ${pick}<span title="${p.state.toLowerCase()}">${svg(P[p.state] || P.OPEN, STATE_COLOR[p.state])}</span>
      <div style="min-width:0">
        <a class="title" href="${esc(p.url)}" target="${target}" title="${esc(p.title + "\n" + p.head + " → " + p.base)}">${esc(p.title)}</a>
        <div class="sub">${sub.join("")}</div>
      </div>
      <div class="cell ci">${ciCell(p)}</div>
      <div class="cell rev">${reviewCell(p)}</div>
      <div class="cell thr">${threadCell(p)}</div>
      <div class="cell age" title="${esc(new Date(when).toLocaleString())}">${ago(when)}</div>
    </div>`;
  }

  // Pull-request icon; a red dot means something on the Mine tab needs action.
  function setFavicon(alert) {
    const dot = alert ? `<circle cx="12.5" cy="3.5" r="3.5" fill="#f85149"/>` : "";
    const icon = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><path fill="#2da44e" d="${P.OPEN}"/>${dot}</svg>`;
    if (opts.favicon) document.getElementById("favicon").href = "data:image/svg+xml," + encodeURIComponent(icon);
  }

  const SCOPED = /^\w+(\(([^)]+)\))?!?:/;

  // Name for a group with no manual one: its conventional-commit scopes, most
  // shared first, else the title of the stack's bottom PR.
  function autoName(keys) {
    const prs = keys.map(k => data.prs[k]).filter(Boolean);
    const scopes = new Map(); // lowercased -> { name, n }
    for (const p of prs) {
      const seen = new Set();
      for (const part of (SCOPED.exec(p.title)?.[2] || "").split(/[,/]/)) {
        const name = part.trim(), low = name.toLowerCase();
        if (!name || seen.has(low)) continue;
        seen.add(low);
        const e = scopes.get(low) || scopes.set(low, { name, n: 0 }).get(low);
        e.n++;
      }
    }
    // Map keeps first-seen order and sort is stable, so ties stay in member order.
    const top = [...scopes.values()].sort((a, b) => b.n - a.n).slice(0, 3);
    if (top.length) return top.map(e => e.name).join(" · ");
    const bottoms = prs.filter(p => !prs.some(q => q !== p && q.head === p.base));
    const bottom = (bottoms.length ? bottoms : prs).sort((a, b) => a.createdAt.localeCompare(b.createdAt))[0];
    return bottom ? bottom.title.replace(/^\w+(\([^)]*\))?!?:\s*/, "") : "Connected";
  }

  // The newest manual name that shares a PR with the group, or "".
  function manualName(keys) {
    const mine = new Set(keys.map(k => k.toLowerCase()));
    const hits = (data.groupNames || []).filter(g => g.prs.some(k => mine.has(k)));
    return hits.sort((a, b) => b.at.localeCompare(a.at))[0]?.name || "";
  }

  async function saveName() {
    const ed = editing;
    if (!ed) return;
    editing = null;
    try {
      const r = await api("/api/groups", { method: "PUT", body: JSON.stringify({ prs: ed.keys, name: ed.draft.trim() }) });
      const body = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(body.error || r.statusText);
      data.groupNames = body.names;
    } catch (e) {
      data.error = e instanceof TypeError ? (opts.unreachable || "Overlay server unreachable") : "Overlay: " + e.message;
    }
    render();
  }

  function render() {
    if (!data) return;
    setFavicon(section("mine").some(FILTERS.action.fn));
    const owners = {}, visible = {};
    Object.values(data.prs).forEach(p => {
      const o = ownerOf(p);
      owners[o] = (owners[o] || 0) + 1;
      if (inOrgs(p)) visible[o] = (visible[o] || 0) + 1;
    });
    mainOwner = Object.entries(visible).sort((a, b) => b[1] - a[1])[0]?.[0] || "";

    // A saved org with no PRs right now keeps its chip, so it can be unticked.
    const orgList = [...new Set([...Object.keys(owners).sort((a, b) => owners[b] - owners[a]), ...orgs])];
    $("orgs").innerHTML = orgList.length > 1 || orgs.size
      ? [`<button class="chip ${orgs.size ? "" : "active"}" data-org="">All orgs</button>`,
         ...orgList.map(o => `<button class="chip ${orgs.has(o) ? "active" : ""}" data-org="${esc(o)}">${esc(o)}</button>`)].join("")
      : "";

    $("error").innerHTML = data.error ? `<div class="error">⚠ ${esc(data.error)}</div>` : "";
    $("meta").textContent =
      data.fetchedAt ? `updated ${ago(data.fetchedAt) === "now" ? "just now" : ago(data.fetchedAt) + " ago"}` : "loading…";
    $("meta").title = `${data.viewer || ""} · ${data.rateRemaining} API points left`;

    $("tabs").innerHTML = TABS.map(t =>
      `<button class="tab ${t.id === tab ? "active" : ""}" data-tab="${t.id}">${t.label}<span class="n">${section(t.id).length}</span></button>`
    ).join("");

    const prs = section(tab);
    const useFilters = tab === "mine";
    $("filters").innerHTML = useFilters ? Object.entries(FILTERS).map(([id, f]) => {
      const n = prs.filter(f.fn).length;
      if (!n && id !== filter && id !== "all") return "";
      return `<button class="chip ${id === filter ? "active" : ""}" data-filter="${id}">${f.label}${id === "all" ? "" : `<span class="n">${n}</span>`}</button>`;
    }).join("") : "";

    const shown = prs.filter(useFilters ? FILTERS[filter].fn : () => true);
    // Only requestable rows that are on screen can stay selected.
    shownSel = new Set(shown.filter(canRequest).map(p => p.key));
    for (const k of [...selectedPRs]) if (!shownSel.has(k)) selectedPRs.delete(k);
    root.classList.toggle("sel", tab === "mine");

    // Unconnected PRs share one card at the top; each bundle follows in its
    // own card. Both keep the section's order (most recently updated first).
    // Open tabs group by bundle, which can include merged/closed PRs linked to
    // the open ones; those stay folded until asked for. The Closed tab groups
    // closed PRs linked to each other.
    const groupOf = p => tab === "closed" ? p.closedBundle : p.bundle;
    const loose = [], bundles = [], done = new Set();
    for (const p of shown) {
      if (done.has(p.key)) continue;
      done.add(p.key);
      const id = groupOf(p);
      if (!id) { loose.push(row(p, new Set([p.key]))); continue; }
      const members = shown.filter(q => groupOf(q) === id);
      members.forEach(q => done.add(q.key));
      const all = new Set(data.bundles[id]);
      const finished = tab === "closed" ? [] : [...all].map(k => data.prs[k]).filter(isDone)
        .sort((a, b) => (b.closedAt || "").localeCompare(a.closedAt || ""));
      const gid = data.bundles[id][0]; // ids renumber on refresh; the first member is steadier
      const open = expanded.has(gid);
      const hidden = all.size - members.length - finished.length;
      const counts = tab === "closed" ? `${all.size} PRs` : `${all.size - finished.length} open`;
      const reqKeys = members.filter(canRequest).map(q => q.key);
      const pickAll = reqKeys.length
        ? `<button class="toggle" data-selkeys="${esc(reqKeys.join(","))}">${reqKeys.every(k => selectedPRs.has(k)) ? "Unselect" : "Select"}</button>` : "";
      const toggle = pickAll + (finished.length
        ? `<button class="toggle" data-expand="${esc(gid)}">${open ? "Hide" : "Show"} ${finished.length} ${finished.every(q => q.state === "MERGED") ? "merged" : "closed"}</button>` : "");
      const keys = [...all], auto = autoName(keys), manual = manualName(keys);
      const nameEl = editing?.gid === gid
        ? `<input class="name-input" value="${esc(editing.draft)}" placeholder="${esc(auto)}" maxlength="80">`
        : `<button class="name" data-rename="${esc(gid)}" data-keys="${esc(keys.join(","))}" data-auto="${esc(auto)}" data-manual="${esc(manual)}" title="Rename group">${esc(manual || auto)}</button>`;
      const rows = members.map(q => row(q, all)).join("") + (open ? finished.map(q => row(q, all)).join("") : "");
      bundles.push(`<div class="card bundle"><div class="bundle-head">${svg(P.link, "currentColor", 12)}${nameEl}<span class="n">${counts}${hidden ? `, ${hidden} not shown` : ""}</span>${toggle}</div>${rows}</div>`);
    }
    // An open rename input survives the 30s redraws by leaving the list alone.
    if (editing?.drawn) return renderSelbar();
    const cards = [...(loose.length ? [`<div class="card">${loose.join("")}</div>`] : []), ...bundles];
    $("list").innerHTML = cards.join("") || `<div class="empty">Nothing here.</div>`;
    renderSelbar();
    if (editing) {
      editing.drawn = true;
      const input = root.querySelector(".name-input");
      if (input) { input.focus(); input.select(); } else editing = null; // its group is gone
    }
  }

  function renderSelbar() {
    const n = selectedPRs.size, bar = $("selbar");
    root.querySelector(".wrap").classList.toggle("hasbar", n > 0);
    bar.hidden = !n;
    if (!n) { bar.innerHTML = ""; return; }
    bar.innerHTML = `<span>${n} PR${n === 1 ? "" : "s"} selected</span><span class="sep">·</span>
      <button class="btn" data-bar="all">Select all shown</button>
      <button class="btn" data-bar="clear">Clear</button>
      <button class="btn primary" data-bar="request" data-bulk>Request reviewers…</button>`;
  }

  // The server rejects API calls without this header (see guard in main.go).
  function api(path, opts = {}) {
    return doFetch(path, { ...opts, headers: { "X-Overlay": "1", "Content-Type": "application/json", ...opts.headers } });
  }

  // ---- Reviewer picker

  // { key, el, initial: Set, selected: Set, cands, query, busy, error }; a bulk picker
  // (add-only, over selectedPRs) has bulk: true, keys, candsBy and loading instead.
  let picker = null;
  const candCache = {};

  function closePicker() {
    if (!picker) return;
    picker.el.remove();
    picker = null;
    render();
  }

  async function openPicker(key, anchor) {
    if (picker) { const same = !picker.bulk && picker.key === key; closePicker(); if (same) return; }
    const p = data.prs[key];
    const current = new Set(p.requestedReviewers.map(lc));
    const el = document.createElement("div");
    el.className = "picker";
    el.setAttribute("role", "dialog");
    el.setAttribute("aria-label", "Request reviewers");
    root.appendChild(el);
    picker = { key, el, anchor, initial: current, selected: new Set(current), cands: candCache[key], query: "", busy: false, error: "" };
    render(); // marks the button open
    drawPicker(true);
    if (!picker.cands) {
      try {
        const r = await api("/api/reviewers?pr=" + encodeURIComponent(key));
        const body = await r.json();
        if (!r.ok) throw new Error(body.error || r.statusText);
        candCache[key] = body;
        if (picker && picker.key === key) { picker.cands = body; drawPicker(); }
      } catch (e) {
        if (picker && picker.key === key) { picker.error = e.message; drawPicker(); }
      }
    }
  }

  function positionPicker() {
    if (picker.bulk) {
      // Opens upward from the selection bar's button.
      const doc = document.documentElement;
      const r = root.querySelector("[data-bulk]")?.getBoundingClientRect() ?? { right: doc.clientWidth - 16, top: doc.clientHeight - 16 };
      const w = picker.el.offsetWidth, h = picker.el.offsetHeight;
      picker.el.style.left = Math.max(16, Math.min(r.right - w, doc.clientWidth - w - 16)) + window.scrollX - origin().x + "px";
      picker.el.style.top = Math.max(window.scrollY + 8, r.top + window.scrollY - h - 4) - origin().y + "px";
      return;
    }
    const btn = root.querySelector(`[data-reviewers="${CSS.escape(picker.key)}"]`) || picker.anchor;
    const r = btn.getBoundingClientRect();
    const w = picker.el.offsetWidth;
    const left = Math.max(16, Math.min(r.right - w, document.documentElement.clientWidth - w - 16));
    picker.el.style.left = left + window.scrollX - origin().x + "px";
    picker.el.style.top = r.bottom + window.scrollY + 4 - origin().y + "px";
  }

  function pickerChanges() {
    const { initial, selected } = picker;
    const add = [...selected].filter(k => !initial.has(k));
    const remove = [...initial].filter(k => !selected.has(k));
    return { add, remove, count: add.length + remove.length };
  }

  function drawPicker(focus) {
    const { bulk, key, cands, selected, query, busy, error } = picker;
    const p = bulk ? null : data.prs[key];
    const reviewed = bulk ? {} : Object.fromEntries(humanReviews(p).map(r => [lc(r.login), r.state]));
    // Bulk: show "requested" when everyone selected already has this reviewer, or
    // how many PRs a candidate applies to when not all of them.
    const bulkHint = (id, c) => {
      const ps = picker.keys.map(k => data.prs[k]).filter(Boolean);
      if (ps.length && ps.every(x => x.requestedReviewers.map(lc).some(r => r === id))) return "requested";
      return c.n < picker.loaded ? `${c.n} of ${picker.loaded} PRs` : "";
    };
    const q = lc(query.trim());
    const match = c => !q || lc(c.login).includes(q) || lc(c.name || "").includes(q);

    // Selected entries may not be in the candidate list (e.g. a team the token
    // can't list); keep them visible so they can be removed.
    const known = new Set();
    const opt = (id, login, name, hint, text = hint ? REVIEW_TEXT[hint] : "") => {
      known.add(id);
      const kind = hint ? REVIEW_CLASS[hint] : "";
      return `<label class="opt">
        <input type="checkbox" data-id="${esc(id)}" ${selected.has(id) ? "checked" : ""} ${busy ? "disabled" : ""}>
        ${avatar(login)}
        <span class="who">${esc(login.replace(/^@/, ""))}${name ? `<small>${esc(name)}</small>` : ""}</span>
        <span class="hint ${kind}">${esc(text)}</span>
      </label>`;
    };

    let list;
    if (!cands) {
      list = `<div class="picker-empty">${picker.loading || (!bulk && !error) ? "Loading reviewers…" : ""}</div>`;
    } else {
      const users = cands.users.filter(match);
      const teams = cands.teams.filter(match);
      const groups = [
        ["Suggested", users.filter(u => u.suggested).map(u => opt(lc(u.login), u.login, u.name, reviewed[lc(u.login)], bulk ? bulkHint(lc(u.login), u) : undefined))],
        ["People", users.filter(u => !u.suggested).map(u => opt(lc(u.login), u.login, u.name, reviewed[lc(u.login)], bulk ? bulkHint(lc(u.login), u) : undefined))],
        ["Teams", teams.map(t => opt("@" + lc(t.login), "@" + t.login, t.name, null, bulk ? bulkHint("@" + lc(t.login), t) : undefined))],
      ].filter(([, items]) => items.length);
      list = groups.map(([label, items]) => `<div class="picker-group">${label}</div>${items.join("")}`).join("")
        || `<div class="picker-empty">No one matches “${esc(query)}”.</div>`;
    }
    const orphans = bulk ? [] : [...selected].filter(id => cands && !known.has(id) && (!q || id.includes(q)));
    if (orphans.length) list = `<div class="picker-group">Requested</div>${orphans.map(id => opt(id, id, "", null)).join("")}` + list;

    const pills = [...selected].map(id =>
      `<span class="pill">${esc(id)}<button data-unpick="${esc(id)}" aria-label="Remove ${esc(id)}" ${busy ? "disabled" : ""}>×</button></span>`).join("");
    const ch = bulk ? { add: [...selected], remove: [], count: selected.size } : pickerChanges();
    const status = bulk ? (error ? `<span class="status err">${esc(error)}</span>` : busy ? `<span class="status">Requesting…</span>` : `<span class="status">${selected.size} selected</span>`)
      : error ? `<span class="status err">${esc(error)}</span>`
      : busy ? `<span class="status">Saving…</span>`
      : `<span class="status">${ch.count ? `${ch.add.length ? "+" + ch.add.length : ""}${ch.add.length && ch.remove.length ? " " : ""}${ch.remove.length ? "−" + ch.remove.length : ""}` : `${selected.size} selected`}</span>`;

    const listEl = picker.el.querySelector(".picker-list");
    const scroll = listEl ? listEl.scrollTop : 0;
    const hadFocus = activeEl() && picker.el.contains(activeEl()) ? activeEl().dataset.id || "search" : null;
    picker.el.innerHTML = `
      <div class="picker-head">
        <b>Request reviewers <span class="pr">${bulk ? `on ${picker.keys.length} PR${picker.keys.length === 1 ? "" : "s"}` : `${esc(repoLabel(p.repo))} #${p.number}`}</span></b>
        <div class="pills">${pills}</div>
        <input type="search" placeholder="Filter people and teams" value="${esc(query)}" aria-label="Filter reviewers">
      </div>
      <div class="picker-list">${list}</div>
      ${picker.report?.length ? `<div class="picker-report">${picker.report.map(l => `<div${l.err ? ' class="err"' : ""}>${esc(l.text)}</div>`).join("")}</div>` : ""}
      <div class="picker-foot">${status}
        <button class="btn" data-act="cancel">Cancel</button>
        <button class="btn primary" data-act="apply" ${ch.count && !busy && !picker.loading ? "" : "disabled"}>${bulk ? "Request" : "Update"}</button>
      </div>`;
    picker.el.querySelector(".picker-list").scrollTop = scroll;
    const input = picker.el.querySelector("input[type=search]");
    if (focus || hadFocus === "search") { input.focus(); input.setSelectionRange(input.value.length, input.value.length); }
    else if (hadFocus) picker.el.querySelector(`input[data-id="${CSS.escape(hadFocus)}"]`)?.focus();
    positionPicker();
  }

  async function applyPicker() {
    if (picker.bulk) return applyBulk();
    const ch = pickerChanges();
    if (!ch.count || picker.busy) return;
    const split = ids => ({ users: ids.filter(i => !i.startsWith("@")), teams: ids.filter(i => i.startsWith("@")).map(i => i.slice(1)) });
    // Send the logins with their original casing where we know it.
    const cased = id => {
      const c = picker.cands;
      const hit = id.startsWith("@") ? c?.teams.find(t => "@" + lc(t.login) === id) : c?.users.find(u => lc(u.login) === id);
      return hit ? (id.startsWith("@") ? "@" + hit.login : hit.login) : id;
    };
    const key = picker.key;
    picker.busy = true; picker.error = ""; drawPicker();
    try {
      const r = await api("/api/reviewers", { method: "POST",
        body: JSON.stringify({ changes: [{ pr: key, add: split(ch.add.map(cased)), remove: split(ch.remove.map(cased)) }] }) });
      const body = await r.json();
      if (!r.ok) throw new Error(body.error || r.statusText);
      const res = body.results[0];
      if (res.error) throw new Error(res.error);
      if (data.prs[key] && res.pr) data.prs[key] = res.pr;
      if (res.ignored.length) {
        // GitHub accepted the call but skipped these; stay open and say so.
        const now = new Set((res.pr || data.prs[key]).requestedReviewers.map(lc));
        Object.assign(picker, { busy: false, initial: now, selected: new Set(now),
          error: `GitHub skipped ${res.ignored.join(", ")}. They may lack access to this repo.` });
        render(); drawPicker();
        return;
      }
      closePicker();
    } catch (e) {
      if (picker && picker.key === key) { picker.busy = false; picker.error = e.message; drawPicker(); }
    }
  }

  const prName = (p, key) => p ? repoLabel(p.repo) + " #" + p.number : key;

  // Union of the selected PRs' candidates. n counts the PRs a candidate is valid for.
  function bulkCands() {
    const users = new Map(), teams = new Map();
    let loaded = 0;
    for (const k of picker.keys) {
      const c = picker.candsBy[k];
      if (!c) continue;
      loaded++;
      for (const [map, list] of [[users, c.users], [teams, c.teams]]) for (const x of list) {
        const e = map.get(lc(x.login));
        if (e) { e.n++; e.suggested ||= x.suggested; e.name ||= x.name; }
        else map.set(lc(x.login), { ...x, n: 1 });
      }
    }
    picker.loaded = loaded;
    const byLogin = (a, b) => lc(a.login).localeCompare(lc(b.login));
    return loaded ? { users: [...users.values()].sort(byLogin), teams: [...teams.values()].sort(byLogin) } : null;
  }

  async function openBulk(anchor) {
    if (picker) { const same = picker.bulk; closePicker(); if (same) return; }
    const keys = [...selectedPRs].filter(k => data.prs[k]);
    if (!keys.length) return;
    const el = document.createElement("div");
    el.className = "picker";
    el.setAttribute("role", "dialog");
    el.setAttribute("aria-label", "Request reviewers");
    root.appendChild(el);
    const me = picker = { bulk: true, keys, key: keys[0], el, anchor, initial: new Set(), selected: new Set(), candsBy: {}, loaded: 0,
      cands: null, loading: true, query: "", busy: false, error: "", report: [] };
    drawPicker(true);
    const failed = [];
    await Promise.all(keys.map(async key => {
      try {
        if (!candCache[key]) {
          const r = await api("/api/reviewers?pr=" + encodeURIComponent(key));
          const body = await r.json();
          if (!r.ok) throw new Error(body.error || r.statusText);
          candCache[key] = body;
        }
        me.candsBy[key] = candCache[key];
      } catch (e) { failed.push(`${prName(data.prs[key], key)}: ${e.message}`); }
    }));
    if (picker !== me) return;
    me.loading = false;
    me.cands = bulkCands();
    if (failed.length) me.error = `Couldn't load reviewers for ${failed.join("; ")}`;
    drawPicker();
  }

  async function applyBulk() {
    if (picker.busy || picker.loading || !picker.selected.size) return;
    const me = picker, report = [], changes = [];
    const split = ids => ({ users: ids.filter(i => !i.startsWith("@")), teams: ids.filter(i => i.startsWith("@")).map(i => i.slice(1)) });
    for (const key of me.keys) {
      const p = data.prs[key], c = me.candsBy[key];
      if (!p) continue;
      if (!c) { report.push({ text: `skipped ${prName(p)} (reviewers not loaded)`, err: true }); continue; }
      // Send each id with its original casing, and only the ones this PR accepts.
      const users = new Map(c.users.map(u => [lc(u.login), u.login])), teams = new Map(c.teams.map(t => ["@" + lc(t.login), "@" + t.login]));
      const ids = [], noUser = [], noTeam = [];
      for (const id of me.selected) {
        const hit = (id.startsWith("@") ? teams : users).get(id);
        if (hit) ids.push(hit); else (id.startsWith("@") ? noTeam : noUser).push(id);
      }
      if (noUser.length) report.push({ text: `skipped ${noUser.join(", ")} on ${prName(p)} (not a collaborator)`, err: false });
      if (noTeam.length) report.push({ text: `skipped ${noTeam.join(", ")} on ${prName(p)} (team not available)`, err: false });
      if (ids.length) changes.push({ pr: key, add: split(ids), remove: { users: [], teams: [] } });
    }
    me.busy = true; me.error = ""; me.report = []; drawPicker();
    try {
      if (changes.length) {
        const r = await api("/api/reviewers", { method: "POST", body: JSON.stringify({ changes }) });
        const body = await r.json();
        if (!r.ok) throw new Error(body.error || r.statusText);
        body.results.forEach((res, i) => {
          const key = changes[i].pr, p = data.prs[key];
          if (res.error) { report.push({ text: `${prName(p, key)}: ${res.error}`, err: true }); return; }
          if (res.pr && p) data.prs[key] = res.pr;
          selectedPRs.delete(key); // done; a retry only hits the failures
          if (res.ignored.length) report.push({ text: `${prName(p, key)}: GitHub skipped ${res.ignored.join(", ")}`, err: true });
        });
      }
      if (!report.length) { selectedPRs.clear(); if (picker === me) closePicker(); else render(); return; }
      if (picker !== me) { render(); return; }
      me.keys = me.keys.filter(k => selectedPRs.has(k));
      Object.assign(me, { busy: false, report, selected: new Set(), query: "" });
      me.cands = bulkCands();
      render(); drawPicker();
    } catch (e) {
      if (picker === me) { me.busy = false; me.error = e.message; drawPicker(); }
    }
  }

  // Row checkboxes, bundle Select buttons and the selection bar.
  function changeSelection(el) {
    const d = el.dataset;
    if (d.sel) el.checked ? selectedPRs.add(d.sel) : selectedPRs.delete(d.sel);
    else if (d.selkeys) {
      const keys = d.selkeys.split(","), all = keys.every(k => selectedPRs.has(k));
      keys.forEach(k => all ? selectedPRs.delete(k) : selectedPRs.add(k));
    } else if (d.bar === "all") shownSel.forEach(k => selectedPRs.add(k));
    else if (d.bar === "clear") selectedPRs.clear();
    // Clicking outside closes the picker, as anywhere else; closing also redraws.
    picker ? closePicker() : render();
  }

  on(document, "click", e => {
    const btn = evt(e).closest("[data-reviewers]");
    if (btn) { openPicker(btn.dataset.reviewers, btn); return; }
    const bar = evt(e).closest("[data-bar=request]");
    if (bar) { openBulk(bar); return; }
    const sel = evt(e).closest("[data-sel], [data-selkeys], [data-bar]");
    if (sel) { changeSelection(sel); return; }
    if (!picker) return;
    if (!picker.el.contains(evt(e))) { closePicker(); return; }
    const act = evt(e).closest("[data-act]")?.dataset.act;
    if (act === "cancel") closePicker();
    if (act === "apply") applyPicker();
    const un = evt(e).closest("[data-unpick]");
    if (un && !picker.busy) { picker.selected.delete(un.dataset.unpick); drawPicker(); }
  });
  on(document, "change", e => {
    if (!picker || !picker.el.contains(evt(e)) || !evt(e).dataset.id) return;
    const id = evt(e).dataset.id;
    evt(e).checked ? picker.selected.add(id) : picker.selected.delete(id);
    drawPicker();
  });
  on(document, "input", e => {
    if (!picker || evt(e).type !== "search" || !picker.el.contains(evt(e))) return;
    picker.query = evt(e).value; drawPicker();
  });
  on(document, "keydown", e => {
    if (!picker) return;
    if (e.key === "Escape") { closePicker(); return; }
    if (e.key === "Enter" && evt(e).type === "search") {
      e.preventDefault();
      // Enter in the filter toggles the top match; Cmd/Ctrl+Enter saves.
      if (e.metaKey || e.ctrlKey) { applyPicker(); return; }
      const first = picker.el.querySelector(".picker-list input[type=checkbox]:not(:disabled)");
      if (first) { const id = first.dataset.id; picker.selected.has(id) ? picker.selected.delete(id) : picker.selected.add(id); picker.query = ""; drawPicker(); }
    }
  }, true);
  on(window, "resize", () => picker && positionPicker());

  async function load(force) {
    const btn = $("refresh");
    if (force) btn.classList.add("spin");
    try {
      const r = await api("/api/prs" + (force ? "?refresh=1" : ""));
      if (!r.ok) {
        // A 403 here means this tab's code is older than the server's.
        throw new Error(r.status === 403 ? "the server was updated; reload this page" : `HTTP ${r.status}`);
      }
      data = await r.json();
    } catch (e) {
      data = data || { prs: {}, sections: {}, bundles: {} };
      data.error = e instanceof TypeError ? (opts.unreachable || "Overlay server unreachable") : "Overlay: " + e.message;
    }
    btn.classList.remove("spin");
    render();
    eggs.loaded(force);
  }

  $("tabs").addEventListener("click", e => {
    const b = evt(e).closest("[data-tab]"); if (!b) return;
    if (b.dataset.tab !== tab) selectedPRs.clear();
    tab = b.dataset.tab; store.set("tab", tab); render(); eggs.switched();
  });
  $("filters").addEventListener("click", e => {
    const b = evt(e).closest("[data-filter]"); if (!b) return;
    filter = b.dataset.filter; store.set("filter", filter); render(); eggs.switched();
  });
  $("orgs").addEventListener("click", e => {
    const b = evt(e).closest("[data-org]"); if (!b) return;
    const o = b.dataset.org;
    if (!o) orgs.clear(); else orgs.has(o) ? orgs.delete(o) : orgs.add(o);
    store.set("orgs", JSON.stringify([...orgs]));
    render();
  });
  $("refresh").addEventListener("click", () => load(true));
  $("list").addEventListener("click", e => {
    const n = evt(e).closest("[data-rename]");
    if (n) {
      const d = n.dataset;
      editing = { gid: d.rename, keys: d.keys.split(","), auto: d.auto, draft: d.manual, drawn: false };
      render();
      return;
    }
    const b = evt(e).closest("[data-expand]"); if (!b) return;
    const id = b.dataset.expand;
    expanded.has(id) ? expanded.delete(id) : expanded.add(id);
    render();
  });
  ;(() => {
    const list = $("list");
    list.addEventListener("input", e => { if (editing && evt(e).matches(".name-input")) editing.draft = evt(e).value; });
    list.addEventListener("focusout", e => { if (editing && evt(e).matches(".name-input")) saveName(); });
    list.addEventListener("keydown", e => {
      if (!editing || !evt(e).matches(".name-input")) return;
      if (e.key === "Enter") { e.preventDefault(); evt(e).blur(); } // blur saves
      else if (e.key === "Escape") { e.stopPropagation(); editing = null; render(); }
    });
  })();
  on(document, "keydown", e => {
    if (e.key === "r" && !e.metaKey && !e.ctrlKey && !picker && evt(e) === document.body) load(true);
  });

  // ---- Easter eggs. Rare on purpose; ?egg=wave|roll|tumbleweed|disco shows one.

  const eggs = (() => {
    const chance = n => Math.random() < 1 / n;
    const calm = () => matchMedia("(prefers-reduced-motion: reduce)").matches;
    let shown = new URLSearchParams(location.search).get("egg");

    function toast(text) {
      const el = document.createElement("div");
      el.className = "egg-toast";
      el.textContent = text;
      root.appendChild(el);
      setTimeout(() => el.remove(), 2500);
    }

    // Rows bob up one after another, top to bottom.
    function wave() {
      const rows = root.querySelectorAll(".row");
      rows.forEach((r, i) => r.style.setProperty("--i", i));
      $("list").classList.add("wave");
      setTimeout(() => $("list").classList.remove("wave"), 600 + rows.length * 40);
    }

    function roll() {
      $("list").classList.add("roll");
      toast("do a barrel roll");
      setTimeout(() => $("list").classList.remove("roll"), 1100);
    }

    // A tumbleweed crosses a PR nobody has touched in two weeks.
    function tumbleweed() {
      const stale = [...root.querySelectorAll(".row")].filter(r => /^(1[4-9]|[2-9]\d|\d{3,})d$/.test(r.querySelector(".age")?.textContent));
      const row = stale[Math.floor(Math.random() * stale.length)];
      if (!row) return;
      const el = document.createElement("span");
      el.className = "tumbleweed";
      el.style.setProperty("--dist", row.offsetWidth + 32 + "px");
      el.innerHTML = `<svg viewBox="0 0 24 24" width="24" height="24" fill="none" stroke="#b08850" stroke-width="1.4" stroke-linecap="round">
        <circle cx="12" cy="12" r="9"/><path d="M5 9c4 1 9-3 14 1M4 14c5-2 11 2 16-1M8 4c1 5-2 11 2 16M15 4c-2 5 2 11-1 16M6 18c3-4 9-8 12-12"/></svg>`;
      row.appendChild(el);
      setTimeout(() => el.remove(), 2600);
    }

    // Avatar rings and connected cards party for ten seconds.
    function disco() {
      if (root.classList.contains("disco")) return;
      root.classList.add("disco");
      toast("🕺 disco mode");
      setTimeout(() => root.classList.remove("disco"), 10000);
    }

    const all = { wave, roll, tumbleweed, disco };
    const KONAMI = ["ArrowUp", "ArrowUp", "ArrowDown", "ArrowDown", "ArrowLeft", "ArrowRight", "ArrowLeft", "ArrowRight", "b", "a"];
    let pos = 0;
    on(document, "keydown", e => {
      if (/^(INPUT|TEXTAREA|SELECT)$/.test(evt(e).tagName)) return;
      pos = e.key === KONAMI[pos] ? pos + 1 : e.key === KONAMI[0] ? 1 : 0;
      if (pos === KONAMI.length) { pos = 0; if (!calm()) disco(); }
    });

    return {
      loaded(force) {
        if (calm() || !data?.fetchedAt) return;
        if (shown && all[shown]) { all[shown](); shown = null; return; }
        if (!force) return;
        if (chance(50)) roll();
        else if (chance(30)) wave();
      },
      switched() {
        if (!calm() && chance(20)) tumbleweed();
      },
    };
  })();

  load();
  // A hidden tab stops asking, so the server sees nobody looking and stops
  // polling GitHub; showing it again catches up at once.
  timers.push(setInterval(() => document.hidden || load(), 30000));
  on(document, "visibilitychange", () => document.hidden || load());
  timers.push(setInterval(render, 30000)); // keep relative times fresh

  return {
    destroy() {
      timers.forEach(clearInterval);
      listeners.forEach(([el, type, fn, capture]) => el.removeEventListener(type, fn, capture));
      root.innerHTML = "";
      root.classList.remove("gho", "sel");
    },
  };
}

globalThis.GithubOverlay = { mount };
})();
