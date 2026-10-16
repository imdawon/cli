#!/usr/bin/env node
// "Acme Internal Admin": a stand-in internal web app for testing PAM web app sessions.
// One page that exercises navigation, form submission (POST + redirect), asset loading (CSS, JS, SVG,
// fetch), and shows what the app received (so credential injection is visible later).
// Compose publishes it on 127.0.0.1 only: like a real internal app, nothing off this host can reach it.
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
const PORT = Number(process.env.PORT || 3001);
const HOST = process.env.HOST || "127.0.0.1";

const submissions = [];
let requestCount = 0;

const ASSETS = {
  "/assets/style.css": "text/css",
  "/assets/app.js": "text/javascript",
  "/assets/logo.svg": "image/svg+xml"
};

const VIEWS = {
  overview: "Overview",
  orders: "Orders",
  settings: "Settings"
};

const esc = (s) =>
  String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

const readBody = (req) =>
  new Promise((resolve, reject) => {
    let size = 0;
    const chunks = [];
    req.on("data", (c) => {
      size += c.length;
      if (size > 64 * 1024) {
        reject(new Error("body too large"));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });

const describeAuth = (req) => {
  const auth = req.headers.authorization;
  if (!auth) return "none";
  if (auth.startsWith("Basic ")) {
    const user = Buffer.from(auth.slice(6), "base64").toString("utf8").split(":")[0];
    return `Basic (user: ${user})`;
  }
  if (auth.startsWith("Bearer ")) return "Bearer token";
  return "present";
};

const renderPage = (req, url) => {
  const view = VIEWS[url.searchParams.get("view")] ? url.searchParams.get("view") : "overview";
  const submitted = url.searchParams.get("submitted") === "1";
  const nav = Object.entries(VIEWS)
    .map(([key, label]) => `<a href="/?view=${key}" class="${key === view ? "active" : ""}">${label}</a>`)
    .join("");
  const rows = submissions
    .slice(-6)
    .reverse()
    .map(
      (s) =>
        `<tr><td>${esc(s.at)}</td><td>${esc(s.customer)}</td><td>${esc(s.priority)}</td><td>${esc(s.note)}</td></tr>`
    )
    .join("");

  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Acme Internal Admin · ${VIEWS[view]}</title>
  <link rel="stylesheet" href="/assets/style.css">
</head>
<body>
  <header>
    <img src="/assets/logo.svg" alt="Acme" id="logo" width="36" height="36">
    <div>
      <h1>Acme Internal Admin</h1>
      <p class="sub">Private network only · ${esc(req.headers.host || "")}</p>
    </div>
    <div id="clock" class="clock">--:--:--</div>
  </header>

  <nav>${nav}</nav>

  <main>
    <section class="card">
      <h2>${VIEWS[view]}</h2>
      <p>You are on the <strong>${VIEWS[view]}</strong> view. Each tab above is a full page load, so back and forward work.</p>
      <p><a href="#orders-form">Jump to the order form</a> · <a href="https://example.com" id="external">External link (example.com)</a></p>
    </section>

    <section class="card">
      <h2>Asset loading</h2>
      <ul class="checks">
        <li id="check-css">Stylesheet</li>
        <li id="check-js">Script</li>
        <li id="check-img">Image</li>
        <li id="check-fetch">API call</li>
      </ul>
      <button id="fetch-btn" type="button">Call /api/status</button>
      <pre id="api-out">(not called yet)</pre>
    </section>

    <section class="card" id="orders-form">
      <h2>Create order</h2>
      ${submitted ? '<p class="ok">Order saved. The page reloaded after a POST and redirect.</p>' : ""}
      <form method="POST" action="/orders">
        <label>Customer <input name="customer" required maxlength="60" placeholder="Globex"></label>
        <label>Priority
          <select name="priority"><option>low</option><option selected>normal</option><option>high</option></select>
        </label>
        <label>Note <input name="note" maxlength="120" placeholder="Ship by Friday"></label>
        <button type="submit">Submit order</button>
      </form>
      <table>
        <thead><tr><th>Time</th><th>Customer</th><th>Priority</th><th>Note</th></tr></thead>
        <tbody>${rows || '<tr><td colspan="4" class="empty">No orders yet</td></tr>'}</tbody>
      </table>
    </section>

    <section class="card">
      <h2>What this app received</h2>
      <dl>
        <dt>Authorization</dt><dd id="auth">${esc(describeAuth(req))}</dd>
        <dt>Cookie</dt><dd>${esc(req.headers.cookie ? "present" : "none")}</dd>
        <dt>User agent</dt><dd class="ua">${esc(req.headers["user-agent"] || "")}</dd>
        <dt>Requests served</dt><dd>${requestCount}</dd>
      </dl>
    </section>
  </main>

  <script src="/assets/app.js"></script>
</body>
</html>`;
};

const server = http.createServer(async (req, res) => {
  requestCount += 1;
  const url = new URL(req.url, `http://${req.headers.host || `${HOST}:${PORT}`}`);
  console.log(`${new Date().toISOString()} ${req.method} ${url.pathname}${url.search}`);

  try {
    if (req.method === "GET" && url.pathname === "/") {
      res.writeHead(200, { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" });
      res.end(renderPage(req, url));
      return;
    }

    if (req.method === "GET" && ASSETS[url.pathname]) {
      const file = path.join(HERE, "public", path.basename(url.pathname));
      res.writeHead(200, { "Content-Type": ASSETS[url.pathname], "Cache-Control": "no-store" });
      fs.createReadStream(file).pipe(res);
      return;
    }

    if (req.method === "POST" && url.pathname === "/orders") {
      const form = new URLSearchParams(await readBody(req));
      submissions.push({
        at: new Date().toLocaleTimeString("en-US", { hour12: false }),
        customer: (form.get("customer") || "").slice(0, 60),
        priority: ["low", "normal", "high"].includes(form.get("priority")) ? form.get("priority") : "normal",
        note: (form.get("note") || "").slice(0, 120)
      });
      res.writeHead(303, { Location: "/?submitted=1#orders-form" });
      res.end();
      return;
    }

    if (req.method === "GET" && url.pathname === "/api/status") {
      res.writeHead(200, { "Content-Type": "application/json", "Cache-Control": "no-store" });
      res.end(
        JSON.stringify({ ok: true, serverTime: new Date().toISOString(), orders: submissions.length, requestCount })
      );
      return;
    }

    res.writeHead(404, { "Content-Type": "text/plain" });
    res.end("Not found");
  } catch (err) {
    res.writeHead(400, { "Content-Type": "text/plain" });
    res.end(String(err.message || err));
  }
});

server.listen(PORT, HOST, () => console.log(`Acme Internal Admin listening on http://${HOST}:${PORT}`));
