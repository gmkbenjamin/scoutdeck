"use strict";
// ScoutDeck web HUD. Talks to the Mac app's agent API with a same-origin session cookie.

const $ = (id) => document.getElementById(id);
// Touch devices (iPhone, iPad, even with a keyboard) get the on-screen joystick.
const forced = new URLSearchParams(location.search).get("input");
document.documentElement.classList.add(forced === "touch" || (forced !== "mouse" && navigator.maxTouchPoints > 0) ? "touch" : "mouse");
const app = $("app");
const LIMITS = { x: 0.2, y: 0.3, z: 1.8 };
const SPEEDS = [0.45, 0.75, 1.0]; // fraction of the API limits
const STEP_FOR_PHASE = { 1: 0, 8: 0, 2: 1, 3: 2, 4: 2 };

const state = {
  paired: false,
  connected: false,
  speed: Number(localStorage.getItem("scoutdeck.speed") ?? localStorage.getItem("scout.speed") ?? 1),
  joy: { x: 0, y: 0 },
  strafe: 0,
  keys: new Set(),
  pad: { x: 0, y: 0, z: 0 },
  lastSent: null,
  inflight: false,
  lastConflictToast: 0,
  docking: null,
  status: null,
  wakeLock: null,
};

// ---------- HTTP ----------
async function api(path, opts = {}) {
  const init = { method: opts.method || "GET", credentials: "same-origin", headers: {} };
  if (opts.body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(opts.body);
  }
  const res = await fetch(path, init);
  if (res.status === 401) {
    showPairing();
    throw new Error("not paired");
  }
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = { error: text }; }
  if (!res.ok) {
    const err = new Error((data && data.error) || res.statusText);
    err.status = res.status;
    throw err;
  }
  return data;
}

// ---------- Toasts ----------
function toast(msg, kind = "info", ms = 4000) {
  const el = document.createElement("div");
  el.className = "toast " + kind;
  el.textContent = msg;
  $("toasts").append(el);
  while ($("toasts").children.length > 3) $("toasts").firstChild.remove();
  setTimeout(() => el.remove(), ms);
}

// ---------- Pairing ----------
function showPairing() {
  state.paired = false;
  $("pairing").hidden = false;
  $("offline").hidden = true;
}

async function pair(code) {
  const res = await fetch("/web/pair", {
    method: "POST", credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token: code.trim() }),
  });
  if (!res.ok) throw new Error(res.status === 401 ? "That code isn't valid." : "Pairing failed (" + res.status + ").");
}

$("pairForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("pairError").textContent = "";
  try {
    await pair($("pairCode").value);
    $("pairCode").value = "";
    $("pairing").hidden = true;
    start();
  } catch (err) {
    $("pairError").textContent = err.message;
  }
});

// ---------- Status ----------
const fmtTime = (s) => `${Math.floor(s / 60)}:${String(Math.floor(s % 60)).padStart(2, "0")}`;

function renderStatus(s) {
  state.status = s;
  const robot = s.robot || {};
  state.connected = !!robot.connected;
  app.dataset.state = state.connected ? "ready" : "offline";
  $("offline").hidden = state.connected || !state.paired;
  if (!state.connected && robot.message) $("offlineText").textContent = robot.message;
  $("host").textContent = robot.host || "";

  const camLive = s.camera && s.camera.available;
  app.dataset.video = camLive ? "live" : (videoLoaded ? "stale" : "none");
  $("stageMsg").textContent = state.connected ? (videoLoaded ? "Video paused" : "Waiting for video…") : "Robot offline";
  $("liveDot").style.background = camLive ? "var(--good)" : "var(--warn)";
  $("live").textContent = camLive ? "LIVE" : "NO VIDEO";
  $("live").style.color = camLive ? "var(--good)" : "var(--warn)";

  let owner = (s.control && s.control.owner) || "stopped";
  if (owner === "agent" && s.activity && s.activity.kind) owner = s.activity.kind;
  const badge = { manual: "MAC", agent: "YOU", move: "MOVING", patrol: "PATROL", docking: "DOCKING", "manual-stop": "STOP", stopped: "IDLE" }[owner] || owner.toUpperCase();
  $("owner").textContent = badge;
  $("owner").dataset.owner = owner === "manual-stop" ? "stop" : (owner === "move" || owner === "patrol") ? "agent" : owner;
  $("stopBanner").hidden = owner !== "manual-stop";
  if (owner === "manual-stop") $("stopBanner").textContent = "STOPPED ON THE MAC";

  const b = s.battery || {};
  const pct = b.percent;
  if (pct >= 0) {
    $("battPct").textContent = pct + "%";
    $("battFill").style.width = Math.max(4, Math.min(100, pct)) + "%";
    $("battFill").style.background = pct < 15 ? "var(--bad)" : pct < 40 ? "var(--warn)" : "var(--good)";
    $("battLabel").textContent = { 0: "Charging", 1: "On battery", 2: "On dock · full" }[b.charge_state] || "Battery";
    $("battLabel").style.color = { 0: "var(--accent)", 2: "var(--good)" }[b.charge_state] || "";
  } else {
    $("battPct").textContent = "--";
    $("battFill").style.width = "0";
  }
  renderDock(s.docking);
  renderExtras(s);
  const ad = s.auto_dock || {};
  $("autoBanner").hidden = !(ad.countdown_seconds > 0);
  if (ad.countdown_seconds > 0) $("autoBanner").textContent = `LOW BATTERY · DOCKING IN ${ad.countdown_seconds}s`;
}

