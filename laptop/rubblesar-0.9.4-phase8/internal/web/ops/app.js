/* RubbleSAR operator view (phase 7) and presenter (phase 8).
   No libraries: it must work on the incident network without internet.
   Data: GET /v1/scene once, then the picture as server-sent events from
   /v1/stream (src=live, replay or present). */
"use strict";
(() => {
  const Q = new URLSearchParams(location.search);
  const PRESENT = Q.get("present") === "1";
  const SRC = PRESENT ? "present" : (Q.get("src") === "replay" ? "replay" : "live");
  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s == null ? "" : s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
  const TRI = { Red: "#d32f2f", Yellow: "#f2b600", Green: "#43a047", Black: "#24272a", Grey: "#9e9e9e" };
  const RESCUED = "#2e7d32";
  const DRONE_COL = ["#2a78d6", "#eb6834", "#1baf7a", "#a35bd1", "#d1495b"];
  const reduceMotion = matchMedia("(prefers-reduced-motion: reduce)").matches;
  const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();

  let scene = null, pic = null, prevPic = null, picAt = 0, picGap = 1000, selected = null, cue = null;
  const droneColour = {};
  function colourOf(id) {
    if (!droneColour[id]) {
      const ids = ((pic && pic.drones) || []).map((d) => d.id).sort();
      let i = ids.indexOf(id);
      if (i < 0) i = Object.keys(droneColour).length;
      droneColour[id] = DRONE_COL[i % DRONE_COL.length];
    }
    return droneColour[id];
  }

  // --- Theme -------------------------------------------------------------------------------------
  function setTheme(t) {
    document.documentElement.dataset.theme = t;
    $("theme").textContent = t === "dark" ? "Light" : "Dark";
    try { localStorage.setItem("rubblesar-theme", t); } catch (e) { /* private window */ }
    map && map.redraw();
  }
  let theme = Q.get("theme");
  if (!theme) { try { theme = localStorage.getItem("rubblesar-theme"); } catch (e) { theme = null; } }
  if (!theme) theme = PRESENT || matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  document.documentElement.dataset.theme = theme;
  $("theme").addEventListener("click", () => setTheme(document.documentElement.dataset.theme === "dark" ? "light" : "dark"));
  if (PRESENT) document.body.classList.add("present");

  // --- Words ---------------------------------------------------------------------------------------
  function mmss(d) {
    const s = Math.max(0, Math.round(d / 1000));
    const h = Math.floor(s / 3600), m = Math.floor(s / 60) % 60, x = s % 60;
    return (h ? h + ":" + String(m).padStart(2, "0") : String(m).padStart(2, "0")) + ":" + String(x).padStart(2, "0");
  }
  function span(sec) {
    if (sec == null || sec < 0) return "";
    if (sec < 90) return Math.round(sec) + " s";
    const m = Math.round(sec / 60);
    if (m < 60) return m + " min";
    return Math.floor(m / 60) + " h" + (m % 60 ? " " + (m % 60) + " min" : "");
  }
  function when(ms) {
    if (!ms) return "";
    if (pic && pic.mode === "replay" && pic.start) return "+" + mmss(ms - pic.start);
    return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
  }
  const triWord = (c) => (c.prov ? c.triage + "?" : c.triage);
  const statusWords = {
    warm_unresponsive: "warm, not responding", deteriorating: "warm but cooling", warm_responsive: "warm and moving: trapped but responsive",
    mobile: "moving freely", no_heat: "at the temperature of the rubble: unconfirmed deceased", unknown: "temperature not known yet", not_found: "nothing found on close looks",
  };
  const plural = (n, one, many) => n + " " + (n === 1 ? one : (many || one + "s"));
  // q adds src=replay to the crop addresses while a replay is shown.
  function q(params) {
    const p = new URLSearchParams(params || {});
    if (pic && pic.mode === "replay") p.set("src", "replay");
    const t = p.toString();
    return t ? "?" + t : "";
  }
  function trim(s, n) { s = String(s || ""); return s.length > n ? s.slice(0, n - 1).trimEnd() + "…" : s; }

  // --- Map -----------------------------------------------------------------------------------------
  const TILE = 256;
  function proj(lat, lon, z) {
    const s = TILE * Math.pow(2, z), sin = Math.sin((lat * Math.PI) / 180);
    return [((lon + 180) / 360) * s, (0.5 - Math.log((1 + sin) / (1 - sin)) / (4 * Math.PI)) * s];
  }
  function unproj(x, y, z) {
    const s = TILE * Math.pow(2, z), lon = (x / s) * 360 - 180, n = Math.PI - (2 * Math.PI * y) / s;
    return [(180 / Math.PI) * Math.atan(0.5 * (Math.exp(n) - Math.exp(-n))), lon];
  }
  const ease = (t) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2);

  class MapView {
    constructor(canvas) {
      this.c = canvas; this.g = canvas.getContext("2d");
      this.lat = 36.2025; this.lon = 36.16056; this.zoom = 16;
      this.tiles = new Map(); this.anim = null; this.follow = null; this.dirty = true; this.userMoved = false;
      this.hits = [];
      new ResizeObserver(() => this.resize()).observe(canvas);
      this.resize();
      this.bind();
      const loop = (t) => { this.frame(t); requestAnimationFrame(loop); };
      requestAnimationFrame(loop);
    }
    resize() {
      const r = this.c.getBoundingClientRect(), dpr = window.devicePixelRatio || 1;
      this.w = r.width; this.h = r.height; this.dpr = dpr;
      this.c.width = Math.round(r.width * dpr); this.c.height = Math.round(r.height * dpr);
      this.dirty = true;
    }
    redraw() { this.dirty = true; }
    toScreen(lat, lon) {
      const p = proj(lat, lon, this.zoom), c = proj(this.lat, this.lon, this.zoom);
      return [p[0] - c[0] + this.w / 2, p[1] - c[1] + this.h / 2];
    }
    toGeo(x, y) {
      const c = proj(this.lat, this.lon, this.zoom);
      return unproj(c[0] + x - this.w / 2, c[1] + y - this.h / 2, this.zoom);
    }
    mpp() { return (156543.03392 * Math.cos((this.lat * Math.PI) / 180)) / Math.pow(2, this.zoom); }
    limits() {
      const m = scene && scene.map;
      return [m ? Math.max(12, m.min_zoom - 1) : 12, m ? m.max_zoom + 2 : 21];
    }
    setView(lat, lon, zoom) {
      const [lo, hi] = this.limits();
      this.lat = lat; this.lon = lon; this.zoom = Math.max(lo, Math.min(hi, zoom)); this.dirty = true;
    }
    flyTo(lat, lon, zoom, ms) {
      const [lo, hi] = this.limits();
      zoom = Math.max(lo, Math.min(hi, zoom));
      if (reduceMotion || !ms) { this.anim = null; this.setView(lat, lon, zoom); return; }
      this.anim = { a: { lat: this.lat, lon: this.lon, zoom: this.zoom }, b: { lat, lon, zoom }, t0: performance.now(), ms };
    }
    fitZoom(pts, pad) {
      if (!pts.length) return null;
      let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
      for (const [la, lo] of pts) { const [x, y] = proj(la, lo, 0); minX = Math.min(minX, x); maxX = Math.max(maxX, x); minY = Math.min(minY, y); maxY = Math.max(maxY, y); }
      const W = Math.max(40, this.w - 2 * pad), H = Math.max(40, this.h - 2 * pad);
      let z = scene && scene.map ? Math.min(19, scene.map.max_zoom + 0.6) : 19;
      while (z > 12 && ((maxX - minX) * Math.pow(2, z) > W || (maxY - minY) * Math.pow(2, z) > H)) z -= 0.25;
      const [lat, lon] = unproj((minX + maxX) / 2, (minY + maxY) / 2, 0);
      return { lat, lon, zoom: z };
    }
    fit(pts, ms, pad) {
      const f = this.fitZoom(pts, pad == null ? 60 : pad);
      if (f) this.flyTo(f.lat, f.lon, f.zoom, ms);
    }
    bind() {
      const c = this.c;
      let drag = null;
      c.addEventListener("pointerdown", (e) => { drag = { x: e.clientX, y: e.clientY, lat: this.lat, lon: this.lon, moved: false }; c.setPointerCapture(e.pointerId); });
      c.addEventListener("pointermove", (e) => {
        if (drag) {
          const dx = e.clientX - drag.x, dy = e.clientY - drag.y;
          if (!drag.moved && Math.hypot(dx, dy) < 4) return;
          drag.moved = true; c.classList.add("dragging"); this.anim = null; this.follow = null; this.userMoved = true;
          const p = proj(drag.lat, drag.lon, this.zoom);
          const ll = unproj(p[0] - dx, p[1] - dy, this.zoom);
          this.setView(ll[0], ll[1], this.zoom);
        } else this.hover(e);
      });
      c.addEventListener("pointerup", (e) => {
        c.classList.remove("dragging");
        if (drag && !drag.moved) this.click(e);
        drag = null;
      });
      c.addEventListener("pointerleave", () => { $("tip").hidden = true; });
      c.addEventListener("wheel", (e) => {
        e.preventDefault();
        this.zoomAround(e.offsetX, e.offsetY, this.zoom - e.deltaY * (e.deltaMode ? 0.05 : 0.0022));
      }, { passive: false });
      c.addEventListener("dblclick", (e) => this.zoomAround(e.offsetX, e.offsetY, this.zoom + 1));
      c.addEventListener("keydown", (e) => {
        const step = 80, k = e.key;
        if (k === "+" || k === "=") this.zoomAround(this.w / 2, this.h / 2, this.zoom + 0.5);
        else if (k === "-") this.zoomAround(this.w / 2, this.h / 2, this.zoom - 0.5);
        else if (k.startsWith("Arrow")) {
          const p = proj(this.lat, this.lon, this.zoom);
          const d = { ArrowLeft: [-step, 0], ArrowRight: [step, 0], ArrowUp: [0, -step], ArrowDown: [0, step] }[k];
          const ll = unproj(p[0] + d[0], p[1] + d[1], this.zoom); this.setView(ll[0], ll[1], this.zoom); this.userMoved = true;
        } else return;
        e.preventDefault();
      });
    }
    zoomAround(x, y, z) {
      this.anim = null; this.follow = null; this.userMoved = true;
      const [lo, hi] = this.limits();
      z = Math.max(lo, Math.min(hi, z));
      const g = this.toGeo(x, y);
      const p = proj(g[0], g[1], z);
      const ll = unproj(p[0] - (x - this.w / 2), p[1] - (y - this.h / 2), z);
      this.setView(ll[0], ll[1], z);
    }
    hit(e) {
      const x = e.offsetX, y = e.offsetY;
      let best = null, bd = 16;
      for (const h of this.hits) { const d = Math.hypot(h.x - x, h.y - y); if (d < bd + (h.kind === "cas" ? 2 : 0)) { bd = d; best = h; } }
      return best;
    }
    hover(e) {
      const h = this.hit(e), tip = $("tip");
      if (!h) { tip.hidden = true; this.c.style.cursor = ""; return; }
      this.c.style.cursor = "pointer";
      tip.innerHTML = h.tip; tip.hidden = false;
      const r = this.c.getBoundingClientRect();
      tip.style.left = Math.min(e.offsetX + 14, r.width - 330) + "px";
      tip.style.top = Math.min(e.offsetY + 14, r.height - 90) + "px";
    }
    click(e) {
      const h = this.hit(e);
      if (!h) return;
      if (h.kind === "cas") select(h.id, false);
      else if (h.kind === "drone") { this.follow = h.id; this.userMoved = true; }
    }
    // tile returns a loaded image for z/x/y, or null (and starts loading it).
    tile(z, x, y) {
      const m = scene && scene.map;
      if (!m) return null;
      const k = z + "/" + x + "/" + y;
      let t = this.tiles.get(k);
      if (!t) {
        t = { img: new Image(), ok: false };
        t.img.onload = () => { t.ok = true; this.dirty = true; };
        t.img.onerror = () => { t.bad = true; };
        t.img.src = m.url.replace("{z}", z).replace("{x}", x).replace("{y}", y);
        this.tiles.set(k, t);
        if (this.tiles.size > 900) { const first = this.tiles.keys().next().value; this.tiles.delete(first); }
      }
      t.used = performance.now();
      return t.ok ? t.img : null;
    }
    drawTiles(g) {
      const m = scene && scene.map;
      g.fillStyle = "#e9e6e0"; g.fillRect(0, 0, this.w, this.h);
      if (!m) return false;
      const z = Math.max(m.min_zoom, Math.min(m.max_zoom, Math.floor(this.zoom + 0.25)));
      const sc = Math.pow(2, this.zoom - z), size = TILE * sc;
      const c = proj(this.lat, this.lon, z);
      const x0 = c[0] - this.w / 2 / sc, y0 = c[1] - this.h / 2 / sc;
      const tx0 = Math.floor(x0 / TILE), ty0 = Math.floor(y0 / TILE), tx1 = Math.floor((x0 + this.w / sc) / TILE), ty1 = Math.floor((y0 + this.h / sc) / TILE);
      const n = Math.pow(2, z);
      g.imageSmoothingEnabled = true; g.imageSmoothingQuality = "high";
      for (let ty = ty0; ty <= ty1; ty++) {
        if (ty < 0 || ty >= n) continue;
        for (let tx = tx0; tx <= tx1; tx++) {
          if (tx < 0 || tx >= n) continue;
          const sx = (tx * TILE - x0) * sc, sy = (ty * TILE - y0) * sc;
          const img = this.tile(z, tx, ty);
          if (img) { g.drawImage(img, sx, sy, size + 0.5, size + 0.5); continue; }
          // While it loads: a parent tile, enlarged.
          for (let up = 1; up <= 4 && z - up >= m.min_zoom; up++) {
            const pz = z - up, f = Math.pow(2, up), px = Math.floor(tx / f), py = Math.floor(ty / f);
            const pk = this.tiles.get(pz + "/" + px + "/" + py);
            if (pk && pk.ok) {
              const part = TILE / f;
              g.drawImage(pk.img, (tx - px * f) * part, (ty - py * f) * part, part, part, sx, sy, size + 0.5, size + 0.5);
              break;
            }
          }
        }
      }
      return true;
    }
    frame(now) {
      if (this.anim) {
        const a = this.anim, t = Math.min(1, (now - a.t0) / a.ms), e = ease(t);
        // Zoom out a little on long flights so the move reads as a move.
        const pa = proj(a.a.lat, a.a.lon, 0), pb = proj(a.b.lat, a.b.lon, 0);
        const lat = unproj(pa[0] + (pb[0] - pa[0]) * e, pa[1] + (pb[1] - pa[1]) * e, 0);
        let z = a.a.zoom + (a.b.zoom - a.a.zoom) * e;
        const far = Math.hypot((pb[0] - pa[0]) * Math.pow(2, Math.min(a.a.zoom, a.b.zoom)), (pb[1] - pa[1]) * Math.pow(2, Math.min(a.a.zoom, a.b.zoom)));
        if (far > this.w) z -= Math.min(1.5, Math.log2(far / this.w)) * Math.sin(Math.PI * e);
        this.lat = lat[0]; this.lon = lat[1]; this.zoom = z; this.dirty = true;
        if (t >= 1) this.anim = null;
      } else if (this.follow && pic) {
        const d = pic.drones.find((x) => x.id === this.follow);
        if (d) {
          const p = tweened("d:" + d.id, d.lat, d.lon, now);
          this.lat += (p[0] - this.lat) * 0.12; this.lon += (p[1] - this.lon) * 0.12; this.dirty = true;
        }
      }
      if (tweensActive(now)) this.dirty = true;
      if (pulse(now)) this.dirty = true;
      if (!this.dirty) return;
      this.dirty = false;
      const g = this.g;
      g.setTransform(this.dpr, 0, 0, this.dpr, 0, 0);
      const tiles = this.drawTiles(g);
      drawOverlays(this, g, now, tiles);
    }
  }

  // Tweening of drones and teams between pictures.
  const tw = new Map();
  function setTween(key, lat, lon, now) {
    const t = tw.get(key);
    if (!t) { tw.set(key, { a: [lat, lon], b: [lat, lon], t0: now, ms: 1 }); return; }
    const cur = tweened(key, lat, lon, now);
    const jump = Math.hypot((lat - cur[0]) * 111000, (lon - cur[1]) * 90000) > 400;
    tw.set(key, { a: jump ? [lat, lon] : cur, b: [lat, lon], t0: now, ms: Math.max(150, Math.min(1500, picGap * 1.05)) });
  }
  function tweened(key, lat, lon, now) {
    const t = tw.get(key);
    if (!t) return [lat, lon];
    const k = Math.min(1, (now - t.t0) / t.ms);
    return [t.a[0] + (t.b[0] - t.a[0]) * k, t.a[1] + (t.b[1] - t.a[1]) * k];
  }
  function tweensActive(now) {
    for (const t of tw.values()) if (now - t.t0 < t.ms) return true;
    return false;
  }
  let pulseUntil = 0;
  function pulse(now) { return (selected || (cue && cue.focus)) && !reduceMotion && now < pulseUntil; }

  // --- Drawing on the map --------------------------------------------------------------------------
  function halo(g, text, x, y, colour, font) {
    g.font = font || "600 12.5px Barlow, sans-serif";
    g.lineJoin = "round"; g.lineWidth = 3.5; g.strokeStyle = "rgba(255,255,255,.92)"; g.strokeText(text, x, y);
    g.fillStyle = colour || "#1d2125"; g.fillText(text, x, y);
  }
  function poly(g, view, pts) {
    g.beginPath();
    pts.forEach((p, i) => { const [x, y] = view.toScreen(p[0], p[1]); i ? g.lineTo(x, y) : g.moveTo(x, y); });
  }
  function drawOverlays(view, g, now, tiles) {
    view.hits = [];
    if (!scene) return;
    const big = PRESENT ? 1.25 : 1;
    // Rubble and collapsed buildings when there are no map tiles (the tiles draw them).
    if (!tiles) {
      g.fillStyle = "rgba(121,85,72,.4)"; g.strokeStyle = "#5d4037"; g.lineWidth = 1.2;
      for (const r of scene.rubble) { poly(g, view, r); g.closePath(); g.fill(); g.stroke(); }
      g.fillStyle = "rgba(78,52,46,.75)";
      for (const s of scene.sites) { if (s.footprint) { poly(g, view, s.footprint); g.closePath(); g.fill(); } }
      if (view.zoom >= 15.5) {
        for (const s of scene.sites) {
          const [x, y] = view.toScreen(s.lat, s.lon), f = (PRESENT ? "600 15px" : "600 12.5px") + " Barlow, sans-serif";
          g.font = f;
          halo(g, siteName(s), x - g.measureText(siteName(s)).width / 2, y - 22 * big, "#4e342e", f);
        }
      }
    }
    // Command post (the offline map's tiles draw it themselves).
    if (scene.staging && (scene.staging[0] || scene.staging[1])) {
      const [x, y] = view.toScreen(scene.staging[0], scene.staging[1]);
      if (!tiles) {
        g.fillStyle = "#1565c0"; g.strokeStyle = "#fff"; g.lineWidth = 2;
        g.beginPath(); g.rect(x - 8 * big, y - 8 * big, 16 * big, 16 * big); g.fill(); g.stroke();
        halo(g, "Command post", x + 12 * big, y + 4, "#1565c0", (PRESENT ? "600 15px" : "600 12.5px") + " Barlow, sans-serif");
      }
      view.hits.push({ kind: "cp", x, y, tip: "<b>Command post</b><br>where the rescue teams start" });
    }
    if (!pic) return;
    const teamsBy = {};
    for (const t of pic.teams) teamsBy[t.id] = t;
    // Routes of teams on the way or on scene.
    for (const a of pic.assignments) {
      if (!a.route || a.route.length < 2) continue;
      const acc = Math.min(a.access || a.route.length - 1, a.route.length - 1);
      g.lineCap = "round"; g.lineJoin = "round";
      g.strokeStyle = "rgba(255,255,255,.85)"; g.lineWidth = 7 * big; poly(g, view, a.route); g.stroke();
      g.strokeStyle = css("--route") || "#6250d6"; g.lineWidth = 3.6 * big;
      poly(g, view, a.route.slice(0, acc + 1)); g.stroke();
      g.setLineDash([5, 5]); poly(g, view, a.route.slice(acc)); g.stroke(); g.setLineDash([]);
    }
    // A survey drone's planned tour of the collapses, numbered in the order it flies them
    // (until its survey is done): home, each site, home.
    const siteBy = {};
    for (const s of scene.sites) siteBy[s.id] = s;
    for (const d of pic.drones) {
      if (!d.survey || !d.survey.length || d.survey_done || !(d.home_lat || d.home_lon)) continue;
      const stops = d.survey.map((id) => siteBy[id]).filter(Boolean);
      if (!stops.length) continue;
      const pts = [[d.home_lat, d.home_lon], ...stops.map((s) => [s.lat, s.lon]), [d.home_lat, d.home_lon]];
      const col = colourOf(d.id);
      g.save(); g.globalAlpha = 0.75; g.strokeStyle = col; g.lineWidth = 2.2 * big; g.setLineDash([9 * big, 7 * big]); g.lineJoin = "round";
      poly(g, view, pts); g.stroke(); g.restore();
      stops.forEach((s, i) => {
        const [x, y] = view.toScreen(s.lat, s.lon), rr = 10 * big;
        g.beginPath(); g.arc(x, y, rr, 0, 2 * Math.PI); g.fillStyle = col; g.fill(); g.lineWidth = 2; g.strokeStyle = "#fff"; g.stroke();
        g.fillStyle = "#fff"; g.font = `600 ${Math.round(12.5 * big)}px Barlow, sans-serif`; g.textAlign = "center"; g.textBaseline = "middle";
        g.fillText(String(i + 1), x, y + 0.5); g.textAlign = "start"; g.textBaseline = "alphabetic";
        view.hits.push({ kind: "tour", x, y, tip: `<b>${esc(d.id)}'s survey, stop ${i + 1} of ${stops.length}</b><br>${esc(siteName(s))}: the shortest round trip from its home` });
      });
    }
    // Drone trails (the last minute or so, fading), close-look lines, drones.
    const keep = PRESENT ? 90 : 45;
    for (const d of pic.drones) {
      const col = colourOf(d.id), tr = (d.trail || []).slice(-keep);
      if (tr.length > 1) {
        g.strokeStyle = col; g.lineWidth = 2 * big; g.lineJoin = "round"; g.lineCap = "round";
        for (let i = 1; i < tr.length; i++) {
          g.globalAlpha = 0.08 + 0.5 * (i / (tr.length - 1));
          poly(g, view, [tr[i - 1], tr[i]]); g.stroke();
        }
        g.globalAlpha = 1;
      }
    }
    // Casualties: CEP, then markers, then labels.
    const zoomed = view.zoom >= 17.6 || PRESENT;
    const r = Math.max(4, Math.min(9, 3 + (view.zoom - 15) * 2.2)) * big;
    const labels = [];
    const casDrawn = [];
    for (const c of pic.casualties) {
      const [x, y] = view.toScreen(c.lat, c.lon);
      if (x < -60 || y < -60 || x > view.w + 60 || y > view.h + 60) continue;
      const col = c.rescued ? RESCUED : TRI[c.triage] || "#9e9e9e";
      const cep = c.cep / view.mpp();
      if (cep > r + 3) {
        g.beginPath(); g.arc(x, y, cep, 0, 2 * Math.PI);
        g.fillStyle = col + "1f"; g.fill(); g.strokeStyle = col + "66"; g.lineWidth = 1; g.stroke();
      }
      casDrawn.push({ c, x, y, col });
    }
    // Close-look lines (drone to target) under the markers.
    for (const d of pic.drones) {
      if (!d.task || !["en_route", "on_station", "assigned"].includes(d.task.state)) continue;
      const p = tweened("d:" + d.id, d.lat, d.lon, now);
      const [x1, y1] = view.toScreen(p[0], p[1]), [x2, y2] = view.toScreen(d.task.lat, d.task.lon);
      g.strokeStyle = colourOf(d.id); g.lineWidth = 1.6 * big; g.setLineDash([4, 4]);
      g.beginPath(); g.moveTo(x1, y1); g.lineTo(x2, y2); g.stroke(); g.setLineDash([]);
    }
    const focus = (cue && cue.focus) || selected;
    for (const { c, x, y, col } of casDrawn) {
      const rr = c.id === focus ? r * 1.25 : r;
      if (c.id === focus) {
        const ph = reduceMotion ? 0.5 : (now / 900) % 1;
        g.beginPath(); g.arc(x, y, rr + 4 + ph * 14, 0, 2 * Math.PI);
        g.strokeStyle = css("--signal") || "#1f5fd1"; g.globalAlpha = 1 - ph; g.lineWidth = 3; g.stroke(); g.globalAlpha = 1;
        pulseUntil = performance.now() + 2000;
      }
      g.beginPath(); g.arc(x, y, rr, 0, 2 * Math.PI);
      if (c.prov && !c.rescued) {
        g.fillStyle = "#fff"; g.fill(); g.lineWidth = Math.max(3, rr * 0.55); g.strokeStyle = col; g.stroke();
      } else {
        g.fillStyle = col; g.fill(); g.lineWidth = 2.2; g.strokeStyle = "#fff"; g.stroke();
      }
      if (c.rescued && rr >= 6) {
        g.strokeStyle = "#fff"; g.lineWidth = 2; g.beginPath(); g.moveTo(x - rr * 0.45, y); g.lineTo(x - rr * 0.1, y + rr * 0.35); g.lineTo(x + rr * 0.5, y - rr * 0.35); g.stroke();
      }
      const state = c.rescued ? "freed" : triWord(c);
      view.hits.push({ kind: "cas", id: c.id, x, y, tip: `<b>${esc(c.id)} ${esc(state)}</b><br>${esc(trim(c.reason, 140))}` });
      if (zoomed || c.id === focus) labels.push({ x, y, text: c.id, col: "#1d2125", r: rr, prio: c.id === focus ? 0 : c.need_team && !c.rescued ? 1 : 2 });
    }
    // Teams.
    for (const t of pic.teams) {
      if (!t.lat && !t.lon) continue;
      const p = tweened("t:" + t.id, t.lat, t.lon, now);
      const [x, y] = view.toScreen(p[0], p[1]);
      const s = 7 * big;
      g.fillStyle = t.state === "offline" ? "#9e9e9e" : css("--route") || "#6250d6"; g.strokeStyle = "#fff"; g.lineWidth = 2;
      g.beginPath(); g.roundRect ? g.roundRect(x - s, y - s, 2 * s, 2 * s, 3) : g.rect(x - s, y - s, 2 * s, 2 * s); g.fill(); g.stroke();
      if (t.source === "tak") { g.fillStyle = "#fff"; g.beginPath(); g.arc(x, y, 2.2 * big, 0, 2 * Math.PI); g.fill(); }
      labels.push({ x, y, text: t.callsign + (t.source === "tak" ? " (TAK)" : ""), col: css("--route") || "#6250d6", r: s, prio: 1 });
      view.hits.push({ kind: "team", id: t.id, x, y, tip: `<b>${esc(t.callsign)}</b> ${esc(teamWords(t))}` });
    }
    // Drones on top.
    for (const d of pic.drones) {
      const p = tweened("d:" + d.id, d.lat, d.lon, now);
      const [x, y] = view.toScreen(p[0], p[1]);
      const col = d.stale ? "#8a8a8a" : colourOf(d.id), L = 13 * big;
      g.save(); g.translate(x, y); g.rotate(((d.yaw || 0) * Math.PI) / 180);
      g.beginPath(); g.moveTo(0, -L); g.lineTo(L * 0.72, L * 0.7); g.lineTo(0, L * 0.32); g.lineTo(-L * 0.72, L * 0.7); g.closePath();
      g.fillStyle = col; g.fill(); g.lineWidth = 2; g.strokeStyle = "#fff";
      if (d.stale) g.setLineDash([3, 2]);
      g.stroke(); g.setLineDash([]); g.restore();
      const lab = d.id + (d.stale ? " (not heard)" : airborne(d.state) ? " " + Math.round(d.alt) + " m" : "");
      labels.push({ x, y, text: lab, col, r: L, prio: 0 });
      view.hits.push({ kind: "drone", id: d.id, x, y, tip: `<b>${esc(d.id)}</b> ${esc(droneWords(d))}<br>battery ${Math.round(d.battery)}%` });
    }
    // Labels, avoiding each other.
    labels.sort((a, b) => a.prio - b.prio);
    const boxes = [];
    const font = (PRESENT ? "600 15px" : "600 12.5px") + " Barlow, sans-serif";
    g.font = font;
    for (const l of labels) {
      const w = g.measureText(l.text).width + 4, hgt = PRESENT ? 17 : 14;
      for (const [dx, dy] of [[l.r + 5, 4], [l.r + 5, hgt + 2], [-l.r - 5 - w, 4], [-w / 2, -l.r - 5], [-w / 2, l.r + hgt]]) {
        const b = [l.x + dx, l.y + dy - hgt + 2, l.x + dx + w, l.y + dy + 3];
        if (boxes.some((o) => b[0] < o[2] && b[2] > o[0] && b[1] < o[3] && b[3] > o[1])) continue;
        boxes.push(b); halo(g, l.text, l.x + dx, l.y + dy, l.col, font); break;
      }
    }
  }
  const airborne = (s) => !["", "landed", "unspecified", "standby", "idle"].includes(s || "");

  // --- Words for drones and teams --------------------------------------------------------------
  function droneWords(d) {
    if (d.stale) return "not heard for " + span((pic.now - d.seen) / 1000);
    if (d.task && ["assigned", "en_route"].includes(d.task.state)) return "flying to " + d.task.casualty + " for a close look";
    if (d.task && d.task.state === "on_station") return "watching " + d.task.casualty + " close up" + (d.task.hover_s ? ` (${Math.round(d.task.elapsed_s || 0)} of ${d.task.hover_s} s)` : "");
    if (d.state === "survey") return "surveying" + (d.steps ? `, leg ${d.step} of ${d.steps}` : "");
    if (d.survey_done && d.role === "scout" && !airborne(d.state)) return "survey done, landed";
    return { landed: "landed", takeoff: "taking off", transit: "flying", hover: "holding position", return: "flying home", landing: "landing", investigate: "close look", standby: "waiting at home" }[d.state] || d.state;
  }
  function teamWords(t) {
    const a = pic.assignments.find((x) => x.team === t.id && (x.state === "en_route" || x.state === "on_scene"));
    if (t.state === "offline") return "offline (no position from the device)";
    if (a && a.state === "en_route") return `on the way to ${a.casualty} (${Math.round(a.length_m)} m, about ${span(a.eta_s)} ${t.mode === "vehicle" ? "by vehicle" : "on foot"})`;
    if (a && a.state === "on_scene") return `freeing ${a.casualty}` + (a.extrication_s ? `, about ${span(a.extrication_s)} for real rescuers` : "");
    return "free";
  }
  function casualtyState(c) {
    if (c.rescued) return { text: "freed by " + c.team, cls: "ok" };
    if (c.need_team) {
      if (c.asg_state === "en_route") return { text: c.team + " on the way", cls: "" };
      if (c.asg_state === "on_scene") return { text: c.team + " freeing", cls: "" };
      return { text: "waiting for a team", cls: "wait" };
    }
    if (c.triage === "Grey") {
      const t = pic.tasks.find((x) => x.casualty === c.id && ["assigned", "en_route", "on_station"].includes(x.state));
      return { text: t ? "close look under way" : c.needs_look ? "needs a close look" : "", cls: "" };
    }
    if (c.triage === "Black") return { text: "kept for recovery", cls: "" };
    return { text: "", cls: "" };
  }

  // --- Panels --------------------------------------------------------------------------------------
  // setRows keeps one element per item (by key), updating only what changed,
  // so images do not reload and the list does not flicker.
  const rowCache = new Map();
  function setRows(el, items, o) {
    let prev = null;
    for (const it of items) {
      const k = o.key(it), ck = el.id + ":" + k;
      let node = rowCache.get(ck);
      const live = !o.inert || !o.inert(it);
      if (!node) {
        node = document.createElement("div");
        node.__ck = ck;
        if (o.click && live) {
          node.tabIndex = 0; node.setAttribute("role", "button");
          node.addEventListener("click", () => o.click(k));
          node.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); o.click(k); } });
        }
        rowCache.set(ck, node);
      }
      const c = typeof o.cls === "function" ? o.cls(it) : o.cls;
      if (node.className !== c) node.className = c;
      const h = o.html(it);
      if (node.__h !== h) { node.innerHTML = h; node.__h = h; }
      if (o.title) { const t = o.title(it); if (node.title !== t) node.title = t; }
      const next = prev ? prev.nextSibling : el.firstChild;
      if (next !== node) el.insertBefore(node, next);
      prev = node;
    }
    let n = prev ? prev.nextSibling : el.firstChild;
    while (n) { const nx = n.nextSibling; if (n.__ck) rowCache.delete(n.__ck); el.removeChild(n); n = nx; }
  }
  function clearEmpty(el) { if (el.firstChild && el.firstChild.classList && el.firstChild.classList.contains("empty")) el.innerHTML = ""; }
  function showEmpty(el, text) {
    for (const n of el.children) if (n.__ck) rowCache.delete(n.__ck);
    const h = text ? `<div class="empty">${esc(text)}</div>` : "";
    if (el.__empty !== h || el.children.length !== (h ? 1 : 0)) { el.innerHTML = h; el.__empty = h; }
  }

  function group(c) {
    if (c.need_team && !c.rescued) return 0;
    if (c.triage === "Grey") return 1;
    if (c.rescued) return 2;
    return 3;
  }
  const groupNames = ["Need a rescue team", "Need a close look", "Freed", "No team needed"];

  function renderCasualties() {
    const el = $("cas");
    const list = pic.casualties.slice().sort((a, b) => group(a) - group(b));
    if (!list.length) { showEmpty(el, pic.mode === "off" ? "" : "No casualty records yet. The scout reports body parts as it surveys; each record is one person (or something taken for one)."); return; }
    clearEmpty(el);
    const max = PRESENT ? 9 : 400;
    const items = [];
    let g = -1;
    for (const c of list) {
      if (items.length >= max) break;
      if (group(c) !== g) { g = group(c); items.push({ head: groupNames[g], n: list.filter((x) => group(x) === g).length, __id: "h" + g }); }
      items.push(c);
    }
    const rest = list.length - items.filter((x) => x.id).length;
    if (rest > 0) items.push({ more: rest, __id: "more" });
    setRows(el, items, { key: (it) => it.id || it.__id, inert: (it) => !it.id, html: (it) => {
      if (it.head) return `<span class="grp">${esc(it.head)} <span class="sub">${it.n}</span></span>`;
      if (it.more != null) return `<span class="sub">and ${it.more} more</span>`;
      const st = casualtyState(it);
      const chipCls = it.rescued ? "Rescued" : it.triage + (it.prov ? " prov" : "");
      const chip = `<span class="chip ${chipCls}" style="--c:${TRI[it.triage] || "#999"}">${esc(it.rescued ? "Freed" : triWord(it))}</span>`;
      return `${chip}<span class="id">${esc(it.id)}</span><span class="st ${st.cls}">${esc(st.text)}</span><span class="why">${esc(trim(it.reason, PRESENT ? 80 : 110))}</span>`;
    }, cls: (it) => (!it.id ? "ghead" : "row" + (it.id === selected ? " sel" : "")), click: (id) => select(id, true) });
  }

  function renderDrones() {
    const el = $("drones");
    if (!pic.drones.length) { showEmpty(el, pic.mode === "off" ? "" : "No drone has reported. Is edge-eye running on the Jetson? (rubblesar status)"); return; }
    clearEmpty(el);
    setRows(el, pic.drones, { key: (d) => d.id, html: (d) => {
      const l = d.link || {}, pc = d.perc, known = !!(l.budget_bps || l.used_bps || l.sent_bytes);
      const used = l.budget_bps ? Math.min(100, (100 * (l.used_bps || 0)) / l.budget_bps) : 0;
      const kb = (b) => (b >= 1000 ? Math.round(b / 1000) : Math.round(b / 100) / 10);
      let warn = "";
      if (d.stale) warn = "Link lost: not heard for " + span((pic.now - d.seen) / 1000) + ". Its reports wait on board.";
      else if (l.degrade > 0) warn = "Link falling behind: sending " + ["full reports", "small crops", "thermal crops only", "metadata only"][l.degrade] + ".";
      else if ((l.queued || 0) + (l.unacked || 0) > 8) warn = `${(l.queued || 0) + (l.unacked || 0)} reports waiting to be acknowledged.`;
      const pipe = PRESENT && pc ? `<span class="what">${esc(shortBackend(pc.backend))}, ${pc.hz ? pc.hz.toFixed(1) : "0"} frames/s, ${Math.round(pc.seg_ms || 0)} ms to segment a frame</span>` : "";
      return `<span class="dot" style="background:${colourOf(d.id)}"></span><span><b>${esc(d.id)}</b> <span class="sub">${esc(d.role)}</span></span><span class="st">${esc(droneWords(d))}</span>` +
        `<div class="meters"><span>battery</span><span class="meter bat${d.battery < 30 ? " low" : ""}"><i style="width:${Math.max(0, Math.min(100, d.battery))}%"></i></span>` +
        `<span>radio</span><span class="meter" title="${kb(l.used_bps || 0)} of ${l.budget_bps ? kb(l.budget_bps) + " kbit/s" : "unlimited"}"><i style="width:${used}%"></i></span>` +
        `<span>${Math.round(d.battery)}%</span><span></span><span>${known ? `${kb(l.used_bps || 0)} of ${l.budget_bps ? kb(l.budget_bps) : "∞"} kbit/s` : "not reported"}</span><span>${plural(d.stored, "report")} here</span></div>` +
        (warn ? `<span class="warnline">${esc(warn)}</span>` : "") + pipe;
    }, cls: "drone", click: (id) => { map.follow = id; map.userMoved = true; } });
  }
  function shortBackend(b) {
    if (!b) return "model";
    if (/tensorrt/i.test(b)) return "TensorRT on the Jetson GPU";
    if (/onnx/i.test(b)) return "ONNX Runtime";
    if (/oracle/i.test(b)) return "truth oracle (test)";
    return b;
  }

  function renderTeams() {
    const el = $("teams");
    if (!pic.teams.length) { showEmpty(el, pic.mode === "off" ? "" : "No rescue teams yet (rescue-dispatch)."); return; }
    clearEmpty(el);
    setRows(el, pic.teams, { key: (t) => t.id, html: (t) => {
      const src = t.source === "tak" ? "TAK device" : "simulated";
      return `<span class="dot" style="${t.state === "offline" ? "background:#9e9e9e" : ""}"></span><span><b>${esc(t.callsign)}</b> <span class="sub">${esc(t.mode)}, ${src}</span></span>` +
        `<span class="st">${t.done ? plural(t.done, "rescue") : ""}</span><span class="what">${esc(teamWords(t))}</span>`;
    }, cls: "team", click: (id) => {
      const t = pic.teams.find((x) => x.id === id);
      if (t && (t.lat || t.lon)) { map.follow = null; map.flyTo(t.lat, t.lon, Math.max(map.zoom, 17.5), 900); }
    } });
  }

  let tlKeys = [];
  function renderTimeline() {
    const el = $("timeline");
    const items = pic.timeline.slice(-160);
    const keys = items.map((i) => i.t + "|" + i.text);
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
    let same = 0;
    while (same < tlKeys.length && same < keys.length && tlKeys[same] === keys[same]) same++;
    if (same < tlKeys.length) { el.innerHTML = ""; same = 0; }
    const fresh = tlKeys.length > 0;
    for (let i = same; i < items.length; i++) {
      const it = items[i], li = document.createElement("li");
      if (fresh) li.className = "new";
      li.innerHTML = `<time>${esc(when(it.t))}</time><span class="tone ${esc(it.tone || "info")}"></span><span>${esc(it.text)}</span>`;
      if (it.ref) { li.style.cursor = "pointer"; li.addEventListener("click", () => select(it.ref, true)); }
      el.appendChild(li);
    }
    tlKeys = keys;
    if (!items.length) el.innerHTML = `<li class="empty">Nothing yet.</li>`, tlKeys = [];
    if (atBottom || !fresh) el.scrollTop = el.scrollHeight;
  }

  function renderThumbs() {
    const el = $("thumbs");
    const ds = pic.detections.slice(0, 24);
    const k = pic.kpi;
    $("repsum").textContent = k.detections ? `${k.detections} received, ${Math.round(k.bytes / 1024)} KB` : "";
    if (!ds.length) { showEmpty(el, "Reports appear here as the drones send them: a small crop of each body part, its temperature and where it is."); return; }
    clearEmpty(el);
    setRows(el, ds, { key: (d) => d.id, html: (d) => {
      const img = d.crop ? `<img src="/v1/crop/${encodeURIComponent(d.id)}${q()}" alt="" loading="lazy">` : d.therm ? `<img src="/v1/thermal/${encodeURIComponent(d.id)}${q()}" alt="" loading="lazy">` : `<div class="meta">metadata only (link busy)</div>`;
      const pct = d.conf ? Math.round(d.conf * 100) + "%" : "";
      return `${img}<div class="cap">${esc(d.part)} ${pct}</div>`;
    }, cls: "thumb", title: (d) => `${d.uav}: ${d.summary || d.part} (${d.payload || "full"} report, ${d.bytes} bytes)`,
      click: (id) => { const d = pic.detections.find((x) => x.id === id); if (d && d.casualty) select(d.casualty, true); } });
  }

  // --- What the drones found (operator view) ------------------------------------------------------
  // One card per casualty record, with the pictures the drones sent and
  // what the model and the thermal camera made of them, most urgent first.
  let showQuiet = false;
  const PART_ORDER = ["head", "torso", "arm", "hand", "leg"];
  // siteName: a collapse site as people say it ("Gündüz Caddesi collapse").
  function siteName(s) { return (s.name || s.id) + " collapse"; }
  // siteOf: the collapse site a casualty is in (within 80 m of its centre), or null.
  function siteOf(c) {
    if (!scene || !scene.sites) return null;
    let best = null, bd = 80;
    for (const s of scene.sites) {
      const dy = (c.lat - s.lat) * 111320, dx = (c.lon - s.lon) * 111320 * Math.cos((s.lat * Math.PI) / 180), d = Math.hypot(dx, dy);
      if (d < bd) { best = s; bd = d; }
    }
    return best;
  }
  function partsWords(c) {
    const best = {};
    for (const p of c.parts || []) if (!best[p.class] || p.conf > best[p.class]) best[p.class] = p.conf;
    const ks = Object.keys(best).sort((a, b) => best[b] - best[a]);
    return ks.map((k) => `<b>${esc(k)}</b>${best[k] > 0 ? " " + Math.round(best[k] * 100) + "%" : ""}`).join(" · ");
  }
  function readWords(c) {
    const t = c.thermal || {}, out = [];
    if (t.readable) out.push(`${t.skin_c.toFixed(1)} °C, ${t.delta_c >= 0 ? "+" : ""}${t.delta_c.toFixed(1)} °C against the rubble`);
    else out.push("temperature not readable");
    if (c.watched_s) out.push(c.moved ? "moved while watched" : `still for ${Math.round(c.watched_s)} s close up`);
    return out.join(" · ");
  }
  function cardOrder(a, b) {
    const g = group(a) - group(b);
    if (g) return g;
    if (group(a) === 0) {
      const r = (a.triage === "Red" ? 0 : 1) - (b.triage === "Red" ? 0 : 1);
      if (r) return r;
      const w = (a.asg_state ? 1 : 0) - (b.asg_state ? 1 : 0);
      if (w) return w;
    }
    return (b.last || b.first || 0) - (a.last || a.first || 0);
  }
  function cardImgs(c) {
    if (!c.best) {
      const what = c.thermal && c.thermal.readable ? "a warm or cold spot in the thermal image; no body part outlined" : "no picture: the report carried its position only";
      return `<figure><div class="none">${esc(what)}</div></figure><figure></figure>`;
    }
    const det = pic.detections.find((d) => d.id === c.best);
    const t = c.thermal || {};
    const outline = det && det.outline && det.outline.length >= 6 ? `<svg viewBox="0 0 1 1" preserveAspectRatio="none" aria-hidden="true"><polygon points="${pairs(det.outline)}" fill="none" stroke="#6ea2ff" vector-effect="non-scaling-stroke" style="stroke-width:2px"/></svg>` : "";
    const lo = t.readable ? Math.floor(t.ambient_c - 2) : "", hi = t.readable ? Math.ceil(Math.max(t.skin_c + 3, t.ambient_c + 8)) : "";
    const lab = det ? esc(det.part) + (det.conf > 0 ? ` ${Math.round(det.conf * 100)}%` : "") : "";
    return `<figure><img src="/v1/crop/${encodeURIComponent(c.best)}${q()}" alt="What the drone's camera saw of ${esc(c.id)}" loading="lazy" onerror="this.parentNode.innerHTML='<div class=none>colour picture not sent: the link was busy</div>'">${outline}${lab ? `<span class="lab">${lab}</span>` : ""}</figure>` +
      `<figure><img src="/v1/thermal/${encodeURIComponent(c.best)}${q(t.readable ? { lo, hi } : {})}" alt="Thermal picture of ${esc(c.id)}" loading="lazy" onerror="this.parentNode.innerHTML='<div class=none>no thermal picture</div>'">${t.readable ? `<span class="lab">${t.skin_c.toFixed(1)} °C</span>` : ""}</figure>`;
  }
  function cardText(c) {
    const st = casualtyState(c);
    const chipCls = c.rescued ? "Rescued" : c.triage + (c.prov ? " prov" : "");
    const parts = partsWords(c);
    const site = siteOf(c);
    const where = site ? ` · ${esc(siteName(site))}` : "";
    const saw = parts ? `${parts} <span class="sub">seen by ${esc((c.uavs || []).join(", "))}${where}</span>` : `<span class="sub">no body part outlined; seen by ${esc((c.uavs || []).join(", "))}${where}</span>`;
    return `<div class="top"><span class="chip ${chipCls}" style="--c:${TRI[c.triage] || "#999"}">${esc(c.rescued ? "Freed" : triWord(c))}</span><span class="cid">${esc(c.id)}</span><span class="st ${st.cls}">${esc(st.text)}</span></div>` +
      `<div class="saw">${saw}</div><div class="read">${esc(readWords(c))}</div><div class="why">${esc(c.reason)}</div>`;
  }
  const cardCache = new Map();
  function renderFound() { foundInto($("cards"), $("foundsum"), { quiet: true }); }
  // foundInto renders the cards into el (and the summary into sumEl):
  // o.quiet: the "no team needed" group folds away; o.max: at most this many cards.
  function foundInto(el, sumEl, o) {
    let list = pic.casualties.slice().sort(cardOrder);
    const k = pic.kpi;
    // Summary: what the model found, in the drones' reports.
    const by = {};
    for (const c of list) for (const p of c.parts || []) by[p.class] = (by[p.class] || 0) + 1;
    const partsSum = PART_ORDER.filter((x) => by[x]).map((x) => `${by[x]} ${x}${by[x] === 1 ? "" : "s"}`).join(", ");
    const be = pic.drones.map((d) => d.perc && d.perc.backend).find((b) => b);
    const sum = !list.length ? "" : `<b>${list.length}</b> ${list.length === 1 ? "person or possible person" : "people or possible people"} from <b>${k.detections || 0}</b> drone reports` +
      (partsSum ? `; body parts outlined: ${partsSum}` : "") + (be ? `. Model: ${esc(shortBackend(be))}.` : ".");
    if (sumEl && sumEl.__h !== sum) { sumEl.innerHTML = sum; sumEl.__h = sum; }
    if (o.max) list = list.filter((c) => group(c) !== 3).slice(0, o.max);
    if (!list.length) {
      for (const n of el.children) if (n.__ck) cardCache.delete(n.__ck);
      showEmpty(el, pic.mode === "off" ? "" : "Nothing found yet. As the scout surveys, every body part its camera picks out appears here: the picture it sent, what the model outlined, the temperature, and what happens next.");
      return;
    }
    clearEmpty(el);
    const items = [];
    let g = -1;
    for (const c of list) {
      if (group(c) !== g) {
        g = group(c);
        const n = list.filter((x) => group(x) === g).length;
        if (!o.max) items.push({ head: groupNames[g], n, g, key: "h" + g });
      }
      if (g === 3 && o.quiet && !showQuiet) continue;
      items.push(c);
    }
    let prev = null;
    for (const it of items) {
      const key = el.id + ":" + (it.key || it.id);
      let node = cardCache.get(key);
      if (!node) {
        node = document.createElement("div");
        node.__ck = key;
        if (it.id) {
          node.innerHTML = `<div class="imgs"></div><div class="txt"></div>`;
          node.tabIndex = 0; node.setAttribute("role", "button");
          const id = it.id;
          node.addEventListener("click", () => select(id, true));
          node.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); select(id, true); } });
        }
        cardCache.set(key, node);
      }
      if (it.head) {
        const more = it.g === 3 && o.quiet ? `<button class="more" type="button">${showQuiet ? "hide" : "show"}</button>` : "";
        const h = `<h3>${esc(it.head)} <span class="sub">${it.n}</span></h3>${more}`;
        if (node.__h !== h) {
          node.innerHTML = h; node.__h = h;
          const b = node.querySelector(".more");
          if (b) b.addEventListener("click", () => { showQuiet = !showQuiet; renderFound(); });
        }
        if (node.className !== "gh") node.className = "gh";
      } else {
        const cls = "card" + (group(it) === 0 ? " urgent" + (it.triage === "Yellow" ? " y" : "") : "") + (group(it) === 3 ? " quiet" : "") + (it.id === selected ? " sel" : "");
        if (node.className !== cls) node.className = cls;
        const im = cardImgs(it), tx = cardText(it);
        const a = node.firstChild, b = node.lastChild;
        if (a.__h !== im) { a.innerHTML = im; a.__h = im; }
        if (b.__h !== tx) { b.innerHTML = tx; b.__h = tx; }
      }
      const next = prev ? prev.nextSibling : el.firstChild;
      if (next !== node) el.insertBefore(node, next);
      prev = node;
    }
    let n = prev ? prev.nextSibling : el.firstChild;
    while (n) { const nx = n.nextSibling; if (n.__ck) cardCache.delete(n.__ck); el.removeChild(n); n = nx; }
  }

  // The operator view's layout: the map and its tabs beside the findings.
  function setupOperatorLayout() {
    const lower = $("lower"), tabs = $("tabs");
    for (const id of ["drones", "teams"]) {
      const sec = $(id).closest(".block");
      sec.classList.add("tab-" + id);
      lower.appendChild(sec);
    }
    $("main").appendChild($("detail"));
    const blocks = [["time", "What happened", lower.querySelector(".feed")], ["chat", "Chat with the teams", lower.querySelector(".chat")],
      ["drones", "Drones", lower.querySelector(".tab-drones")], ["teams", "Rescue teams", lower.querySelector(".tab-teams")]];
    let on = "time";
    try { on = localStorage.getItem("rubblesar-tab") || on; } catch (e) { /* private window */ }
    if (!blocks.some((b) => b[0] === on)) on = "time";
    tabs.innerHTML = blocks.map(([k, t]) => `<button type="button" role="tab" id="tab-${k}" data-k="${k}">${esc(t)}<span class="n" id="n-${k}"></span></button>`).join("");
    const show = (k) => {
      for (const [kk, , sec] of blocks) {
        sec.classList.toggle("on", kk === k);
        sec.setAttribute("role", "tabpanel");
        const b = $("tab-" + kk);
        b.setAttribute("aria-selected", kk === k ? "true" : "false");
        b.tabIndex = kk === k ? 0 : -1;
      }
      try { localStorage.setItem("rubblesar-tab", k); } catch (e) { /* private window */ }
    };
    tabs.addEventListener("click", (e) => { const b = e.target.closest("button[data-k]"); if (b) show(b.dataset.k); });
    tabs.addEventListener("keydown", (e) => {
      if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
      const ks = blocks.map((b) => b[0]), i = ks.indexOf(document.activeElement.dataset.k);
      const k = ks[(i + (e.key === "ArrowRight" ? 1 : ks.length - 1)) % ks.length];
      show(k); $("tab-" + k).focus();
    });
    tabs.hidden = false;
    show(on);
  }
  if (!PRESENT) setupOperatorLayout();

  function renderChat() {
    const el = $("chat");
    const cs = pic.chats.slice(-60);
    if (!cs.length) { el.innerHTML = `<li class="empty">rescue-dispatch (SAR-OPS) sends each team its casualty as a chat message; teams on ATAK or WinTAK answer "ok", "done" or "unable".</li>`; return; }
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
    el.innerHTML = cs.map((c) => `<li><time>${esc(when(c.at))}</time> <span class="who">${esc(c.from)}</span> <span class="to">to ${esc(c.to)}</span>: ${esc(c.text)}</li>`).join("");
    if (atBottom) el.scrollTop = el.scrollHeight;
  }

  function renderHeader() {
    const k = pic.kpi, m = $("mode");
    $("incident").textContent = (scene && scene.name) || pic.incident || "";
    if (pic.mode === "replay") {
      const r = pic.replay;
      m.className = "mode replay"; m.textContent = r ? `Replay ×${r.speed}${r.playing ? "" : ", paused"}` : "Replay";
    } else if (pic.mode === "live" && pic.bus === "connected") { m.className = "mode live"; m.textContent = "Live"; }
    else { m.className = "mode down"; m.textContent = pic.mode === "off" ? "Not running" : "Waiting"; m.title = pic.bus || ""; }
    let s = "";
    if (pic.mode === "off" || (pic.mode === "live" && pic.bus !== "connected")) s = pic.bus ? "Live view: " + pic.bus : "";
    else if (!k.records) {
      const sc = pic.drones.find((d) => d.role === "scout" && d.state === "survey");
      s = "No casualties found yet." + (sc ? ` ${sc.id} is surveying${sc.steps ? ", leg " + sc.step + " of " + sc.steps : ""}.` : "");
    } else {
      const open = k.need_team - k.rescued;
      if (open > 0) {
        const parts = [];
        if (k.waiting) parts.push(`<b>${k.waiting}</b> waiting for a team`);
        if (k.teams_en_route) parts.push(`${plural(k.teams_en_route, "team")} on the way`);
        if (k.teams_on_scene) parts.push(`${k.teams_on_scene} on scene`);
        s = `<b>${open}</b> ${open === 1 ? "person needs" : "people need"} rescuing${parts.length ? ": " + parts.join(", ") : ""}.`;
      } else s = k.need_team ? "Everyone who needed a team has been reached." : "Nobody needs a rescue team at the moment.";
      if (k.rescued) s += ` <b>${k.rescued}</b> freed so far.`;
      if (k.looks_open) s += ` ${plural(k.looks_open, "close look")} under way.`;
    }
    $("situation").innerHTML = s;
    // Triage tape.
    const segs = [];
    const by = k.by_triage || {};
    const redOpen = pic.casualties.filter((c) => c.triage === "Red" && !c.rescued).length, yelOpen = pic.casualties.filter((c) => c.triage === "Yellow" && !c.rescued).length;
    if (redOpen) segs.push(["red", redOpen, `${redOpen} Red`]);
    if (yelOpen) segs.push(["yellow", yelOpen, `${yelOpen} Yellow`]);
    if (k.rescued) segs.push(["rescued", k.rescued, `${k.rescued} freed`]);
    if (by.Green) segs.push(["green", by.Green, `${by.Green} Green`]);
    if (by.Grey) segs.push(["grey", by.Grey, `${by.Grey} Grey`]);
    if (by.Black) segs.push(["black", by.Black, `${by.Black} Black`]);
    const bg = { red: TRI.Red, yellow: TRI.Yellow, rescued: RESCUED, green: TRI.Green, grey: TRI.Grey, black: "#24272a" };
    $("tape").innerHTML = segs.map(([c, n, t]) => `<div class="t-${c}" style="flex-grow:${n};background:${bg[c]}" title="${esc(t)}">${esc(t)}</div>`).join("");
  }

  function renderLegend() {
    const drones = pic ? pic.drones.map((d) => `<span><span class="ln" style="background:${colourOf(d.id)}"></span>${esc(d.id)}</span>`).join("") : "";
    const rubble = scene && !scene.map ? `<span><span class="sw sq" style="background:rgba(121,85,72,.6)"></span>rubble</span>` : "";
    $("legend").innerHTML = `<span><span class="sw" style="background:${TRI.Red}"></span>Red</span><span><span class="sw" style="background:${TRI.Yellow}"></span>Yellow</span>` +
      `<span><span class="sw" style="background:${TRI.Grey}"></span>Grey</span><span><span class="sw" style="background:${TRI.Black}"></span>Black</span>` +
      `<span><span class="sw" style="background:#fff;box-shadow:inset 0 0 0 3px ${TRI.Red}"></span>provisional</span><span><span class="sw" style="background:${RESCUED}"></span>freed</span>` +
      `<span><span class="sw sq" style="background:${css("--route")}"></span>team, route</span>` + drones + rubble +
      (pic && pic.drones.some((d) => d.survey && d.survey.length && !d.survey_done) ? `<span><span class="ln dash" style="border-color:${colourOf((pic.drones.find((d) => d.survey && d.survey.length) || {}).id)}"></span>planned survey</span>` : "");
  }

  // --- Detail ---------------------------------------------------------------------------------------
  function select(id, fly) {
    selected = id;
    pulseUntil = performance.now() + 4000;
    const c = pic && pic.casualties.find((x) => x.id === id);
    if (c && fly) { map.follow = null; map.flyTo(c.lat, c.lon, Math.max(map.zoom, 18.5), 900); }
    renderDetail();
    renderCasualties();
    if (!PRESENT && pic) renderFound();
    map.redraw();
  }
  $("map").addEventListener("keydown", (e) => { if (e.key === "Escape") closeDetail(); });
  function closeDetail() { selected = null; $("detail").hidden = true; renderCasualties(); if (!PRESENT && pic) renderFound(); map.redraw(); }
  function renderTabCounts() {
    const set = (k, v) => { const e = $("n-" + k); if (e && e.textContent !== v) e.textContent = v; };
    set("chat", pic.chats.length ? String(pic.chats.length) : "");
    set("drones", pic.drones.length ? String(pic.drones.length) : "");
    set("teams", pic.teams.length ? String(pic.teams.length) : "");
  }

  function renderDetail() {
    const el = $("detail");
    const c = selected && pic && pic.casualties.find((x) => x.id === selected);
    if (!c || PRESENT && !(cue && cue.panel === "casualty")) { el.hidden = true; return; }
    el.hidden = false;
    el.innerHTML = casualtyHTML(c, false);
    const b = el.querySelector(".close");
    if (b) b.addEventListener("click", closeDetail);
  }

  function casualtyHTML(c, big) {
    const t = c.thermal || {};
    const det = pic.detections.find((d) => d.id === c.best);
    const outline = det && det.outline && det.outline.length >= 6 ? `<svg viewBox="0 0 1 1" preserveAspectRatio="none"><polygon points="${pairs(det.outline)}" fill="none" stroke="#6ea2ff" stroke-width="0.012" vector-effect="non-scaling-stroke" style="stroke-width:2px"/></svg>` : "";
    const lo = t.readable ? Math.floor(t.ambient_c - 2) : "", hi = t.readable ? Math.ceil(Math.max(t.skin_c + 3, t.ambient_c + 8)) : "";
    const thermalUrl = `/v1/thermal/${encodeURIComponent(c.best)}${q(t.readable ? { lo, hi } : {})}`;
    const ev = c.best ? `<div class="evidence"><figure><div class="frame"><img src="/v1/crop/${encodeURIComponent(c.best)}${q()}" alt="Colour crop sent by the drone" onerror="this.closest('figure').hidden=true">${outline}</div><figcaption>Colour crop the drone sent${det ? ` (${esc(det.part)}, ${Math.round(det.conf * 100)}%)` : ""}</figcaption></figure>` +
      `<figure><div class="frame"><img src="${thermalUrl}" alt="Thermal crop sent by the drone" onerror="this.closest('figure').hidden=true"></div><figcaption>Thermal crop${t.readable ? `, ${t.skin_c.toFixed(1)} °C` : ""}</figcaption></figure></div>` : "";
    const st = casualtyState(c);
    const a = pic.assignments.filter((x) => x.casualty === c.id && x.state !== "cancelled").pop();
    const rows = [
      ["Condition", statusWords[c.status] || c.status || ""],
      ["Temperature", t.readable ? `${t.skin_c.toFixed(1)} °C against ${t.ambient_c.toFixed(1)} °C around it (${t.delta_c >= 0 ? "+" : ""}${t.delta_c.toFixed(1)})${t.trend_c_per_h ? `, ${t.trend_c_per_h.toFixed(1)} °C per hour` : ""}` : "not readable"],
      ["Movement", c.watched_s ? (c.moved ? `moved while watched (${Math.round(c.watched_s)} s)` : `still for ${Math.round(c.watched_s)} s watched close up`) : "not watched close up yet"],
      ["Position", `within ${c.cep.toFixed(1)} m (half of fixes fall inside)`],
      ["Seen by", (c.uavs || []).join(", ") + (c.first ? `, first ${when(c.first)}` : "")],
      ["Close looks", c.looks ? String(c.looks) : c.needs_look ? "wanted: " + (c.look_why || "") : "none needed"],
      ["Rescue", st.text + (a && a.length_m ? `; route ${Math.round(a.length_m)} m${a.streets && a.streets.length ? " via " + a.streets[0] : ""}` : "") + (a && a.extrication ? `; ${a.extrication}` : "")],
    ];
    const parts = (c.parts || []).map((p) => `<li>${esc(p.class)} ${Math.round(p.conf * 100)}% by ${esc(p.uav)}${p.obs ? `, seen ${p.obs} times` : ""}</li>`).join("");
    const chipCls = c.rescued ? "Rescued" : c.triage + (c.prov ? " prov" : "");
    return `<div class="top"><span class="chip ${chipCls}" style="--c:${TRI[c.triage] || "#999"}">${esc(c.rescued ? "Freed" : triWord(c))}</span><h3>${esc(c.id)}</h3>${big ? "" : `<button class="ghost close" type="button">Close</button>`}</div>` +
      `<p class="lead">${esc(c.reason)}</p>${ev}<dl class="facts">${rows.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join("")}</dl>` +
      (parts ? `<ul class="parts">${parts}</ul>` : "") + (c.prov ? `<p class="sub">Provisional: a closer look may change it. Triage here is a suggestion for a person to confirm.</p>` : "");
  }
  function pairs(a) { const out = []; for (let i = 0; i + 1 < a.length; i += 2) out.push(a[i] + "," + a[i + 1]); return out.join(" "); }

  // --- Picture -----------------------------------------------------------------------------------
  let renderPending = false, lastRender = 0, firstFit = true;
  function onPicture(p) {
    const now = performance.now();
    picGap = picAt ? Math.min(2000, now - picAt) : 1000; picAt = now;
    prevPic = pic; pic = p;
    if (prevPic && prevPic.run !== p.run && p.mode === "live") { tw.clear(); }
    for (const d of p.drones) setTween("d:" + d.id, d.lat, d.lon, now);
    for (const t of p.teams) setTween("t:" + t.id, t.lat, t.lon, now);
    map.redraw();
    if (firstFit && !PRESENT && (p.casualties.length || p.drones.length)) { firstFit = false; fitAll(0); }
    if (!renderPending) {
      renderPending = true;
      const wait = Math.max(0, 250 - (now - lastRender));
      setTimeout(() => { renderPending = false; lastRender = performance.now(); renderAll(); }, wait);
    }
  }
  function renderAll() {
    if (!pic) return;
    renderHeader(); renderLegend(); renderCasualties(); renderDrones(); renderTeams();
    if (!PRESENT) { renderTimeline(); renderThumbs(); renderChat(); renderFound(); renderTabCounts(); }
    renderDetail();
    if (PRESENT) renderPanel();
    renderClock();
  }
  function renderClock() {
    if (!pic) return;
    const c = $("clock");
    if (pic.mode === "replay" && pic.start) c.textContent = "+" + mmss(pic.now - pic.start);
    else c.textContent = new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
    if (PRESENT) {
      const r = pic.replay;
      $("capClock").textContent = cue && cue.clock === "mission" && pic.start ? `mission time +${mmss(pic.now - pic.start)}${r && r.speed !== 1 ? `, shown ${r.speed}× faster` : ""}` : "";
    }
  }
  setInterval(renderClock, 1000);

  // near: within the incident area (5 km, or 3 times its radius, of its centre).
  // A TAK device reporting from somewhere else (a phone's own GPS at home) must
  // not pull "fit" out to a zoom the offline map has no tiles for.
  function near(lat, lon) {
    if (!scene || !scene.centre || !(scene.centre[0] || scene.centre[1])) return true;
    const r = Math.max(5000, 3 * (scene.radius_m || 0));
    const dy = (lat - scene.centre[0]) * 111320, dx = (lon - scene.centre[1]) * 111320 * Math.cos(scene.centre[0] * Math.PI / 180);
    return dx * dx + dy * dy <= r * r;
  }
  function everything() {
    const pts = [];
    if (pic) {
      for (const c of pic.casualties) pts.push([c.lat, c.lon]);
      for (const d of pic.drones) if ((d.lat || d.lon) && near(d.lat, d.lon)) pts.push([d.lat, d.lon]);
      for (const t of pic.teams) if ((t.lat || t.lon) && near(t.lat, t.lon)) pts.push([t.lat, t.lon]);
    }
    if (scene) {
      for (const s of scene.sites) pts.push([s.lat, s.lon]);
      if (scene.staging && (scene.staging[0] || scene.staging[1])) pts.push(scene.staging);
    }
    return pts;
  }
  function fitAll(ms) {
    const pts = everything();
    if (pts.length) map.fit(pts, ms, PRESENT ? 90 : 60);
    else if (scene) map.flyTo(scene.centre[0], scene.centre[1], 16, ms);
  }
  $("zin").addEventListener("click", () => map.zoomAround(map.w / 2, map.h / 2, map.zoom + 1));
  $("zout").addEventListener("click", () => map.zoomAround(map.w / 2, map.h / 2, map.zoom - 1));
  $("zfit").addEventListener("click", () => { map.follow = null; fitAll(700); });

  // --- Presenter -----------------------------------------------------------------------------------
  const PANELS = {}; // name -> function(cue) returning HTML (phase 8 fills it)
  window.RubbleSARPanels = PANELS;
  function onCue(c) {
    const prev = cue;
    cue = c;
    // Caption.
    const cap = $("caption");
    cap.hidden = !(c.title || c.caption);
    document.body.classList.toggle("nocap", cap.hidden);
    $("capTitle").textContent = c.title || "";
    const tx = $("capText");
    if (tx.textContent !== (c.caption || "")) { tx.textContent = c.caption || ""; tx.classList.remove("enter"); void tx.offsetWidth; tx.classList.add("enter"); }
    $("capProgress").innerHTML = c.count ? Array.from({ length: c.count }, (_, i) => `<i class="${i + 1 < c.index ? "done" : i + 1 === c.index ? "now" : ""}"></i>`).join("") : "";
    // Card over the map.
    const card = $("card");
    if (c.card && c.card.title) {
      card.hidden = false; card.className = "card-over " + (c.card.kind || "");
      card.innerHTML = `<div class="inner"><h1>${esc(c.card.title)}</h1>${c.card.subtitle ? `<p class="subtitle">${esc(c.card.subtitle)}</p>` : ""}${c.card.lines && c.card.lines.length ? `<ul>${c.card.lines.map((l) => `<li>${esc(l)}</li>`).join("")}</ul>` : ""}</div>`;
    } else card.hidden = true;
    // Slate.
    const sl = $("slate");
    if (c.slate && (!prev || prev.slate !== c.slate)) { sl.textContent = c.slate; sl.hidden = false; setTimeout(() => { sl.hidden = true; }, 2500); }
    else if (!c.slate) sl.hidden = true;
    // Map.
    if (c.map && (!prev || prev.chapter !== c.chapter || JSON.stringify(prev.map) !== JSON.stringify(c.map))) applyMapCue(c.map);
    if (c.focus) pulseUntil = performance.now() + 60000;
    renderAll();
  }
  // closeZoom: as close as the offline map draws sharply (its last zoom, a little enlarged).
  function closeZoom() { return scene && scene.map ? Math.min(19.5, scene.map.max_zoom + 0.6) : 19; }
  function applyMapCue(m) {
    const ms = m.ms == null ? 1600 : m.ms;
    map.follow = null;
    if (m.mode === "fit") fitAll(ms);
    else if (m.mode === "view") map.flyTo(m.lat, m.lon, m.zoom || map.zoom, ms);
    else if (m.mode === "follow") { map.follow = m.target; const d = pic && pic.drones.find((x) => x.id === m.target); if (d) map.flyTo(d.lat, d.lon, m.zoom || Math.min(18, closeZoom() - 0.5), ms); }
    else if (m.mode === "site" && scene) {
      const s = scene.sites.find((x) => x.id === m.target) || scene.sites[0];
      if (s) map.flyTo(s.lat, s.lon, m.zoom || closeZoom(), ms);
    } else if (m.mode === "casualty" && pic) {
      const c = pic.casualties.find((x) => x.id === m.target);
      if (c) map.flyTo(c.lat, c.lon, m.zoom || closeZoom(), ms);
    } else if (m.mode === "sites" && scene) map.fit(scene.sites.map((s) => [s.lat, s.lon]).concat(scene.staging ? [scene.staging] : []), ms, 120);
  }
  // Panels are drawn once per chapter (html, then after) and kept up to date (tick).
  let panelKey = "";
  function renderPanel() {
    const p = $("panel"), side = $("side");
    const name = cue && cue.panel;
    if (!name) { p.hidden = true; side.hidden = false; panelKey = ""; return; }
    const ctx = { pic, scene, cue };
    if (name === "casualty") {
      const c = pic && pic.casualties.find((x) => x.id === (cue.focus || selected));
      p.hidden = false; side.hidden = true;
      const h = c ? `<div class="pin">${casualtyHTML(c, true)}</div>` : `<div class="pin"><p class="intro">Waiting for the casualty record.</p></div>`;
      if (p.__h !== h) { p.innerHTML = h; p.__h = h; }
      panelKey = "casualty";
      return;
    }
    const fn = PANELS[name];
    if (!fn) { p.hidden = true; side.hidden = false; return; }
    p.hidden = false; side.hidden = true;
    const key = name + "|" + (cue.chapter || "") + "|" + (scene ? 1 : 0);
    if (key !== panelKey) {
      panelKey = key;
      p.innerHTML = `<div class="pin p-${name}">${fn.html(ctx)}</div>`;
      p.__h = null;
      if (fn.after) fn.after(p, ctx);
    }
    if (fn.tick && pic) fn.tick(p, ctx);
  }

  // --- Explanation panels (the demo director's chapters) ------------------------------------------
  const cache = new Map();
  function getJSON(url) {
    if (!url) return Promise.reject(new Error("no address"));
    if (!cache.has(url)) cache.set(url, fetch(url, { cache: "no-store" }).then((r) => { if (!r.ok) throw new Error(r.status); return r.json(); }));
    return cache.get(url);
  }
  // cpName: the command post's computer as the video names it (the storyboard's command_post).
  const cpName = (ctx) => String(fact(ctx, "command_post", "the command-post laptop"));
  const fact = (ctx, k, alt) => { const f = ctx.cue && ctx.cue.data && ctx.cue.data.facts; return f && f[k] != null && f[k] !== "" ? f[k] : alt; };
  const setText = (root, sel, text) => { const el = root.querySelector(sel); if (el && el.textContent !== String(text)) el.textContent = text; };
  const setHTML = (root, sel, h) => { const el = root.querySelector(sel); if (el && el.__h !== h) { el.innerHTML = h; el.__h = h; } };
  const bar = (pct, cls) => `<span class="pbar ${cls || ""}"><i style="width:${Math.max(0, Math.min(100, pct))}%"></i></span>`;
  const chip = (tri, prov, label) => `<span class="chip ${tri}${prov ? " prov" : ""}" style="--c:${TRI[tri] || "#999"}">${esc(label || (prov ? tri + "?" : tri))}</span>`;
  function mediaURL(ctx, file) { const d = ctx.cue && ctx.cue.data; return d && d.media ? d.media + file : ""; }

  PANELS.scenario = {
    html(ctx) {
      const sc = ctx.scene || {}, v = sc.victims || {};
      const kinds = {};
      for (const s of sc.sites || []) kinds[s.kind] = (kinds[s.kind] || 0) + 1;
      const hs = (sc.sites || []).map((s) => s.height_m).filter((x) => x);
      const lo = hs.length ? Math.min(...hs).toFixed(0) : "", hi = hs.length ? Math.max(...hs).toFixed(0) : "";
      const height = hs.length ? (lo === hi ? `a pile ${hi} m high` : `piles ${lo} to ${hi} m high`) : "";
      const total = Object.values(v).reduce((a, b) => a + b, 0);
      const groups = [["alive responsive", "Yellow", "conscious, can move a little"], ["alive unresponsive", "Red", "unconscious"], ["deteriorating", "Red", "getting colder"], ["deceased", "Black", "dead"]];
      const seg = groups.filter(([k]) => v[k]).map(([k, t, w]) => `<div class="s-${t}" style="flex-grow:${v[k]};background:${TRI[t]}" title="${esc(w)}">${v[k]}</div>`).join("");
      const legend = groups.filter(([k]) => v[k]).map(([k, t, w]) => `<span><span class="sw" style="background:${TRI[t]}"></span>${v[k]} ${esc(w)}</span>`).join("");
      const teams = sc.teams || [];
      const sim = teams.filter((t) => t.source === "sim"), tak = teams.filter((t) => t.source === "tak");
      const place = String(fact(ctx, "place", sc.name || "The incident area"));
      const veh = sim.filter((t) => t.mode === "vehicle").length;
      return `<h2>The incident</h2><p class="intro">${esc(place.charAt(0).toUpperCase() + place.slice(1))}: a simulated collapse of ${plural((sc.sites || []).length, "building")}, with ${total || fact(ctx, "victims", "?")} people in the rubble.</p>
        <dl class="facts l">
          <dt>Collapsed buildings</dt><dd>${Object.entries(kinds).map(([k, n]) => plural(n, k === "pancake" ? "pancake collapse" : k === "mound" ? "rubble mound" : k)).join(", ")}${height ? "; " + height : ""}</dd>
          <dt>Streets closed</dt><dd>by the rubble of ${plural((sc.rubble || []).length, "collapse")}; the teams route around them</dd>
          <dt>Rescue teams</dt><dd>${sim.length ? `${sim.length} simulated${veh ? ` (${veh} with a vehicle)` : ""}` : ""}${sim.length && tak.length ? ", " : ""}${tak.length ? `${tak.length} with TAK devices: ${tak.map((t) => esc(t.callsign)).join(", ")}` : ""}${!teams.length ? "as configured at the command post" : ""}</dd>
        </dl>
        <h3>The people under the rubble</h3><div class="stack">${seg}</div><div class="legend2">${legend}</div>
        <p class="note">What the system should find: the colour each person should get (START triage). The people, the collapses and the teams are simulated; the streets are OpenStreetMap's.</p>`;
    },
  };

  PANELS.drones = {
    html() {
      return `<h2>Three drones, one Jetson</h2><p class="intro">A Jetson Orin Nano is the payload computer of all three. It flies them in simulation and runs the real image processing.</p><div class="dcards" id="dcards"></div>
        <dl class="facts"><dt>Flight</dt><dd>multirotor model, GPS error 1.5 m, compass drift, wind-free, 30 min batteries</dd><dt>Cameras</dt><dd>colour 1920 × 1080 and radiometric thermal 640 × 512, one frame a second each</dd>
        <dt>On board</dt><dd>body-part model (TensorRT), thermal reading, tracking, geolocation, a queue that holds every report until the command post confirms it</dd></dl>`;
    },
    tick(el, ctx) {
      const h = ctx.pic.drones.map((d) => {
        const job = d.role === "scout" ? "surveys the collapse sites" : "flies close looks when sent";
        const prog = d.steps ? bar((100 * d.step) / d.steps) + `<span class="sub">survey leg ${d.step} of ${d.steps}</span>` : "";
        return `<div class="dcard"><span class="dot" style="background:${colourOf(d.id)}"></span><div><b>${esc(d.id)}</b> <span class="sub">${esc(d.role)}: ${job}</span>
          <div class="sub">${esc(droneWords(d))}${airborne(d.state) ? `, ${Math.round(d.alt)} m up, ${d.speed.toFixed(0)} m/s` : ""}</div>
          <div class="meters2"><span>battery</span>${bar(d.battery, "bat")}<span>${Math.round(d.battery)}%</span>${prog ? `<span>survey</span>${prog}` : ""}</div></div></div>`;
      }).join("");
      setHTML(el, "#dcards", h);
    },
  };

  PANELS.camera = {
    html(ctx) {
      return `<h2>What the drone sees</h2><div id="cam"><p class="intro">Loading the frame…</p></div>`;
    },
    after(el, ctx) {
      getJSON(mediaURL(ctx, "media.json")).then((m) => {
        const t = ctx.pic && ctx.pic.start ? "+" + mmss(m.capture - ctx.pic.start) : "";
        el.querySelector("#cam").innerHTML = `<figure><img src="${mediaURL(ctx, m.rgb)}" alt="Colour frame from ${esc(m.uav)}"><figcaption>Colour camera, 1920 × 1080: ${m.gsd_cm.toFixed(1)} cm a pixel from ${Math.round(m.alt_m)} m up</figcaption></figure>
          <div class="grid2"><figure><img src="${mediaURL(ctx, m.thermal)}" alt="Thermal frame"><figcaption>Thermal camera, 640 × 512: every pixel a temperature</figcaption></figure>
          <div class="aside"><p>${esc(m.uav)}, mission time ${t}. Warm skin stands out against the cold rubble of a February morning; clothing and dust hide much of it.</p>
          <p class="note">${m.exact ? "Rendered again at the command post from the drone's own pose, clock and noise seed: the frame it saw, pixel for pixel." : esc(m.note || "Rendered again at the command post.")} The drone itself never sends whole frames.</p></div></div>`;
      }).catch(() => { el.querySelector("#cam").innerHTML = `<p class="intro">No frames for this run yet: <code>rubblesar demo media</code> renders them.</p>`; });
    },
  };

  // zoomBox is the region around the best cluster of parts found (2:1, at least 240 px wide, inside the frame).
  function zoomBox(m) {
    const ds = m.detections;
    if (!ds.length) return null;
    const c = (d) => [(d.box[0] + d.box[2]) / 2, (d.box[1] + d.box[3]) / 2];
    let best = null, bestN = -1;
    for (const d of ds) {
      const [x, y] = c(d);
      const n = ds.filter((e) => { const [u, v] = c(e); return Math.abs(u - x) < 180 && Math.abs(v - y) < 90; }).reduce((a, e) => a + e.conf, 0);
      if (n > bestN) { best = d; bestN = n; }
    }
    const [sx, sy] = c(best);
    const near = ds.filter((e) => { const [u, v] = c(e); return Math.abs(u - sx) < 180 && Math.abs(v - sy) < 90; });
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const d of near) { x0 = Math.min(x0, d.box[0]); y0 = Math.min(y0, d.box[1]); x1 = Math.max(x1, d.box[2]); y1 = Math.max(y1, d.box[3]); }
    let w = Math.min(m.width, Math.max(240, (x1 - x0) * 1.8, (y1 - y0) * 1.8 * 2));
    const h = w / 2;
    const cx = (x0 + x1) / 2, cy = (y0 + y1) / 2;
    return { x: Math.max(0, Math.min(m.width - w, cx - w / 2)), y: Math.max(0, Math.min(m.height - h, cy - h / 2)), w, h };
  }
  // placeLabels puts each part's label next to its box without overlapping the others (in frame pixels).
  function placeLabels(ds, z, fs) {
    const boxes = [], out = [];
    for (const d of ds) boxes.push([d.box[0] - 2, d.box[1] - 2, d.box[2] + 2, d.box[3] + 2]);
    for (const d of ds) {
      const text = d.class + (d.conf < 1 ? " " + Math.round(d.conf * 100) + "%" : "");
      const w = text.length * fs * 0.52, h = fs;
      const cands = [[d.box[0], d.box[1] - fs * 0.3], [d.box[0], d.box[3] + fs * 1.05], [d.box[2] + fs * 0.3, (d.box[1] + d.box[3]) / 2 + fs * 0.35], [d.box[0] - w - fs * 0.3, (d.box[1] + d.box[3]) / 2 + fs * 0.35]];
      let placed = null;
      for (const [x, y] of cands) {
        const b = [x, y - h, x + w, y + fs * 0.2];
        if (b[0] < z.x || b[2] > z.x + z.w || b[1] < z.y || b[3] > z.y + z.h) continue;
        if (boxes.some((o) => b[0] < o[2] && b[2] > o[0] && b[1] < o[3] && b[3] > o[1])) continue;
        placed = [x, y]; boxes.push(b); break;
      }
      if (placed) out.push({ d, text, x: placed[0], y: placed[1] });
    }
    return out;
  }
  PANELS.model = {
    html() { return `<h2>Finding people in the pixels</h2><div id="mdl"><p class="intro">Loading…</p></div>`; },
    after(el, ctx) {
      getJSON(mediaURL(ctx, "media.json")).then((m) => {
        const W = m.width, H = m.height, z = zoomBox(m);
        const tiles = m.tiles.map((t) => `<rect x="${t[0] + 3}" y="${t[1] + 3}" width="${t[2] - 6}" height="${t[2] - 6}" class="tile"/>`).join("");
        const zoomRect = z ? `<rect x="${z.x}" y="${z.y}" width="${z.w}" height="${z.h}" class="zoomrect"/>` : "";
        const fs = z ? Math.max(6, z.w / 30) : 30;
        const inZoom = z ? m.detections.filter((d) => d.box[2] > z.x && d.box[0] < z.x + z.w && d.box[3] > z.y && d.box[1] < z.y + z.h) : [];
        const labels = z ? placeLabels(inZoom, z, fs) : [];
        const boxes = inZoom.map((d) => `<rect class="dbox" x="${d.box[0] - 2}" y="${d.box[1] - 2}" width="${d.box[2] - d.box[0] + 4}" height="${d.box[3] - d.box[1] + 4}" style="stroke-width:${z.w / 420}"/>`).join("") +
          labels.map((l) => `<text class="dlab" x="${l.x}" y="${l.y}" style="font-size:${fs}px;stroke-width:${fs / 5}px">${esc(l.text)}</text>`).join("");
        const model = ctx.scene && ctx.scene.model;
        const mk = model && model.metrics ? Object.keys(model.metrics).find((x) => /\.all\.mask\.map50$/.test(x)) : null;
        const seg = fact(ctx, "seg_ms", "");
        const zoomImg = z ? `<div class="zoom" style="aspect-ratio:${z.w}/${z.h}"><img src="${mediaURL(ctx, m.masks)}" alt="The parts found, close up" style="width:${(100 * W) / z.w}%;left:${(-100 * z.x) / z.w}%;top:${(-100 * z.y) / z.h}%">
          <svg viewBox="${z.x} ${z.y} ${z.w} ${z.h}" preserveAspectRatio="none">${boxes}</svg></div>` : "";
        el.querySelector("#mdl").innerHTML = `<div class="seq"><img class="base" src="${mediaURL(ctx, m.rgb)}" alt="">
          <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">${tiles}${zoomRect}</svg></div>
          <p class="cap2">The frame cut into six overlapping 640-pixel tiles, all run in one batch.</p>${zoomImg}
          <p class="cap2">${m.detections.every((d) => d.conf >= 1) ? `${m.truth || m.detections.length} body parts in view, each outlined (the scenario's labels: no model here).` : `${m.detections.length} body parts found${m.truth ? ` (${m.truth} in view)` : ""}, each outlined, with its confidence.`}</p>
          <dl class="facts"><dt>Model</dt><dd>${esc(m.model_name || (model ? model.name + " (" + model.arch + ")" : "YOLO11 segmentation"))}: hand, arm, leg, head, torso</dd>
          <dt>On the Jetson</dt><dd>${esc(shortBackend(fact(ctx, "backend", "TensorRT")))}${seg && !isNaN(+seg) ? `, ${seg} ms to segment a frame` : ""}</dd>
          ${mk ? `<dt>Accuracy</dt><dd>${Math.round(model.metrics[mk] * 100)}% mask mAP@50 on held-out synthetic frames</dd>` : ""}
          <dt>Here</dt><dd class="sub">${esc(m.model)}</dd></dl>`;
      }).catch(() => { el.querySelector("#mdl").innerHTML = `<p class="intro">No frames for this run yet: <code>rubblesar demo media</code> renders them.</p>`; });
    },
  };

  PANELS.packet = {
    html() { return `<h2>From a detection to a report</h2><div id="pkt"><p class="intro">Waiting for the first report…</p></div>`; },
    tick(el, ctx) {
      const id = ctx.cue.focus;
      const c = id && ctx.pic.casualties.find((x) => x.id === id);
      const good = (x) => x && x.crop && x.part !== "warm spot";
      const mine = c ? ctx.pic.detections.filter((x) => x.casualty === c.id || x.id === c.best) : [];
      const d = mine.find((x) => x.id === c.best && good(x)) || mine.filter(good).sort((a, b) => b.conf - a.conf)[0] || ctx.pic.detections.find(good) || mine[0];
      if (!d) return;
      const t = (c && c.thermal) || d.thermal || {};
      const frame = Number(fact(ctx, "frame_mb", "1.2")) * 1e6 || 1.2e6;
      const ratio = Math.round(frame / Math.max(1, d.bytes));
      const outline = d.outline && d.outline.length >= 6 ? `<svg viewBox="0 0 1 1" preserveAspectRatio="none"><polygon points="${pairs(d.outline)}" fill="none" stroke="#6ea2ff" style="stroke-width:3px" vector-effect="non-scaling-stroke"/></svg>` : "";
      const lo = t.readable ? Math.floor(t.ambient_c - 2) : "", hi = t.readable ? Math.ceil(Math.max(t.skin_c + 3, t.ambient_c + 8)) : "";
      const h = `<div class="grid2 crops"><figure><div class="frame">${d.crop ? `<img src="/v1/crop/${encodeURIComponent(d.id)}${q()}" alt="">` : ""}${outline}</div><figcaption>Colour crop and outline: ${esc(d.part)}${d.conf ? `, ${Math.round(d.conf * 100)}% sure` : ""}</figcaption></figure>
        <figure><div class="frame">${d.therm ? `<img src="/v1/thermal/${encodeURIComponent(d.id)}${q(t.readable ? { lo, hi } : {})}" alt="">` : ""}</div><figcaption>Thermal crop</figcaption></figure></div>
        ${t.readable ? `<p class="reading"><span class="big">${t.skin_c.toFixed(1)} °C</span> against <span class="big">${t.ambient_c.toFixed(1)} °C</span> around it: ${t.warm ? "warm, a living body?" : "no warmer than the rubble"}</p>` : ""}
        <h3>What crosses the radio</h3>
        <div class="sizes"><div><span>This report</span>${bar(Math.max(0.6, (100 * d.bytes) / frame), "sig")}<b>${(d.bytes / 1024).toFixed(1)} KB</b></div>
        <div><span>The two whole frames</span>${bar(100, "grey")}<b>${(frame / 1e6).toFixed(1)} MB</b></div></div>
        <p class="sub">${ratio > 1 ? `${ratio} times smaller. ` : ""}Seen in ${d.obs || "several"} frames, placed within ${d.cep.toFixed(1)} m (CEP), sent as a ${esc(d.payload || "full")} report${c ? `; it is casualty ${esc(c.id)}` : ""}.</p>`;
      setHTML(el, "#pkt", h);
    },
  };

  PANELS.radio = {
    html() {
      return `<h2>Built for a poor radio link</h2><p class="intro">Each drone's radio over the last few minutes: its budget, what it uses (kbit/s), and the reports waiting on board.</p>
        <div class="rlegend"><span><i class="ln" style="background:#6ea2ff"></i>used</span><span><i class="ln dash"></i>budget</span><span><i class="sq" style="background:rgba(240,138,75,.8)"></i>reports waiting</span><span><i class="sq" style="background:rgba(240,138,75,.25)"></i>nothing heard</span></div>
        <div id="radio"></div>
        <div class="levels"><span>When the link falls behind, reports shrink:</span><i data-l="0">full</i><i data-l="1">small crops</i><i data-l="2">thermal only</i><i data-l="3">metadata only</i></div>
        <div class="counts" id="rcounts"></div>`;
    },
    after(el, ctx) {
      el.querySelector("#radio").innerHTML = ctx.pic ? ctx.pic.drones.map((d) => `<div class="rrow" data-id="${esc(d.id)}"><div class="rhead"><span class="dot" style="background:${colourOf(d.id)}"></span><b>${esc(d.id)}</b><span class="rst sub"></span></div><canvas width="1400" height="150"></canvas></div>`).join("") : "";
    },
    tick(el, ctx) {
      const p = ctx.pic;
      let waiting = 0, lost = 0;
      for (const d of p.drones) {
        const l = d.link || {};
        waiting += (l.queued || 0) + (l.unacked || 0); lost += l.discarded || 0;
        const row = el.querySelector(`.rrow[data-id="${CSS.escape(d.id)}"]`);
        if (!row) continue;
        const st = d.stale ? `link lost: nothing heard for ${span((p.now - d.seen) / 1000)}` : `${(l.used_bps / 1000).toFixed(0)} of ${l.budget_bps ? (l.budget_bps / 1000).toFixed(0) : "∞"} kbit/s, ${(l.queued || 0) + (l.unacked || 0)} reports waiting${l.degrade ? ", sending " + ["full reports", "small crops", "thermal only", "metadata only"][l.degrade] : ""}`;
        setText(row, ".rst", st);
        const h = (d.link_hist || []).map((x) => ({ t: x[0], used: x[1], budget: x[2], q: x[3], dg: x[4] }));
        drawRadio(row.querySelector("canvas"), h, p.now);
      }
      const lv = Math.max(0, ...p.drones.filter((d) => !d.stale).map((d) => (d.link || {}).degrade || 0));
      el.querySelectorAll(".levels i").forEach((i) => i.classList.toggle("on", +i.dataset.l === lv));
      setHTML(el, "#rcounts", `<div><span class="big">${p.kpi.detections}</span><span>reports reached the command post</span></div><div><span class="big">${waiting}</span><span>waiting on board now</span></div><div><span class="big ${lost ? "bad" : "good"}">${lost}</span><span>lost</span></div>`);
    },
  };
  // drawRadio: one drone's link history. Time runs left to right over the
  // history there is (one to three minutes); kbit/s on a log scale, so a few
  // kbit/s of use still shows under a budget of hundreds.
  const DEGRADE_FILL = ["", "rgba(240,190,75,.85)", "rgba(240,138,75,.9)", "rgba(229,72,77,.9)"];
  function drawRadio(cv, h, now) {
    if (!cv) return;
    const g = cv.getContext("2d"), W = cv.width, H = cv.height, L = 92, T = 14, B = H - 6;
    g.clearRect(0, 0, W, H);
    const span = Math.min(180000, Math.max(60000, now - (h.length ? h[0].t : now)));
    const x = (t) => L + (W - L) * (1 - (now - t) / span);
    const top = Math.max(256000, ...h.map((p) => Math.max(p.budget, p.used)));
    const lt = Math.log10(top / 1000);
    const y = (v) => B - (Math.log10(Math.max(v, 1000) / 1000) / lt) * (B - T);
    const maxQ = Math.max(20, ...h.map((p) => p.q));
    // Scale: 1, 10, 100 kbit/s.
    g.font = "500 24px Barlow, sans-serif"; g.textBaseline = "middle"; g.textAlign = "right";
    for (let v = 1000; v <= top; v *= 10) {
      g.strokeStyle = "rgba(160,160,160,.18)"; g.lineWidth = 1; g.beginPath(); g.moveTo(L, y(v)); g.lineTo(W, y(v)); g.stroke();
      g.fillStyle = "#9a958d"; g.fillText(v >= 1e6 ? v / 1e6 + "M" : v / 1000 + "k", L - 12, Math.min(B - 10, Math.max(T + 8, y(v))));
    }
    g.save(); g.beginPath(); g.rect(L, 0, W - L, H); g.clip();
    // Silent stretches.
    g.fillStyle = "rgba(240,138,75,.22)";
    for (let i = 0; i + 1 < h.length; i++) if (h[i + 1].t - h[i].t > 6000) g.fillRect(x(h[i].t), 0, x(h[i + 1].t) - x(h[i].t), H);
    if (h.length && now - h[h.length - 1].t > 6000) g.fillRect(x(h[h.length - 1].t), 0, W, H);
    // Reports shrinking: a band along the top in the colour of the level.
    for (let i = 0; i + 1 < h.length; i++) if (h[i].dg > 0 && h[i + 1].t - h[i].t <= 6000) { g.fillStyle = DEGRADE_FILL[Math.min(3, h[i].dg)]; g.fillRect(x(h[i].t), 0, x(h[i + 1].t) - x(h[i].t) + 1, 10); }
    // Reports waiting (bars, their own scale).
    g.fillStyle = "rgba(240,138,75,.75)";
    for (const p of h) { const bh = (p.q / maxQ) * (B - T); if (bh > 0) g.fillRect(x(p.t) - 3, B - bh, 6, bh); }
    // Budget and use.
    g.lineWidth = 3; g.setLineDash([8, 6]); g.strokeStyle = "rgba(170,170,170,.95)"; g.beginPath();
    h.forEach((p, i) => (i ? g.lineTo(x(p.t), y(p.budget)) : g.moveTo(x(p.t), y(p.budget)))); g.stroke(); g.setLineDash([]);
    g.strokeStyle = "#6ea2ff"; g.lineWidth = 4; g.beginPath();
    let first = true;
    for (let i = 0; i < h.length; i++) {
      const p = h[i], prev = h[i - 1];
      if (!first && prev && p.t - prev.t > 6000) { g.stroke(); g.beginPath(); first = true; }
      if (first) { g.moveTo(x(p.t), y(p.used)); first = false; } else g.lineTo(x(p.t), y(p.used));
    }
    g.stroke();
    g.restore();
  }

  PANELS.found = {
    html() {
      return `<h2>What the drones found</h2><p class="intro" id="fsum"></p><div class="cards pcards" id="pcards"></div>`;
    },
    tick(el) { foundInto(el.querySelector("#pcards"), el.querySelector("#fsum"), { max: 4 }); },
  };

  PANELS.zbook = {
    html(ctx) {
      return `<h2>The command post</h2><div class="flow">
        <div class="st jet"><b>Jetson Orin Nano</b><span>edge-eye: three drones, their cameras, the model, the on-board queue</span></div>
        <div class="arrow"><span id="zlink">Zenoh over Wi-Fi</span></div>
        <div class="st"><b>zenohd</b><span>routes every message at the command post</span></div>
        <div class="st"><b>casualty-analyst</b><span id="zan">stores and acknowledges each report, merges them into one record per person, suggests the triage</span></div>
        <div class="st"><b>mission-planner</b><span id="zpl">sends an investigator where a close look could change the answer</span></div>
        <div class="st"><b>rescue-dispatch</b><span id="zdi">sends the nearest free team along streets clear of rubble</span></div>
        <div class="two"><div class="st"><b>taky TAK server</b><span id="ztak">ATAK and WinTAK devices</span></div><div class="st"><b>sar-web</b><span>the offline map, this view, the after-action reports</span></div></div>
        </div><p class="note">All on ${esc(cpName(ctx))}: no internet needed during the incident.</p>`;
    },
    tick(el, ctx) {
      const p = ctx.pic, k = p.kpi;
      setText(el, "#zlink", `Zenoh over Wi-Fi: ${k.detections} reports, ${Math.round(k.bytes / 1024)} KB so far`);
      setText(el, "#zan", `${k.detections} reports stored and acknowledged; ${k.records} casualty records, one per person`);
      setText(el, "#zpl", `${k.looks_done} close looks done, ${k.looks_open} under way`);
      setText(el, "#zdi", `${p.assignments.length} team assignments; ${k.rescued} people freed so far`);
      const users = p.tak && p.tak.users ? p.tak.users.length : 0;
      setText(el, "#ztak", `ATAK and WinTAK devices${users ? `: ${users} on the server now` : ""}`);
    },
  };

  PANELS.triage = {
    html() {
      const rows = [["Warm, not seen close up yet", chip("Red", true), "an investigator is sent for a close look"], ["Moves while watched close up", chip("Yellow"), "trapped but responsive: a team, after the Reds"],
        ["Warm and still, close up", chip("Red"), "a team at once"], ["Warm but cooling", chip("Red"), "a team at once"], ["At rubble temperature, close up", chip("Black"), "kept for recovery"],
        ["Temperature unknown", chip("Grey", true), "a close look"]];
      return `<h2>The close look, and triage</h2><table class="rules"><tr><th>What the drones saw</th><th>Colour</th><th>What happens</th></tr>${rows.map((r) => `<tr><td>${r[0]}</td><td>${r[1]}</td><td>${r[2]}</td></tr>`).join("")}</table>
        <div id="tri"></div><p class="note">Colours are suggestions for a person to confirm. The thresholds are illustrative and await medical review.</p>`;
    },
    tick(el, ctx) {
      const id = ctx.cue.focus, p = ctx.pic;
      const c = id && p.casualties.find((x) => x.id === id);
      if (!c) return;
      const dr = p.drones.find((d) => d.task && d.task.casualty === id);
      const t = c.thermal || {};
      let watch = "";
      if (dr && dr.task.state === "on_station") watch = `${esc(dr.id)} is watching it from ${Math.round(dr.alt)} m: ${Math.round(dr.task.elapsed_s || 0)} of ${dr.task.hover_s || 30} s`;
      else if (dr) watch = `${esc(dr.id)} is on its way for a close look`;
      else if (c.close) watch = c.moved ? `it moved while watched (${Math.round(c.watched_s)} s)` : `watched close up for ${Math.round(c.watched_s)} s: no movement`;
      setHTML(el, "#tri", `<div class="focus">${chip(c.triage, c.prov)}<b>${esc(c.id)}</b><span>${esc(c.reason)}</span></div>
        <dl class="facts"><dt>Temperature</dt><dd>${t.readable ? `${t.skin_c.toFixed(1)} °C, ${t.delta_c >= 0 ? "+" : ""}${t.delta_c.toFixed(1)} against the rubble` : "not readable yet"}</dd><dt>Close look</dt><dd>${watch || "not yet"}</dd></dl>`);
    },
  };

  const takTeams = (ctx) => {
    const set = new Set(((ctx.scene && ctx.scene.teams) || []).filter((t) => t.source === "tak").map((t) => t.callsign));
    for (const t of (ctx.pic && ctx.pic.teams) || []) if (t.source === "tak") set.add(t.callsign);
    return set;
  };
  PANELS.tak = {
    html(ctx) {
      const tak = [...takTeams(ctx)];
      return `<h2>On the rescuers' own devices</h2>
        <dl class="facts"><dt>On their maps</dt><dd>casualties in their triage colour (green once freed), the drones, the teams and their routes, the rubble, over an offline map served from the command post</dd>
        <dt>A task</dt><dd>a direct chat message and a route line from SAR-OPS; the team answers "ok", "done" or "unable"</dd>
        <dt>Teams on TAK</dt><dd id="taknames">${tak.length ? tak.map(esc).join(", ") : "none configured"}</dd></dl>
        <h3>The chat with the TAK teams</h3><ol class="bubbles" id="bub"></ol>`;
    },
    tick(el, ctx) {
      const tak = takTeams(ctx);
      setHTML(el, "#taknames", tak.size ? [...tak].map(esc).join(", ") : "none configured");
      const lines = ctx.pic.chats.filter((c) => tak.has(c.to) || tak.has(c.from)).slice(-5);
      setHTML(el, "#bub", lines.map((c) => `<li class="${tak.has(c.from) ? "in" : "out"}"><span class="who">${esc(c.from)} to ${esc(c.to)}</span>${esc(trim(c.text, 220))}</li>`).join("") || `<li class="sub">No messages to a TAK team yet.</li>`);
    },
  };

  PANELS.aar = {
    html() { return `<h2>After the mission</h2><div id="aar"><p class="intro">Loading the report…</p></div>`; },
    after(el, ctx) {
      const d = ctx.cue.data || {};
      getJSON(d.report).then((r) => {
        const k = r.kpi || {};
        const sp = (ns) => (ns == null || ns < 0 ? "-" : span(ns / 1e9));
        const tiles = [[`${k.Found} of ${k.Victims}`, "simulated people found"], [`${k.TriageAgree} of ${k.TriageCompared}`, "given the colour the scenario expects"],
          [`${k.Rescued} of ${k.RedYellow}`, "Red and Yellow casualties freed"], [sp(k.ExtricationRealMed), "to free each person (median, real-world estimate)"],
          [sp(k.TravelRealMed), "for a team to get there (median, real-world estimate)"], [`${k.Records}`, `casualty records${k.FalseRecords ? `, ${k.FalseRecords} of them false alarms` : ""}`]];
        el.querySelector("#aar").innerHTML = `<div class="ktiles">${tiles.map(([v, l]) => `<div><span class="big">${esc(v)}</span><span>${esc(l)}</span></div>`).join("")}</div>
          <table><tr><th>Team</th><th>Rescues</th><th>Walked or driven</th></tr>${(r.teams || []).filter((t) => t.Assignments).map((t) => `<tr><td>${esc(t.Callsign)}</td><td>${t.Done}</td><td>${(t.DistanceM / 1000).toFixed(1)} km</td></tr>`).join("")}</table>
          <p class="note">The full report, with the map, the timeline and every casualty against the scenario: ${esc(location.host)}/reports/${esc(d.incident || "")}/${esc(d.run || "")}/</p>`;
      }).catch(() => { el.querySelector("#aar").innerHTML = `<p class="intro">The after-action report of this run is not there yet.</p>`; });
    },
  };

  // --- Boot ------------------------------------------------------------------------------------------
  const map = new MapView($("map"));
  async function loadScene() {
    try {
      const r = await fetch("/v1/scene", { cache: "no-store" });
      scene = await r.json();
      if (firstFit && !PRESENT) map.setView(scene.centre[0], scene.centre[1], 16);
      if (PRESENT && !cue) map.setView(scene.centre[0], scene.centre[1], 16);
      document.title = "RubbleSAR operator view" + (scene.name ? ": " + scene.name : "");
      map.redraw();
      if (pic) renderAll();
    } catch (e) { setTimeout(loadScene, 3000); }
  }
  loadScene();
  setInterval(loadScene, 60000);
  if (document.fonts) document.fonts.ready.then(() => map.redraw());

  function connect() {
    const es = new EventSource("/v1/stream?src=" + SRC);
    es.addEventListener("picture", (e) => { try { onPicture(JSON.parse(e.data)); } catch (err) { console.error(err); } });
    es.addEventListener("cue", (e) => { try { onCue(JSON.parse(e.data)); } catch (err) { console.error(err); } });
    es.onerror = () => {
      const m = $("mode");
      m.className = "mode down"; m.textContent = "Reconnecting";
    };
  }
  connect();
  // For the demo director and tests: the page's state.
  window.RubbleSAR = { get picture() { return pic; }, get cue() { return cue; }, get scene() { return scene; }, map: () => ({ lat: map.lat, lon: map.lon, zoom: map.zoom }) };
})();
