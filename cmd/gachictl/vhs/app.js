// The VHS deck: a position on the tape, what the cluster held there, and
// what changed as the tape plays.
"use strict";

const token = document.querySelector('meta[name="gachi-token"]').content;
const $ = (id) => document.getElementById(id);
const state = {
  info: null,      // the tape: from/current revisions and times
  at: null,        // the position, ms; null is live
  playing: false,
  last: null,      // the revision shown last, for the chat
  timer: null,
};

function store(k, v) { try { localStorage.setItem(k, v); } catch (_) {} }
function load(k) { try { return localStorage.getItem(k); } catch (_) { return null; } }

function setTheme(t) {
  document.documentElement.dataset.theme = t;
  $("theme").textContent = t === "dungeon" ? "GYM" : "DUNGEON";
  store("theme", t);
}
// The gym is where one starts; the dungeon is for those who ask.
setTheme(load("theme") || "gym");
$("theme").onclick = () => setTheme(document.documentElement.dataset.theme === "dungeon" ? "gym" : "dungeon");

async function api(path, params = {}) {
  const q = new URLSearchParams(Object.entries(params).filter(([, v]) => v !== "" && v != null));
  const r = await fetch(`/api/tape/${path}?${q}`);
  if (!r.ok) throw new Error((await r.text()).trim());
  return r.json();
}

function iso(ms) { return new Date(ms).toISOString().replace(/\.\d+Z$/, "Z"); }
function hms(ms) { return new Date(ms).toLocaleString(undefined, { hour12: false }); }
function filters() { return { namespace: $("ns").value.trim(), kind: $("kind").value.trim() }; }
function say(msg) { $("status").textContent = msg || ""; }

function kindOf(o) { return o.object ? o.object.kind : o.resource; }
function nsOf(o) { return (o.object && o.object.metadata && o.object.metadata.namespace) || ""; }
function nameOf(o) { return (o.object && o.object.metadata && o.object.metadata.name) || o.key; }
function groupOf(o) {
  const av = (o.object && o.object.apiVersion) || "";
  return av.includes("/") ? av.split("/")[0] : "";
}

// The position: live, or a moment within the tape.
function position() { return state.at == null ? "" : iso(state.at); }

async function refreshInfo() {
  state.info = await api("info");
  $("from").textContent = state.info.fromTime ? hms(state.info.fromTime) : "rev " + state.info.from;
  $("now").textContent = "now";
}

// tapeStart is the oldest moment the bar reaches: the tape's start, or the
// last hour before the first archiving compaction gives it one.
function tapeStart() {
  const i = state.info;
  return (i && i.fromTime) || Date.now() - 3600000;
}

function ago(ms) {
  const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m " + (s % 60) + "s";
  return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m";
}

const modes = { play: "PLAY ▶", rew: "◀◀ REW", ff: "▶▶ FF" };

function syncTimeline() {
  const now = Date.now();
  const at = state.at == null ? now : state.at;
  const from = tapeStart();
  if (!state.dragging) $("timeline").value = Math.round(((at - from) / (now - from)) * 1000);
  $("from").textContent = hms(from);
  $("clock").textContent = hms(at);
  $("mode").textContent = state.at == null ? "LIVE ●" : modes[state.motion] || "PAUSE ❚❚";
  document.documentElement.dataset.paused = state.motion || state.at == null ? "0" : "1";
  const where = $("where");
  if (state.at == null) {
    where.className = "where live";
    where.textContent = "● LIVE — the cluster as it is now";
  } else {
    where.className = "where past";
    where.textContent = `◀◀ THE PAST: ${hms(at)}, ${ago(at)} ago` + (state.rev ? ` — revision ${state.rev}` : "") + " — ⏏ LIVE to come back";
  }
  for (const btn of document.querySelectorAll(".buttons button")) {
    btn.classList.toggle("on", btn.dataset.act === state.motion || (btn.dataset.act === "live" && state.at == null));
  }
}