function renderDock(op) {
  state.docking = op;
  const panel = $("dockPanel");
  const now = Date.now();
  const recent = op && op.id && (op.active || now - Date.parse(op.updated_at) < 20000);
  panel.hidden = !recent;
  $("dockBtn").textContent = op && op.active ? "Cancel dock" : "Dock";
  $("dockBtn").classList.toggle("active", !!(op && op.active));
  if (!recent) return;
  const st = $("dockState");
  let label = op.state.replace(/_/g, " ").toUpperCase();
  st.className = "dock-state";
  if (op.state === "charging") { label = "DOCKED"; st.classList.add("good"); }
  else if (["failed", "timed_out", "cancel_failed"].includes(op.state)) st.classList.add("bad");
  else if (op.state === "cancelled") st.classList.add("dim");
  st.textContent = label;
  const started = Date.parse(op.started_at);
  const end = op.active ? now : Date.parse(op.updated_at);
  $("dockTime").textContent = fmtTime((end - started) / 1000) + " / " + fmtTime((Date.parse(op.deadline) - started) / 1000);
  let current = op.state === "charging" ? 4 : (STEP_FOR_PHASE[op.firmware_phase] ?? -1);
  const failed = !op.active && op.state !== "charging";
  [...$("steps").children].forEach((li, i) => {
    li.className = i < current ? "done" : i === current ? (failed ? "fail" : "now") : "";
  });
  const line = $("dockLine");
  line.className = "dock-line muted";
  if (op.error) { line.textContent = op.error; line.className = "dock-line bad"; }
  else if (op.state === "charging") { line.textContent = "Docked and charging (verified)"; line.className = "dock-line good"; }
  else if (op.active) line.textContent = "Stop cancels docking" + (op.power && op.power.status ? " · power: " + op.power.status : "");
  else line.textContent = "";
}

async function pollStatus() {
  if (document.hidden || !state.paired) return;
  try {
    renderStatus(await api("/v1/status"));
  } catch (err) {
    if (err.message !== "not paired") {
      app.dataset.state = "offline";
      $("stageMsg").textContent = "Can't reach the ScoutDeck app on the Mac";
    }
  }
}

// ---------- Video ----------
let videoLoaded = false;
let videoRetry = null;
function startVideo() {
  clearTimeout(videoRetry);
  $("video").src = "/v1/video.mjpeg?t=" + Date.now();
}
$("video").addEventListener("load", () => { videoLoaded = true; });
$("video").addEventListener("error", () => {
  clearTimeout(videoRetry);
  videoRetry = setTimeout(startVideo, 2000);
});

// ---------- Driving ----------
function currentMotion() {
  const k = state.keys;
  const kf = (k.has("w") || k.has("arrowup") ? 1 : 0) - (k.has("s") || k.has("arrowdown") ? 1 : 0);
  const kz = (k.has("a") || k.has("arrowleft") ? 1 : 0) - (k.has("d") || k.has("arrowright") ? 1 : 0);
  const kx = (k.has("e") ? 1 : 0) - (k.has("q") ? 1 : 0);
  const pick = (...v) => v.reduce((a, b) => (Math.abs(b) > Math.abs(a) ? b : a), 0);
  const f = SPEEDS[state.speed] ?? SPEEDS[1];
  const x = pick(kx, state.strafe, state.pad.x);
  const y = pick(kf, state.joy.y, state.pad.y);
  const z = pick(kz, -state.joy.x, state.pad.z);
  const r = (v) => Math.round(v * 1000) / 1000;
  return { linear_x: r(x * LIMITS.x * f), linear_y: r(y * LIMITS.y * f), angular_z: r(z * LIMITS.z * f) };
}
const isZero = (m) => !m.linear_x && !m.linear_y && !m.angular_z;

async function driveTick() {
  if (!state.connected || state.inflight || document.hidden) return;
  const m = currentMotion();
  const wasMoving = state.lastSent && !isZero(state.lastSent);
  if (isZero(m) && !wasMoving) return;
  state.inflight = true;
  try {
    await api("/v1/drive", { method: "POST", body: { ...m, ttl_ms: 300 } });
    state.lastSent = m;
    if (!isZero(m)) keepAwake();
  } catch (err) {
    state.lastSent = null;
    if (err.status === 409 && Date.now() - state.lastConflictToast > 3000) {
      state.lastConflictToast = Date.now();
      toast(err.message, "bad");
    }
  } finally {
    state.inflight = false;
  }
}
setInterval(driveTick, 100);

function releaseAll() {
  state.keys.clear();
  state.joy = { x: 0, y: 0 };
  state.strafe = 0;
  resetJoystick();
  resetStrafe();
}
window.addEventListener("blur", releaseAll);
document.addEventListener("visibilitychange", () => {
  if (document.hidden) releaseAll();
  else { pollStatus(); startVideo(); }
});

async function keepAwake() {
  if (state.wakeLock || !("wakeLock" in navigator)) return;
  try {
    state.wakeLock = await navigator.wakeLock.request("screen");
    state.wakeLock.addEventListener("release", () => { state.wakeLock = null; });
  } catch { /* not allowed right now */ }
}

// Joystick: up = forward, left/right = turn.
const joy = $("joy"), knob = $("knob");
let joyPointer = null;
function setJoystick(clientX, clientY) {
  const r = joy.getBoundingClientRect();
  const radius = r.width / 2;
  let dx = clientX - (r.left + radius), dy = clientY - (r.top + radius);
  const d = Math.hypot(dx, dy), max = radius * 0.8;
  if (d > max) { dx *= max / d; dy *= max / d; }
  knob.style.transform = `translate(calc(-50% + ${dx}px), calc(-50% + ${dy}px))`;
  const dead = (v) => (Math.abs(v) < 0.12 ? 0 : (v - Math.sign(v) * 0.12) / 0.88);
  state.joy = { x: dead(dx / max), y: dead(-dy / max) };
}
function resetJoystick() {
  joyPointer = null;
  joy.classList.remove("active");
  knob.style.transform = "translate(-50%, -50%)";
  state.joy = { x: 0, y: 0 };
}
joy.addEventListener("pointerdown", (e) => {
  joyPointer = e.pointerId;
  joy.setPointerCapture(e.pointerId);
  joy.classList.add("active");
  setJoystick(e.clientX, e.clientY);
});
joy.addEventListener("pointermove", (e) => { if (e.pointerId === joyPointer) setJoystick(e.clientX, e.clientY); });
["pointerup", "pointercancel", "lostpointercapture"].forEach((t) => joy.addEventListener(t, (e) => { if (e.pointerId === joyPointer) resetJoystick(); }));

