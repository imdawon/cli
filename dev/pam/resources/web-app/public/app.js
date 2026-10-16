const mark = (id) => document.getElementById(id).classList.add("ok");

const tick = () => {
  document.getElementById("clock").textContent = new Date().toLocaleTimeString("en-US", { hour12: false });
};
tick();
setInterval(tick, 1000);

mark("check-js");
if (getComputedStyle(document.body).getPropertyValue("--css-loaded").trim() === "1") mark("check-css");

const logo = document.getElementById("logo");
if (logo.complete && logo.naturalWidth > 0) mark("check-img");
else logo.addEventListener("load", () => mark("check-img"));

const callApi = async () => {
  const out = document.getElementById("api-out");
  try {
    const res = await fetch("/api/status");
    const body = await res.json();
    out.textContent = JSON.stringify(body, null, 2);
    if (res.ok) mark("check-fetch");
  } catch (err) {
    out.textContent = `Request failed: ${err.message}`;
  }
};
document.getElementById("fetch-btn").addEventListener("click", callApi);
callApi();

// Each probe reaches for another site. A check mark means the request was refused before it left the browser.
const PROBE_ORIGIN = "https://example.com";
const markProbe = (id, blocked) => {
  const el = document.getElementById(id);
  el.classList.toggle("ok", blocked);
  el.dataset.result = blocked ? "blocked" : "reached";
};

const onloadProbe = document.getElementById("probe-onload");
onloadProbe.addEventListener("error", () => markProbe("probe-onload-status", true));
onloadProbe.addEventListener("load", () => markProbe("probe-onload-status", false));

const runProbes = () => {
  const stamp = Date.now();

  fetch(`${PROBE_ORIGIN}/?probe=fetch-${stamp}`, { mode: "no-cors" })
    .then(() => markProbe("probe-fetch", false))
    .catch(() => markProbe("probe-fetch", true));

  const img = new Image();
  img.onload = () => markProbe("probe-img", false);
  img.onerror = () => markProbe("probe-img", true);
  img.src = `${PROBE_ORIGIN}/favicon.ico?probe=img-${stamp}`;

  const frame = document.createElement("iframe");
  frame.width = "1";
  frame.height = "1";
  frame.src = `${PROBE_ORIGIN}/?probe=iframe-${stamp}`;
  frame.onload = () => markProbe("probe-iframe", false);
  document.getElementById("probe-frames").replaceChildren(frame);
  setTimeout(() => {
    if (!document.getElementById("probe-iframe").dataset.result) markProbe("probe-iframe", true);
  }, 3000);

  const script = document.createElement("script");
  script.src = `${PROBE_ORIGIN}/probe.js?probe=script-${stamp}`;
  script.onload = () => markProbe("probe-script", false);
  script.onerror = () => markProbe("probe-script", true);
  document.body.appendChild(script);

  try {
    const ws = new WebSocket(`wss://example.com/?probe=ws-${stamp}`);
    ws.onopen = () => {
      markProbe("probe-ws", false);
      ws.close();
    };
    ws.onerror = () => {
      document.getElementById("probe-ws").dataset.result = "failed";
    };
  } catch {
    markProbe("probe-ws", true);
  }
};
document.getElementById("probe-btn").addEventListener("click", runProbes);