async function render() {
  if (state.busy) { state.again = true; return; }
  state.busy = true;
  syncTimeline();
  try {
    const f = filters();
    const [now, diff] = await Promise.all([
      api("objects", { ...f, at: position() }),
      state.at == null ? null : api("diff", { ...f, from: position() }),
    ]);
    $("rev").textContent = now.revision;
    state.rev = state.at == null ? null : now.revision;
    const later = new Map();
    let ghosts = [];
    if (diff) {
      const show = (c) => $("system").checked || !isSystem(c.to || c.from);
      diff.removed.forEach((c) => later.set(c.key, "gone"));
      diff.changed.forEach((c) => later.set(c.key, "chg"));
      ghosts = diff.added.filter(show).map((c) => ({ ...c.to, later: true }));
      $("since").textContent = `From this moment to now: † ${diff.removed.filter(show).length} gone · ~ ${diff.changed.filter(show).length} changed · + ${ghosts.length} came later`;
    } else {
      $("since").textContent = "";
    }
    syncTimeline();
    drawFloor(now.objects.concat(ghosts), later);
    if (state.last != null && state.last !== now.revision) await chatBetween(state.last, now.revision);
    state.last = now.revision;
    say("");
  } catch (e) {
    // The tape may have been cut since it was last asked about (its
    // retention): the position moves to its new start, and a running
    // tape runs on from there.
    if (/knows no revision|older than the tape|Requested Range/i.test(e.message)) {
      await refreshInfo();
      // Live, only the chat was behind: it starts again from now.
      state.last = null;
      if (state.at != null) {
        state.at = Math.ceil(tapeStart() / 1000) * 1000;
        say("The tape was cut by its retention: it starts at " + hms(tapeStart()) + " now.");
      }
      state.again = true;
      return;
    }
    say("Oh shit, I'm sorry: " + e.message);
  } finally {
    state.busy = false;
    if (state.again) { state.again = false; render(); }
  }
}

// What the cluster runs for itself: hidden unless asked for.
const systemKinds = new Set(["APIService", "Event", "Lease", "Endpoints", "EndpointSlice", "FlowSchema",
  "PriorityLevelConfiguration", "PriorityClass", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding",
  "RangeAllocation", "IPAddress", "ServiceCIDR", "ClusterTrustBundle"]);
// The API server's own bookkeeping, stored beside the objects.
const systemKeys = ["/registry/masterleases/", "/registry/peerserverleases/", "/registry/ranges/"];
function isSystem(o) {
  const k = kindOf(o), n = nameOf(o), ns = nsOf(o);
  return systemKinds.has(k) || systemKeys.some((p) => (o.key || "").startsWith(p)) || ns.startsWith("kube-") || (k === "Namespace" && n.startsWith("kube-")) ||
    n.startsWith("system:") || n === "kube-root-ca.crt" || (k === "ServiceAccount" && n === "default") ||
    (k === "Namespace" && (n === "default" || n === "gachinetes"));
}

function drawFloor(objects, later) {
  if (!$("system").checked) objects = objects.filter((o) => !isSystem(o));
  const floor = $("floor");
  floor.replaceChildren();
  const byNs = new Map();
  for (const o of objects) {
    const ns = nsOf(o) || "(cluster)";
    if (!byNs.has(ns)) byNs.set(ns, []);
    byNs.get(ns).push(o);
  }
  if (!byNs.size) floor.textContent = "The gym is empty.";
  for (const [ns, list] of [...byNs].sort()) {
    const sec = document.createElement("div");
    sec.className = "ns";
    const h = document.createElement("h3");
    h.textContent = ns;
    const grid = document.createElement("div");
    grid.className = "athletes";
    for (const o of list.sort((a, b) => (kindOf(a) + nameOf(a)).localeCompare(kindOf(b) + nameOf(b)))) {
      const b = document.createElement("button");
      const mark = o.later ? "later" : later.get(o.key);
      b.className = "athlete" + (mark ? " " + mark : "");
      b.innerHTML = '<span class="k"></span><span class="n"></span><span class="m"></span>';
      b.querySelector(".k").textContent = kindOf(o);
      b.querySelector(".n").textContent = nameOf(o);
      b.querySelector(".m").textContent = { gone: "† gone by now", chg: "~ changed since", later: "+ comes later" }[mark] || o.error || "";
      b.onclick = () => showHistory(o);
      grid.append(b);
    }
    sec.append(h, grid);
    floor.append(sec);
  }
}