// Strafe slider springs back to centre.
const strafe = $("strafe"), strafeKnob = $("strafeKnob");
let strafePointer = null;
function setStrafe(clientX) {
  const r = strafe.getBoundingClientRect();
  const half = r.width / 2 - 26;
  let dx = Math.max(-half, Math.min(half, clientX - (r.left + r.width / 2)));
  strafeKnob.style.transform = `translateX(${dx}px)`;
  const v = dx / half;
  state.strafe = Math.abs(v) < 0.15 ? 0 : v;
}
function resetStrafe() {
  strafePointer = null;
  strafe.classList.remove("active");
  strafeKnob.style.transform = "";
  state.strafe = 0;
}
strafe.addEventListener("pointerdown", (e) => {
  strafePointer = e.pointerId;
  strafe.setPointerCapture(e.pointerId);
  strafe.classList.add("active");
  setStrafe(e.clientX);
});
strafe.addEventListener("pointermove", (e) => { if (e.pointerId === strafePointer) setStrafe(e.clientX); });
["pointerup", "pointercancel", "lostpointercapture"].forEach((t) => strafe.addEventListener(t, (e) => { if (e.pointerId === strafePointer) resetStrafe(); }));

// ---------- Actions ----------
async function stopAll() {
  releaseAll();
  try { await api("/v1/stop", { method: "POST" }); toast("Stopped"); }
  catch (err) { toast(err.message, "bad"); }
}
async function toggleDock() {
  try {
    if (state.docking && state.docking.active) {
      await api("/v1/dock/cancel", { method: "POST" });
      toast("Docking cancelled");
    } else {
      releaseAll();
      renderDock(await api("/v1/dock", { method: "POST", body: { timeout_seconds: 300 } }));
      toast("Docking started");
    }
  } catch (err) { toast(err.message, "bad"); }
}
async function light(command) {
  try { await api("/v1/light", { method: "POST", body: { command } }); }
  catch (err) { toast(err.message, "bad"); }
}
async function snapshot() {
  try {
    const res = await fetch("/v1/screenshot.jpg", { credentials: "same-origin" });
    if (!res.ok) throw new Error("No fresh camera frame");
    const url = URL.createObjectURL(await res.blob());
    const a = document.createElement("a");
    a.href = url;
    a.download = "scoutdeck-" + new Date().toISOString().replace(/[:.]/g, "-") + ".jpg";
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
    toast("Snapshot saved", "good");
  } catch (err) { toast(err.message, "bad"); }
}
function setSpeed(i) {
  state.speed = Math.max(0, Math.min(2, i));
  localStorage.setItem("scoutdeck.speed", state.speed);
  document.querySelectorAll(".speed button").forEach((b) => b.setAttribute("aria-checked", String(Number(b.dataset.speed) === state.speed)));
}

$("stopBtn").addEventListener("click", stopAll);
$("dockBtn").addEventListener("click", toggleDock);
$("snapBtn").addEventListener("click", snapshot);
$("dimBtn").addEventListener("click", () => light(0));
$("brightBtn").addEventListener("click", () => light(1));
document.querySelectorAll(".speed button").forEach((b) => b.addEventListener("click", () => setSpeed(Number(b.dataset.speed))));

$("menuBtn").addEventListener("click", () => openSheet("settings"));
$("eventsBtn").addEventListener("click", () => openSheet("events"));
$("offlineEvents").addEventListener("click", () => openSheet("events"));
async function saveAutoDock() {
  try {
    const ad = await api("/v1/autodock", { method: "POST", body: { enabled: $("adOn").checked, threshold: Number($("adAt").value) } });
    toast(ad.enabled ? `Auto-dock on below ${ad.threshold}%` : "Auto-dock off", "good");
  } catch (err) { toast(err.message, "bad"); }
}
$("adOn").addEventListener("change", saveAutoDock);
$("adAt").addEventListener("change", saveAutoDock);
$("menuClose").addEventListener("click", closeSheet);
$("menu").addEventListener("click", (e) => { if (e.target === $("menu")) closeSheet(); });
$("fullBtn").hidden = !document.documentElement.requestFullscreen;
$("fullBtn").addEventListener("click", () => {
  $("menu").hidden = true;
  document.documentElement.requestFullscreen?.().catch(() => {});
});
$("unpairBtn").addEventListener("click", async () => {
  await fetch("/web/unpair", { method: "POST", credentials: "same-origin" });
  $("menu").hidden = true;
  showPairing();
});

// ---------- Keyboard ----------
const DRIVE_KEYS = new Set(["w", "a", "s", "d", "q", "e", "arrowup", "arrowdown", "arrowleft", "arrowright"]);
window.addEventListener("keydown", (e) => {
  if (e.target instanceof HTMLInputElement || e.target instanceof HTMLSelectElement || e.metaKey || e.ctrlKey || e.altKey) return;
  const k = e.key.toLowerCase();
  if (DRIVE_KEYS.has(k)) { state.keys.add(k); e.preventDefault(); return; }
  if (e.repeat) return;
  if (k === "escape" && !$("menu").hidden) { closeSheet(); return; }
  if (k === "t") { e.preventDefault(); talkStart(); return; }
  const act = { x: stopAll, h: toggleDock, " ": snapshot, r: toggleRec, g: toggleGuard, o: () => setSpeed(state.speed - 1), p: () => setSpeed(state.speed + 1), 9: () => light(0), 0: () => light(1) }[k];
  if (act) { e.preventDefault(); act(); }
});
window.addEventListener("keyup", (e) => {
  const k = e.key.toLowerCase();
  state.keys.delete(k);
  if (k === "t") talkStop();
});

// ---------- Gamepad (Xbox standard mapping) ----------
let padButtons = [];
function pollGamepad() {
  const pads = navigator.getGamepads ? [...navigator.getGamepads()].filter((p) => p && p.mapping === "standard") : [];
  const p = pads[0];
  $("padName").textContent = p ? "🎮 " + p.id.replace(/\(.*\)/, "").trim() : "";
  if (p) {
    const dz = (v) => (Math.abs(v) < 0.15 ? 0 : (v - Math.sign(v) * 0.15) / 0.85);
    state.pad = { x: dz(p.axes[2]), y: dz(-p.axes[1]), z: dz(-p.axes[0]) };
    const pressed = p.buttons.map((b) => b.pressed);
    const edge = (i) => pressed[i] && !padButtons[i];
    if (edge(1) || (pressed[4] && pressed[5] && (edge(4) || edge(5)))) stopAll();
    else {
      if (edge(4)) setSpeed(state.speed - 1);
      if (edge(5)) setSpeed(state.speed + 1);
    }
    if (edge(3)) toggleDock();
    if (edge(0)) snapshot();
    if (edge(2)) light(4);
    if (edge(12)) light(1);
    if (edge(13)) light(0);
    padButtons = pressed;
  } else {
    state.pad = { x: 0, y: 0, z: 0 };
  }
  requestAnimationFrame(pollGamepad);
}

