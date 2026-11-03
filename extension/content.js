// Replaces GitHub's pull request inbox with the overlay dashboard. Runs on all
// of github.com because GitHub navigates without page loads.
const INBOX = "/pulls/inbox";
const OFF_KEY = "gho:off"; // set when the user switched back to GitHub's view

let host = null, ui = null, lastPath = null;

const isInbox = () => location.pathname === INBOX || location.pathname.startsWith(INBOX + "/");
const showingGitHub = () => { try { return localStorage.getItem(OFF_KEY) === "1"; } catch { return false; } };

// fetch() for the dashboard, relayed through the background worker. A
// TypeError means unreachable, as with fetch.
function relayFetch(path, init = {}) {
  return new Promise((resolve, reject) => {
    const fail = msg => reject(new TypeError(msg));
    try {
      chrome.runtime.sendMessage({ type: "api", path, method: init.method, headers: init.headers, body: init.body }, res => {
        if (chrome.runtime.lastError) return fail(chrome.runtime.lastError.message);
        if (!res || res.error) return fail(res?.error || "no response");
        resolve(new Response(res.body, { status: res.status, statusText: res.statusText }));
      });
    } catch (e) {
      fail(e.message); // the extension was reloaded; this page needs a reload too
    }
  });
}

function build() {
  host = document.createElement("div");
  host.id = "gho-host";
  const shadow = host.attachShadow({ mode: "open" });
  let pending = 2;
  for (const file of ["ui.css", "github.css"]) {
    const link = document.createElement("link");
    link.rel = "stylesheet";
    link.href = chrome.runtime.getURL(file);
    link.onload = link.onerror = () => { if (!--pending) host.classList.add("loaded"); };
    shadow.append(link);
  }
  const bar = document.createElement("div");
  bar.className = "switch";
  bar.innerHTML = `<button type="button"></button>`;
  bar.firstChild.addEventListener("click", () => {
    try { localStorage.setItem(OFF_KEY, showingGitHub() ? "0" : "1"); } catch {}
    apply();
  });
  const root = document.createElement("div");
  root.className = "on-github";
  shadow.append(bar, root);
  ui = GithubOverlay.mount(root, {
    fetch: relayFetch,
    linkTarget: "_self",
    storagePrefix: "gho:",
    unreachable: "Can't reach the overlay server at prs.localhost:7777 (run `github-overlay start`)",
  });
}

// Show or hide the dashboard to match the current URL and the user's choice.
function apply() {
  const active = isInbox();
  if (active && !host) build();
  if (!active && host) { ui.destroy(); host.remove(); host = ui = null; }
  if (host) {
    // React may replace <main>; keep the host inside whichever one is current.
    const container = document.querySelector("main") || document.body;
    if (host.parentNode !== container) container.prepend(host);
    const off = showingGitHub();
    host.shadowRoot.querySelector(".switch button").textContent = off ? "Show PR overlay" : "Show GitHub's inbox";
    host.shadowRoot.querySelector(".gho").hidden = off;
  }
  document.documentElement.classList.toggle("gho-active", active && !!host && !showingGitHub());
}

function tick() {
  if (location.pathname !== lastPath || (host && !host.isConnected)) {
    lastPath = location.pathname;
    apply();
  }
}

apply();
lastPath = location.pathname;
document.addEventListener("turbo:load", tick);
window.navigation?.addEventListener("navigatesuccess", tick);
setInterval(tick, 500);
