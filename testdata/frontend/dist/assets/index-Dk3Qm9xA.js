// A built single-page app, written by hand so the tests need no Node: client-side routes, links that push history, and a call to the app's JSON API.
const app = document.getElementById("app");
const routes = {
  "/": () => `<h1>Home</h1><p id="api">…</p>`,
  "/about": () => `<h1>About</h1>`,
};
function render() {
  const p = location.pathname;
  const pet = p.match(/^\/pets\/(\d+)$/);
  const view = routes[p] || (pet ? () => `<h1>Pet ${pet[1]}</h1>` : () => `<h1>Not found</h1>`);
  app.innerHTML = `<nav><a href="/">Home</a> <a href="/about">About</a> <a href="/pets/7">Pet 7</a></nav><main>${view()}</main>`;
  app.dataset.route = p;
  app.dataset.renders = String(Number(app.dataset.renders || 0) + 1);
  const api = document.getElementById("api");
  if (api) fetch("/api/hello").then((r) => r.json()).then((d) => { api.textContent = d.message; });
}
document.addEventListener("click", (e) => {
  const a = e.target.closest("a");
  if (!a || a.origin !== location.origin) return;
  e.preventDefault();
  history.pushState(null, "", a.pathname);
  render();
});
addEventListener("popstate", render);
window.__env = globalThis.__VUKA_ENV__;
render();