// iOS: block pinch zoom and double-tap zoom on the HUD.
document.addEventListener("gesturestart", (e) => e.preventDefault());
document.addEventListener("dblclick", (e) => e.preventDefault());

// ---------- Sensors, detections, activity, map ----------
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; };
const fmtRange = (m) => (Math.round(m * 20) / 20).toFixed(2) + " m";
let lastEventId = null;

function rangeClass(m) { return m <= 0.25 ? "bad" : m < 0.6 ? "warn" : "good"; }

function renderExtras(s) {
  const sn = s.sensors || {};
  // Range only when something is close enough to matter (inside the slow-down distance).
  const r = $("range"), m = sn.obstacle_m;
  const near = sn.range_fresh && m != null && m < 0.6;
  r.hidden = !near;
  if (near) {
    $("rangeVal").textContent = fmtRange(m);
    r.dataset.level = rangeClass(m);
    r.dataset.limiting = sn.obstacle_guard && sn.obstacle_guard.limiting ? "1" : "";
  }

  // Chips: recording, guard, night vision.
  const chips = $("chips");
  chips.replaceChildren();
  const chip = (text, kind) => chips.append(el("span", "chip " + kind, text));
  const rec = s.recording || {};
  state.recording = !!rec.active;
  if (rec.active) chip("● REC " + fmtTime(rec.seconds || 0), "bad");
  const g = s.guard || {};
  state.guard = g;
  if (g.armed) chip("GUARD", "bad");
  if (sn.night_vision) chip("NIGHT IR", "agent");
  if (s.route_recording && s.route_recording.active) chip("● ROUTE " + s.route_recording.name, "bad");
  $("recBtn").textContent = rec.active ? "Stop rec" : "Rec";
  $("recBtn").classList.toggle("on-bad", !!rec.active);
  $("guardBtn").textContent = g.armed ? "Guard on" : "Guard";
  $("guardBtn").classList.toggle("on-bad", !!g.armed);

  // New event badge and toast.
  if (g.last_event && g.last_event.id !== lastEventId) {
    if (lastEventId !== null && ["motion", "person", "cat", "dog"].includes(g.last_event.kind)) toast(g.last_event.text, "bad", 6000);
    lastEventId = g.last_event.id;
  }
  const count = $("eventCount");
  count.hidden = !g.events_today;
  count.textContent = g.events_today > 99 ? "99+" : g.events_today || "";

  renderDetections(s.detections || []);
  renderActivity(s.activity || {}, s.route_recording || {});
  renderRouteRecording(s.route_recording || {});
}

// Boxes are normalised to the camera image; map them onto the letterboxed <img>.
function renderDetections(list) {
  const layer = $("detLayer");
  layer.replaceChildren();
  const img = $("video");
  if (!list.length || !img.naturalWidth) return;
  const box = img.getBoundingClientRect(), stage = $("stage").getBoundingClientRect();
  const sc = Math.min(box.width / img.naturalWidth, box.height / img.naturalHeight);
  const w = img.naturalWidth * sc, h = img.naturalHeight * sc;
  const ox = box.left - stage.left + (box.width - w) / 2, oy = box.top - stage.top + (box.height - h) / 2;
  for (const d of list) {
    const [l, t, r, b] = d.box;
    const div = el("div", "det " + (d.name === "person" ? "bad" : d.name === "motion" ? "agent" : "warn"));
    div.style.left = ox + l * w + "px";
    div.style.top = oy + t * h + "px";
    div.style.width = Math.max(4, (r - l) * w) + "px";
    div.style.height = Math.max(4, (b - t) * h) + "px";
    div.append(el("span", "", d.name.toUpperCase() + (d.score ? " " + Math.round(d.score * 100) + "%" : "")));
    layer.append(div);
  }
}

function renderActivity(a, rr) {
  const p = $("actPanel");
  const show = !!a.kind || !!(a.last);
  p.hidden = !show;
  if (!show) return;
  if (a.kind) {
    $("actKind").textContent = a.kind.toUpperCase();
    $("actLabel").textContent = a.label || "";
    $("actLabel").className = "dock-state";
    $("actTime").textContent = fmtTime(a.seconds || 0);
    let frac = 0; const parts = [];
    if (a.steps) { frac = (a.step - 1) / a.steps; parts.push(`step ${a.step} of ${a.steps}`); }
    if (a.target) {
      const f = Math.max(0, Math.min(1, a.achieved / a.target));
      frac = a.steps ? frac + f / a.steps : f;
      parts.push(a.unit === "deg" ? `${Math.round(a.achieved)}° / ${Math.round(a.target)}°` : `${a.achieved.toFixed(2)} / ${a.target.toFixed(2)} m`);
    }
    $("actBar").style.width = Math.round(Math.max(0, Math.min(1, frac)) * 100) + "%";
    $("actLine").textContent = parts.join(" · ") + (parts.length ? " · " : "") + "Stop cancels";
  } else {
    const l = a.last;
    $("actKind").textContent = (l.kind || "").toUpperCase();
    $("actLabel").textContent = l.completed ? "DONE" : "STOPPED: " + (l.stopped_by || "").toUpperCase();
    $("actLabel").className = "dock-state " + (l.completed ? "good" : "bad");
    $("actTime").textContent = "";
    $("actBar").style.width = l.completed ? "100%" : "0";
    $("actLine").textContent = l.error || "";
  }
}