// The chat and the danmaku tell what changed between two revisions.
async function chatBetween(from, to) {
  const d = await api("diff", { ...filters(), from: String(Math.min(from, to)), to: String(Math.max(from, to)) });
  const lines = [
    ...d.added.map((c) => ["hype", "gachiHYPER", "+", c]),
    ...d.removed.map((c) => ["bass", "gachiBASS", "−", c]),
    ...d.changed.map((c) => ["gasm", "gachiGASM", "~", c]),
  ];
  const shown = lines.filter((l) => $("system").checked || !isSystem(l[3].to || l[3].from));
  let flown = 0;
  for (const [cls, emote, sign, c] of shown.slice(0, 40)) {
    const o = c.to || c.from;
    const text = `${sign} ${kindOf(o)} ${nsOf(o) ? nsOf(o) + "/" : ""}${nameOf(o)}`;
    const li = document.createElement("li");
    li.innerHTML = `<span class="emote ${cls}"></span><span></span>`;
    li.firstChild.textContent = emote;
    li.lastChild.textContent = text;
    $("chat").prepend(li);
    // Winding, the changes go by too fast to fly across the screen; and
    // only a few fly at once, the chat keeps them all.
    if (state.motion !== "rew" && state.motion !== "ff" && flown++ < 3 && $("danmaku").children.length < 8) fly(text);
  }
  while ($("chat").children.length > 200) $("chat").lastChild.remove();
}

function fly(text) {
  const el = document.createElement("div");
  el.className = "dm";
  el.textContent = text;
  el.style.top = 15 + Math.random() * 70 + "vh";
  el.style.animationDuration = 7 + Math.random() * 5 + "s";
  $("danmaku").append(el);
  el.addEventListener("animationend", () => el.remove());
}

async function showHistory(o) {
  $("h-title").textContent = `${kindOf(o)} ${nsOf(o) ? nsOf(o) + "/" : ""}${nameOf(o)}`;
  $("h-list").replaceChildren();
  $("h-body").textContent = JSON.stringify(o.object || o, null, 2);
  $("history").showModal();
  try {
    const versions = await api("history", { key: o.key });
    for (const v of versions) {
      const li = document.createElement("li");
      const b = document.createElement("button");
      b.textContent = `${v.deleted ? "† " : ""}rev ${v.revision} · ${v.time ? hms(v.time) : "before the tape"}`;
      b.onclick = () => { $("h-body").textContent = v.deleted ? "deleted — rest in peace" : JSON.stringify(v.object, null, 2); };
      li.append(b);
      $("h-list").append(li);
    }
  } catch (e) {
    $("h-body").textContent = e.message;
  }
}

async function drawGraveyard() {
  const grave = $("grave");
  grave.replaceChildren();
  let graves;
  try {
    graves = await api("graveyard", filters());
  } catch (e) {
    grave.textContent = e.message;
    return;
  }
  if (!graves.length) grave.textContent = "Nobody here. Everyone is still lifting.";
  for (const g of graves.filter((g) => $("system").checked || !isSystem(g))) {
    const row = document.createElement("div");
    row.className = "grave";
    const who = document.createElement("span");
    who.className = "who";
    who.textContent = `† ${kindOf(g)} ${nsOf(g) ? nsOf(g) + "/" : ""}${nameOf(g)} · ${g.deletedTime ? hms(g.deletedTime) : "rev " + g.deletedAt}`;
    const b = document.createElement("button");
    b.className = "hot";
    b.textContent = "NOT TODAY";
    b.disabled = !g.object;
    b.onclick = () => revive(g);
    row.append(who, b);
    grave.append(row);
  }
}

