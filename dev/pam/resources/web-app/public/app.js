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