// Mini-map: heading-up, robot in the centre, trail and dock.
let mapTimer = null;
async function drawMap() {
  const c = $("minimap");
  if (!state.connected || document.hidden || getComputedStyle(c).display === "none") return;
  let data;
  try { data = await api("/v1/pose"); } catch { return; }
  const ctx = c.getContext("2d");
  const W = c.width, H = c.height, cx = W / 2, cy = H / 2 + 14;
  ctx.clearRect(0, 0, W, H);
  const p = data.pose, trail = data.trail || [];
  let reach = 1.5;
  for (const t of trail.concat([{ x: 0, y: 0 }])) reach = Math.max(reach, Math.hypot(t.x - p.x, t.y - p.y));
  reach = Math.min(reach, 25);
  const scale = (W / 2 - 20) / reach;
  const cs = Math.cos(-p.yaw), sn = Math.sin(-p.yaw);
  const to = (x, y) => { const dx = x - p.x, dy = y - p.y; const fx = dx * cs - dy * sn, fy = dx * sn + dy * cs; return [cx - fy * scale, cy - fx * scale]; };
  const step = reach > 6 ? 5 : 1;
  ctx.strokeStyle = "rgba(79,209,255,.15)"; ctx.lineWidth = 1;
  for (let m = step; m * scale < W; m += step) { ctx.beginPath(); ctx.arc(cx, cy, m * scale, 0, Math.PI * 2); ctx.stroke(); }
  const s = state.status && state.status.sensors;
  if (s && s.range_fresh) {
    const L = Math.min((s.obstacle_m ?? 2) * scale, W / 2), half = 0.218;
    ctx.fillStyle = s.obstacle_m == null ? "rgba(61,220,132,.16)" : s.obstacle_m < 0.6 ? "rgba(255,120,60,.35)" : "rgba(61,220,132,.3)";
    ctx.beginPath(); ctx.moveTo(cx, cy); ctx.lineTo(cx - Math.sin(half) * L, cy - Math.cos(half) * L); ctx.lineTo(cx + Math.sin(half) * L, cy - Math.cos(half) * L); ctx.fill();
  }
  ctx.lineWidth = 2.5; ctx.lineJoin = "round";
  for (let i = 1; i < trail.length; i++) {
    const [x0, y0] = to(trail[i - 1].x, trail[i - 1].y), [x1, y1] = to(trail[i].x, trail[i].y);
    ctx.strokeStyle = `rgba(79,209,255,${0.2 + 0.7 * i / trail.length})`;
    ctx.beginPath(); ctx.moveTo(x0, y0); ctx.lineTo(x1, y1); ctx.stroke();
  }
  const [dx, dy] = to(0, 0);
  ctx.fillStyle = "#3ddc84"; ctx.fillRect(dx - 5, dy - 5, 10, 10);
  ctx.fillStyle = "#eceff4";
  ctx.beginPath(); ctx.moveTo(cx, cy - 10); ctx.lineTo(cx + 7, cy + 7); ctx.lineTo(cx, cy + 3); ctx.lineTo(cx - 7, cy + 7); ctx.fill();
  ctx.fillStyle = "rgba(150,158,170,.9)"; ctx.font = "600 11px -apple-system, sans-serif";
  ctx.fillText("MAP · " + step + " m grid", 10, 16);
  ctx.textAlign = "right"; ctx.fillText(p.odometer_m.toFixed(1) + " m", W - 10, 16); ctx.textAlign = "left";
}

// ---------- Recording, guard, turn ----------
async function toggleRec() {
  try {
    if (state.recording) { const r = await api("/v1/record/stop", { method: "POST" }); toast("Saved on the Mac: " + r.file.split("/").pop(), "good"); }
    else { await api("/v1/record/start", { method: "POST" }); toast("Recording on the Mac"); }
    pollStatus();
  } catch (err) { toast(err.message, "bad"); }
}
async function toggleGuard() {
  try {
    const g = await api("/v1/guard", { method: "POST", body: { armed: !(state.guard && state.guard.armed) } });
    toast(g.armed ? "Guard mode armed" : "Guard mode off", g.armed ? "good" : "info");
    pollStatus();
  } catch (err) { toast(err.message, "bad"); }
}
async function turnAround() {
  releaseAll();
  try {
    const r = await api("/v1/turn", { method: "POST", body: { degrees: 180 } });
    toast(r.completed ? `Turned ${Math.round(r.achieved * 180 / Math.PI)}°` : "Turn stopped: " + r.stopped_by, r.completed ? "good" : "bad");
  } catch (err) { toast(err.message, "bad"); }
}
$("recBtn").addEventListener("click", toggleRec);
$("guardBtn").addEventListener("click", toggleGuard);
$("turnBtn").addEventListener("click", turnAround);

// ---------- Listen and push-to-talk ----------
$("listenBtn").addEventListener("click", () => {
  const a = $("listenAudio");
  if (!a.paused) { a.pause(); a.removeAttribute("src"); a.load(); $("listenBtn").classList.remove("on"); $("listenBtn").textContent = "Listen"; return; }
  a.src = "/v1/microphone.aac?t=" + Date.now();
  a.play().then(() => { $("listenBtn").classList.add("on"); $("listenBtn").textContent = "Listening"; })
    .catch((err) => toast("Can't play the robot's microphone here: " + err.message, "bad"));
});

