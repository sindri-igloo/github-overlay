// Content scripts can't reach the local server across origins, so they send
// their /api/ requests here. Only this extension's content scripts can message
// this worker; web pages can't.
const SERVER = "http://prs.localhost:7777";

chrome.runtime.onMessage.addListener((msg, sender, send) => {
  if (msg?.type !== "api" || sender.id !== chrome.runtime.id || !/^\/api\//.test(msg.path)) return;
  fetch(SERVER + msg.path, { method: msg.method || "GET", headers: msg.headers, body: msg.body })
    .then(async r => send({ status: r.status, statusText: r.statusText, body: await r.text() }),
      e => send({ error: e.message }));
  return true; // respond asynchronously
});