async function revive(g) {
  const kind = kindOf(g) + (groupOf(g) ? "." + groupOf(g) : "");
  const params = new URLSearchParams({ kind, name: nameOf(g), namespace: nsOf(g) });
  const body = $("r-body");
  body.textContent = "Planning…";
  $("revive").showModal();
  const r = await fetch(`/api/undelete?${params}`);
  if (!r.ok) { body.textContent = await r.text(); return; }
  const plan = await r.json();
  body.replaceChildren();
  const ul = document.createElement("ul");
  for (const s of plan.restore || []) {
    const li = document.createElement("li");
    li.textContent = `+ ${s.Kind} ${s.Namespace ? s.Namespace + "/" : ""}${s.Name}`;
    ul.append(li);
  }
  for (const s of plan.skipped || []) {
    const li = document.createElement("li");
    li.className = "dim";
    li.textContent = `· ${s.Key} (${s.Why})`;
    ul.append(li);
  }
  body.append(ul);
  $("revive").onclose = async () => {
    if ($("revive").returnValue !== "go") return;
    const res = await fetch(`/api/undelete?${params}`, { method: "POST", headers: { "X-Gachi-Token": token } });
    if (!res.ok) { say(await res.text()); return; }
    const out = await res.json();
    const back = out.filter((x) => x.outcome === "back").length;
    const failed = out.filter((x) => x.outcome === "failed");
    fly(`♂ Rest in peace? Not today: ${back} back`);
    say(failed.length ? failed.map((x) => `${x.kind} ${x.name}: ${x.error}`).join("; ") : "");
    drawGraveyard();
    render();
  };
}

// The deck: a tape that runs until stopped. PLAY runs forward in time
// times the speed, REW and FF wind ten times faster, PAUSE stops where it is.
function stop() { state.motion = null; clearInterval(state.timer); state.timer = null; syncTimeline(); }
function seek(ms) {
  const now = Date.now();
  if (ms >= now - 1000) { state.at = null; stop(); }
  else state.at = Math.max(Math.ceil(tapeStart() / 1000) * 1000, ms);
  render();
}
function step(delta) { seek((state.at == null ? Date.now() : state.at) + delta); }

function run(motion) {
  stop();
  if (state.at == null && motion !== "rew") return;
  if (state.at == null) state.at = Date.now();
  state.motion = motion;
  const tick = 500;
  state.timer = setInterval(() => {
    const speed = Number($("speed").value) * (motion === "play" ? 1 : 4);
    const delta = speed * tick * (motion === "rew" ? -1 : 1);
    const next = state.at + delta;
    if (motion === "rew" && next <= tapeStart()) { seek(tapeStart()); stop(); return; }
    seek(next);
  }, tick);
  syncTimeline();
}

document.querySelector(".buttons").addEventListener("click", (e) => {
  const act = e.target.closest("button") && e.target.closest("button").dataset.act;
  switch (act) {
    case "rew": case "ff": case "play": run(act); break;
    case "back": stop(); step(-10000); break;
    case "fwd": stop(); step(10000); break;
    case "pause": if (state.at == null) state.at = Date.now(); stop(); render(); break;
    case "live": stop(); seek(Date.now()); break;
  }
});
// The bar: the time follows the thumb while it is dragged, the cluster
// follows when it is let go.
const bar = $("timeline");
const barTime = () => tapeStart() + ((Date.now() - tapeStart()) * bar.value) / 1000;
bar.addEventListener("input", () => {
  state.dragging = true;
  stop();
  $("clock").textContent = hms(barTime());
});
bar.addEventListener("change", () => {
  state.dragging = false;
  seek(barTime());
});
for (const id of ["ns", "kind", "system"]) $(id).addEventListener("change", () => { render(); drawGraveyard(); });
document.querySelector(".tabs").addEventListener("click", (e) => {
  const tab = e.target.dataset.tab;
  if (!tab) return;
  for (const t of document.querySelectorAll(".tab")) t.classList.toggle("on", t.dataset.tab === tab);
  for (const p of document.querySelectorAll(".panel")) p.classList.toggle("on", p.id === tab);
  if (tab === "grave") drawGraveyard();
});

(async function start() {
  try {
    await refreshInfo();
  } catch (e) {
    say("Oh shit, I'm sorry: " + e.message);
    return;
  }
  await render();
  // Live, the floor follows the cluster.
  setInterval(() => { if (state.at == null) render(); }, 5000);
  setInterval(refreshInfo, 60000);
})();