const talk = { stream: null, ctx: null, node: null, chunks: [], rate: 48000, active: false, started: 0 };
async function talkStart() {
  if (talk.active) return;
  if (!navigator.mediaDevices || !window.isSecureContext) {
    toast("Talking needs the HTTPS (Tailscale) address or this Mac's own browser", "bad");
    return;
  }
  talk.active = true; talk.chunks = []; talk.started = Date.now();
  $("talkBtn").classList.add("on-bad"); $("talkBtn").textContent = "Talking…";
  try {
    talk.stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
    if (!talk.active) { talkCleanup(); return; }
    talk.ctx = new (window.AudioContext || window.webkitAudioContext)();
    talk.rate = talk.ctx.sampleRate;
    const src = talk.ctx.createMediaStreamSource(talk.stream);
    talk.node = talk.ctx.createScriptProcessor(4096, 1, 1);
    talk.node.onaudioprocess = (e) => {
      if (talk.active && Date.now() - talk.started < 29000) talk.chunks.push(new Float32Array(e.inputBuffer.getChannelData(0)));
    };
    src.connect(talk.node); talk.node.connect(talk.ctx.destination);
  } catch (err) {
    talk.active = false; talkCleanup();
    toast("Microphone unavailable: " + err.message, "bad");
  }
}
function talkCleanup() {
  if (talk.node) { talk.node.disconnect(); talk.node.onaudioprocess = null; }
  if (talk.ctx) talk.ctx.close();
  if (talk.stream) talk.stream.getTracks().forEach((t) => t.stop());
  talk.node = talk.ctx = talk.stream = null;
  $("talkBtn").classList.remove("on-bad"); $("talkBtn").textContent = "Hold to talk";
}
async function talkStop() {
  if (!talk.active) return;
  talk.active = false;
  const rate = talk.rate, chunks = talk.chunks;
  talkCleanup();
  const n = chunks.reduce((a, c) => a + c.length, 0);
  if (n < rate * 0.3) { toast("Hold the button while you speak"); return; }
  // Downsample to 16 kHz mono PCM16 and wrap in a WAV header.
  const all = new Float32Array(n); let o = 0;
  for (const c of chunks) { all.set(c, o); o += c.length; }
  const ratio = rate / 16000, len = Math.floor(n / ratio);
  const pcm = new Int16Array(len);
  for (let i = 0; i < len; i++) {
    const a = Math.floor(i * ratio), b = Math.min(n, Math.floor((i + 1) * ratio));
    let sum = 0; for (let j = a; j < b; j++) sum += all[j];
    const v = Math.max(-1, Math.min(1, (sum / Math.max(1, b - a)) * 1.6));
    pcm[i] = v < 0 ? v * 0x8000 : v * 0x7fff;
  }
  const buf = new ArrayBuffer(44 + pcm.byteLength), dv = new DataView(buf);
  const str = (off, s) => { for (let i = 0; i < s.length; i++) dv.setUint8(off + i, s.charCodeAt(i)); };
  str(0, "RIFF"); dv.setUint32(4, 36 + pcm.byteLength, true); str(8, "WAVE"); str(12, "fmt ");
  dv.setUint32(16, 16, true); dv.setUint16(20, 1, true); dv.setUint16(22, 1, true); dv.setUint32(24, 16000, true);
  dv.setUint32(28, 32000, true); dv.setUint16(32, 2, true); dv.setUint16(34, 16, true); str(36, "data"); dv.setUint32(40, pcm.byteLength, true);
  new Int16Array(buf, 44).set(pcm);
  try {
    const res = await fetch("/v1/speaker.wav", { method: "POST", credentials: "same-origin", headers: { "Content-Type": "audio/wav" }, body: buf });
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
    toast("Played on the robot", "good");
  } catch (err) { toast(err.message, "bad"); }
}
const tb = $("talkBtn");
tb.addEventListener("pointerdown", (e) => { e.preventDefault(); tb.setPointerCapture(e.pointerId); talkStart(); });
["pointerup", "pointercancel", "lostpointercapture"].forEach((t) => tb.addEventListener(t, talkStop));

// ---------- Sheet with tabs ----------
let currentTab = "events";
function openSheet(tab) {
  $("menu").hidden = false;
  showTab(tab || currentTab);
}
function closeSheet() { $("menu").hidden = true; }
function showTab(tab) {
  currentTab = tab;
  document.querySelectorAll("#tabs [data-tab]").forEach((b) => b.classList.toggle("on", b.dataset.tab === tab));
  document.querySelectorAll(".tab").forEach((p) => { p.hidden = p.dataset.panel !== tab; });
  ({ events: loadEvents, patrols: loadRoutes, clips: loadClips, camera: loadCamera, health: loadHealth, settings: loadSettings }[tab] || (() => {}))();
}
document.querySelectorAll("#tabs [data-tab]").forEach((b) => b.addEventListener("click", () => showTab(b.dataset.tab)));

const KIND_CLASS = { person: "bad", cat: "warn", dog: "warn", motion: "agent", patrol: "good", dock: "good" };
const ago = (t) => {
  const s = (Date.now() - Date.parse(t)) / 1000;
  return s < 60 ? Math.round(s) + "s ago" : s < 3600 ? Math.round(s / 60) + " min ago" : s < 86400 ? Math.round(s / 3600) + " h ago" : new Date(t).toLocaleDateString();
};
async function loadEvents() {
  try {
    const [ev, g] = await Promise.all([api("/v1/events?limit=60"), api("/v1/guard")]);
    $("guardOn").checked = !!g.armed;
    const list = $("eventList");
    list.replaceChildren();
    if (!ev.events.length) list.append(el("p", "muted", "No events yet. Arm guard mode to watch for motion, people and pets."));
    for (const e of ev.events) {
      const row = el("div", "event " + (KIND_CLASS[e.kind] || ""));
      if (e.snapshot) {
        const img = el("img"); img.loading = "lazy"; img.alt = ""; img.src = "/v1/events/" + encodeURIComponent(e.id) + ".jpg";
        img.addEventListener("click", () => img.classList.toggle("big"));
        row.append(img);
      } else row.append(el("div", "no-img", e.kind === "patrol" ? "⟲" : "•"));
      const body = el("div", "event-body");
      body.append(el("b", "", e.text), el("span", "muted", new Date(e.at).toLocaleTimeString() + " · " + ago(e.at)));
      row.append(body);
      list.append(row);
    }
  } catch (err) { toast(err.message, "bad"); }
}
$("guardOn").addEventListener("change", async () => {
  try { await api("/v1/guard", { method: "POST", body: { armed: $("guardOn").checked } }); pollStatus(); loadEvents(); }
  catch (err) { toast(err.message, "bad"); }
});
$("eventsClear").addEventListener("click", async () => {
  if (!confirmTwice($("eventsClear"), "Really clear?")) return;
  try { await api("/v1/events", { method: "DELETE" }); loadEvents(); } catch (err) { toast(err.message, "bad"); }
});

// Two taps instead of a browser confirm() dialog.
function confirmTwice(btn, label) {
  if (btn.dataset.armed === "1") { btn.dataset.armed = ""; btn.textContent = btn.dataset.label; return true; }
  btn.dataset.label = btn.textContent; btn.dataset.armed = "1"; btn.textContent = label;
  setTimeout(() => { if (btn.dataset.armed) { btn.dataset.armed = ""; btn.textContent = btn.dataset.label; } }, 3000);
  return false;
}

