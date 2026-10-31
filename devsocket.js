// The page's side of the development socket: one WebSocket however many
// scripts listen, not an HTTP stream each, which a few tabs of would leave the
// browser no connections for anything else. Opened by the first listener; it
// comes back after the server restarts, as the channels' current values.
window.__vukaDev || (window.__vukaDev = (() => {
  const listeners = {}, last = {}, downs = [];
  let ws = null, timer = 0, delay = 100;
  function connect() {
    clearTimeout(timer);
    timer = 0;
    ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/_vuka/dev");
    ws.onopen = () => { delay = 100; };
    ws.onmessage = (e) => {
      const { c, v } = JSON.parse(e.data);
      last[c] = v;
      for (const f of listeners[c] || []) call(f, v);
    };
    ws.onclose = () => {
      ws = null;
      for (const f of downs) call(f);
      timer = setTimeout(connect, delay / 2 + Math.random() * delay / 2);
      delay = Math.min(delay * 2, 5000);
    };
  }
  function call(f, v) {
    try { f(v); } catch (err) { console.error("vuka:", err); }
  }
  // A background tab's timers run late: back in view, it doesn't wait for one.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible" && timer) connect();
  });
  return {
    // on calls f with each value of the channel, the one it has first; down,
    // when given, each time the connection is lost.
    on(channel, f, down) {
      (listeners[channel] ||= []).push(f);
      if (down) downs.push(down);
      if (channel in last) Promise.resolve().then(() => call(f, last[channel]));
      if (!ws && !timer) connect();
    },
  };
})());