const SCHEDULES = [[0, "Manual"], [30, "Every 30 min"], [60, "Hourly"], [120, "Every 2 h"], [240, "Every 4 h"], [480, "Every 8 h"], [1440, "Daily"]];
async function loadRoutes() {
  try {
    const data = await api("/v1/routes");
    renderRouteRecording(data.recording);
    const list = $("routeList");
    list.replaceChildren();
    if (!data.routes.length) list.append(el("p", "muted", "No routes yet."));
    for (const r of data.routes) {
      const row = el("div", "route");
      const info = el("div", "route-info");
      info.append(el("b", "", r.name), el("span", "muted", `${r.steps.length} steps · ${r.distance_m.toFixed(1)} m · ${fmtTime(r.duration_s)}` + (r.last_run && !r.last_run.startsWith("0001") ? " · last " + ago(r.last_run) : "")));
      const sel = el("select");
      for (const [v, label] of SCHEDULES) { const o = el("option", "", label); o.value = v; sel.append(o); }
      sel.value = String(r.every_minutes || 0);
      sel.addEventListener("change", () => updateRoute(r.name, { every_minutes: Number(sel.value), dock_after: r.dock_after }));
      const dockLbl = el("label", "inline-check"); const dock = el("input"); dock.type = "checkbox"; dock.checked = r.dock_after;
      dock.addEventListener("change", () => updateRoute(r.name, { every_minutes: Number(sel.value), dock_after: dock.checked }));
      dockLbl.append(dock, document.createTextNode(" dock after"));
      const run = el("button", "btn primary small-btn", "Run");
      run.addEventListener("click", async () => {
        try { releaseAll(); await api("/v1/routes/" + encodeURIComponent(r.name) + "/run", { method: "POST" }); closeSheet(); toast("Patrol started: " + r.name); }
        catch (err) { toast(err.message, "bad"); }
      });
      const del = el("button", "btn small-btn", "Delete");
      del.addEventListener("click", async () => {
        if (!confirmTwice(del, "Sure?")) return;
        try { await api("/v1/routes/" + encodeURIComponent(r.name), { method: "DELETE" }); loadRoutes(); } catch (err) { toast(err.message, "bad"); }
      });
      const ctl = el("div", "route-ctl"); ctl.append(sel, dockLbl, del, run);
      row.append(info, ctl);
      list.append(row);
    }
  } catch (err) { toast(err.message, "bad"); }
}
async function updateRoute(name, body) {
  try { await api("/v1/routes/" + encodeURIComponent(name), { method: "POST", body }); toast("Saved", "good"); }
  catch (err) { toast(err.message, "bad"); loadRoutes(); }
}
function renderRouteRecording(rr) {
  if (!rr) return;
  $("routeRec").hidden = !!rr.active;
  $("routeRecLive").hidden = !rr.active;
  if (rr.active) { $("routeRecName").textContent = rr.name; $("routeRecSteps").textContent = `${rr.steps} steps · ${fmtTime(rr.seconds || 0)}`; }
}
$("routeStart").addEventListener("click", async () => {
  const name = $("routeName").value.trim();
  if (!name) { toast("Name the route first", "bad"); return; }
  try { await api("/v1/routes/record", { method: "POST", body: { name } }); $("routeName").value = ""; closeSheet(); toast("Recording " + name + ": drive the route, then open Patrols and Save"); pollStatus(); }
  catch (err) { toast(err.message, "bad"); }
});
$("routeSave").addEventListener("click", async () => {
  try { const r = await api("/v1/routes/record/stop", { method: "POST", body: { discard: false } }); toast(`Saved ${r.name}: ${r.steps.length} steps`, "good"); loadRoutes(); }
  catch (err) { toast(err.message, "bad"); }
});
$("routeDiscard").addEventListener("click", async () => {
  try { await api("/v1/routes/record/stop", { method: "POST", body: { discard: true } }); loadRoutes(); } catch (err) { toast(err.message, "bad"); }
});

async function loadClips() {
  const list = $("clipList");
  list.replaceChildren(el("p", "muted", "Loading…"));
  try {
    const data = await api("/v1/robot-recordings");
    list.replaceChildren();
    if (!data.recordings.length) list.append(el("p", "muted", "No recordings on the robot."));
    for (const r of data.recordings) {
      const row = el("div", "clip");
      const info = el("div", "route-info");
      info.append(el("b", "", new Date(r.created).toLocaleString()), el("span", "muted", `${r.kind} · ${(r.bytes / 1e6).toFixed(1)} MB` + (r.seconds ? " · " + fmtTime(r.seconds) : "")));
      const url = "/v1/robot-recordings/" + encodeURIComponent(r.id);
      const play = el("button", "btn small-btn", r.kind === "snapshot" ? "View" : "Play");
      play.addEventListener("click", () => {
        if (row.querySelector("video,img.clip-media")) return;
        const m = r.mime_type.startsWith("image") ? el("img", "clip-media") : el("video", "clip-media");
        m.src = url; if (m.tagName === "VIDEO") { m.controls = true; m.playsInline = true; m.autoplay = true; }
        row.append(m);
      });
      const save = el("a", "btn small-btn", "Save"); save.href = url; save.download = r.name;
      const del = el("button", "btn small-btn", "Delete");
      del.addEventListener("click", async () => {
        if (!confirmTwice(del, "Sure?")) return;
        try { await api(url, { method: "DELETE" }); loadClips(); } catch (err) { toast(err.message, "bad"); }
      });
      const ctl = el("div", "route-ctl"); ctl.append(play, save, del);
      row.append(info, ctl);
      list.append(row);
    }
  } catch (err) { list.replaceChildren(el("p", "error", err.message)); }
}
$("clipsReload").addEventListener("click", loadClips);

async function loadCamera() {
  const list = $("camList");
  list.replaceChildren(el("p", "muted", "Loading…"));
  try {
    const data = await api("/v1/camera");
    list.replaceChildren();
    for (const p of data.settings) {
      const row = el("label", "setting-row cam");
      row.append(el("span", "", p.label));
      let input;
      if (p.choices) {
        input = el("select");
        p.choices.forEach((c, i) => { const o = el("option", "", c); o.value = p.min + i; input.append(o); });
        input.value = String(p.value);
      } else {
        input = el("input"); input.type = "range"; input.min = p.min; input.max = p.max; input.value = p.value;
      }
      input.addEventListener("change", async () => {
        try { await api("/v1/camera", { method: "POST", body: { name: p.name, value: Number(input.value) } }); toast(p.label + " updated", "good"); }
        catch (err) { toast(err.message, "bad"); loadCamera(); }
      });
      row.append(input);
      list.append(row);
    }
  } catch (err) { list.replaceChildren(el("p", "error", err.message)); }
}

async function loadHealth() {
  const tiles = $("healthTiles");
  tiles.replaceChildren(el("p", "muted", "Checking the robot…"));
  try {
    const h = await api("/v1/health");
    tiles.replaceChildren();
    const tile = (label, value, cls = "") => { const t = el("div", "tile " + cls); t.append(el("span", "muted", label), el("b", "", value)); tiles.append(t); };
    if (h.temperature_c != null) tile("CPU temp", h.temperature_c.toFixed(0) + "°C", h.temperature_c > 80 ? "bad" : h.temperature_c > 70 ? "warn" : "good");
    if (h.wifi_signal != null) tile("Wi-Fi " + (h.wifi_ssid || ""), h.wifi_signal + " dBm", h.wifi_signal < -78 ? "bad" : h.wifi_signal < -67 ? "warn" : "good");
    if (h.battery_volts != null) tile("Battery", h.battery_volts.toFixed(2) + " V");
    if (h.uptime != null) tile("Uptime", Math.floor(h.uptime / 86400) + "d " + Math.floor(h.uptime % 86400 / 3600) + "h");
    if (h.storage_total_kb) tile("Storage", Math.round(h.storage_used_kb / h.storage_total_kb * 100) + "%");
    if (h.memory_total_kb) tile("Memory", Math.round(h.memory_used_kb / h.memory_total_kb * 100) + "%");
    const bits = [];
    if (h.firmware) bits.push("Firmware " + h.firmware);
    if (h.load) bits.push("Load " + h.load);
    bits.push("Wheel odometry " + (h.odometry_feedback ? "working" : "not seen yet"));
    if (h.error) bits.push(h.error);
    $("healthLine").textContent = bits.join(" · ");
    drawBattery(h.battery_history || []);
  } catch (err) { tiles.replaceChildren(el("p", "error", err.message)); }
}
$("healthReload").addEventListener("click", loadHealth);
function drawBattery(samples) {
  const c = $("battChart"), ctx = c.getContext("2d"), W = c.width, H = c.height;
  ctx.clearRect(0, 0, W, H);
  ctx.strokeStyle = "rgba(255,255,255,.08)"; ctx.fillStyle = "rgba(150,158,170,.9)"; ctx.font = "12px ui-monospace, Menlo, monospace";
  for (const p of [25, 50, 75]) { const y = H - 20 - (H - 30) * p / 100; ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(W, y); ctx.stroke(); ctx.fillText(p + "%", 4, y - 4); }
  const t0 = Date.now() - 86400000;
  const x = (t) => W * (Date.parse(t) - t0) / 86400000, y = (p) => H - 20 - (H - 30) * p / 100;
  if (samples.length < 2) { ctx.textAlign = "center"; ctx.fillText("Collecting… one sample a minute while the Mac app is connected", W / 2, H / 2); ctx.textAlign = "left"; return; }
  ctx.lineWidth = 3; ctx.lineJoin = "round";
  for (let i = 1; i < samples.length; i++) {
    const a = samples[i - 1], b = samples[i];
    if (Date.parse(b.t) - Date.parse(a.t) > 600000) continue;
    ctx.strokeStyle = b.c ? "#4fd1ff" : b.p < 25 ? "#ffb020" : "#3ddc84";
    ctx.beginPath(); ctx.moveTo(x(a.t), y(a.p)); ctx.lineTo(x(b.t), y(b.p)); ctx.stroke();
  }
  ctx.fillStyle = "rgba(150,158,170,.9)";
  for (let i = 0; i <= 4; i++) { const t = new Date(t0 + i * 21600000); ctx.textAlign = i === 0 ? "left" : i === 4 ? "right" : "center"; ctx.fillText(t.toTimeString().slice(0, 5), W * i / 4, H - 4); }
  ctx.textAlign = "left";
}

async function loadSettings() {
  try {
    const [ad, og, g] = await Promise.all([api("/v1/autodock"), api("/v1/obstacle-guard"), api("/v1/guard")]);
    $("adOn").checked = !!ad.enabled; $("adAt").value = String(ad.threshold);
    $("ogOn").checked = !!og.manual;
    $("gObjects").checked = !!g.objects; $("gNotify").checked = !!g.notify; $("gSens").value = String(g.sensitivity);
  } catch (err) { toast(err.message, "bad"); }
}
$("ogOn").addEventListener("change", async () => {
  try { await api("/v1/obstacle-guard", { method: "POST", body: { manual: $("ogOn").checked } }); toast("Obstacle guard " + ($("ogOn").checked ? "on" : "off"), "good"); }
  catch (err) { toast(err.message, "bad"); }
});
for (const id of ["gObjects", "gNotify", "gSens"]) {
  $(id).addEventListener("change", async () => {
    try { await api("/v1/guard", { method: "POST", body: { objects: $("gObjects").checked, notify: $("gNotify").checked, sensitivity: Number($("gSens").value) } }); toast("Guard settings saved", "good"); }
    catch (err) { toast(err.message, "bad"); }
  });
}
$("sayBtn").addEventListener("click", async () => {
  const text = $("sayText").value.trim();
  if (!text) return;
  $("sayBtn").disabled = true;
  try { await api("/v1/say", { method: "POST", body: { text } }); $("sayText").value = ""; toast("Spoken", "good"); }
  catch (err) { toast(err.message, "bad"); }
  finally { $("sayBtn").disabled = false; }
});

// ---------- Start ----------
let started = false;
function start() {
  state.paired = true;
  if (started) { pollStatus(); startVideo(); return; }
  started = true;
  pollStatus();
  setInterval(pollStatus, 1000);
  mapTimer = setInterval(drawMap, 1000);
  startVideo();
  requestAnimationFrame(pollGamepad);
}

(async function boot() {
  setSpeed(state.speed);
  const m = location.hash.match(/[#&]pair=([^&]+)/);
  if (m) {
    history.replaceState(null, "", location.pathname);
    try { await pair(decodeURIComponent(m[1])); toast("Paired", "good"); }
    catch (err) { showPairing(); $("pairError").textContent = err.message; return; }
  }
  try {
    renderStatus(await api("/v1/status"));
    start();
  } catch (err) {
    if (err.message !== "not paired") { start(); }
  }
})();
