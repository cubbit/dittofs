// The animation kit. A scene declares nodes, links and steps; this file draws
// it as SVG and moves it one step per reveal fragment. Decks never draw by hand,
// so every deck's pictures behave and look the same.
//
// spec = {
//   viewBox: [w, h],
//   nodes: [{ id, x, y, w, h, kind, label, sub, hidden }],   // label/sub: string keys
//   links: [{ id, from, to, via: [[x, y]], dashed, bad, arrow, label, hidden }],
//   steps: [{
//     focus: [ids],            // highlighted during this step only
//     show: [ids], hide: [ids],
//     dim: [ids], undim: [ids],
//     tone: { id: 'bad' | 'good' },          // persistent node colouring
//     packets: [{ link, reverse, tone, label, bounce, with }],
//     marks: [{ on, text, tone, dx, dy }],   // badges, persistent until cleared
//     clear: true,             // drop earlier marks first
//   }],
// }
// Captions are a string array at the scene's caption key, one per step.
//
// How the step on screen moves is the reader's choice, one of MOTION_MODES,
// kept in localStorage for every deck:
//   loop  the step replays while its slide is shown: after a pause its
//         packets run again;
//   once  the step plays once, then holds its end state;
//   off   nothing moves: each step is drawn at its end state at once.
// Without a stored choice the mode is once, or off under
// prefers-reduced-motion. PDF export is always off and shows the last step.
// A step never advances on its own, the last included: replaying the whole
// scene from the last step read as the scene moving on.
//
// The drawing is aria-hidden: the captions say what it shows. The captions
// list is a polite live region, and the caption of the step that becomes
// current is re-inserted (see captions()), so a screen reader announces it.

import { animate } from '../third-party/anime-4.5.0/anime.esm.min.js';

const SVG = 'http://www.w3.org/2000/svg';
const el = (name, attrs = {}, parent) => {
  const n = document.createElementNS(SVG, name);
  for (const [k, v] of Object.entries(attrs)) if (v !== undefined && v !== null) n.setAttribute(k, v);
  if (parent) parent.appendChild(n);
  return n;
};

const LOOP_PAUSE = 2200; // ms before a step or a scene plays again
// Scales every movement: packets, and the beat before a step's nodes appear.
// The CSS transitions in deck.css are scaled to match by hand.
const PACE = 2;

export const MOTION_MODES = ['loop', 'once', 'off'];
const MOTION_KEY = 'learn-dittofs.motion';
const printing = () => /print-pdf/.test(location.search);

// The mode in force: PDF export, then the stored choice, then the default.
export function motionMode() {
  if (printing()) return 'off';
  try {
    const saved = localStorage.getItem(MOTION_KEY);
    if (MOTION_MODES.includes(saved)) return saved;
  } catch { /* storage unavailable: fall through */ }
  return window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'off' : 'once';
}

// Stores a choice and marks the page with it, for the CSS transitions that
// off also stops. The caller replays the slide on screen (SceneDirector.sync).
export function rememberMotion(mode) {
  try { localStorage.setItem(MOTION_KEY, mode); } catch { /* not persisted */ }
  markMotion();
}

export function markMotion() {
  document.documentElement.dataset.motion = motionMode();
}

// Where the segment from a rect's centre towards (tx, ty) leaves the rect.
function edgePoint(n, tx, ty) {
  const cx = n.x + n.w / 2;
  const cy = n.y + n.h / 2;
  const dx = tx - cx;
  const dy = ty - cy;
  if (dx === 0 && dy === 0) return [cx, cy];
  const sx = dx === 0 ? Infinity : (n.w / 2) / Math.abs(dx);
  const sy = dy === 0 ? Infinity : (n.h / 2) / Math.abs(dy);
  const s = Math.min(sx, sy);
  return [cx + dx * s, cy + dy * s];
}

export class Scene {
  constructor(host, spec, text) {
    this.host = host;
    this.spec = spec;
    this.text = text; // (key) => plain string in the current language
    this.nodes = new Map(spec.nodes.map((n) => [n.id, n]));
    this.step = -1;
    this.running = [];
    this.token = 0;
    this.draw();
  }

  draw() {
    const [w, h] = this.spec.viewBox;
    this.svg = el('svg', { viewBox: `0 0 ${w} ${h}`, 'aria-hidden': 'true', focusable: 'false', preserveAspectRatio: 'xMidYMid meet' });
    const defs = el('defs', {}, this.svg);
    const marker = el('marker', {
      id: `arrow-${this.host.dataset.scene}`, viewBox: '0 0 10 10', refX: 9, refY: 5,
      markerWidth: 7, markerHeight: 7, orient: 'auto-start-reverse',
    }, defs);
    el('path', { d: 'M0,0 L10,5 L0,10 z', class: 'arrowhead' }, marker);

    this.layers = {
      groups: el('g', {}, this.svg),
      links: el('g', {}, this.svg),
      nodes: el('g', {}, this.svg),
      packets: el('g', {}, this.svg),
      marks: el('g', {}, this.svg),
    };
    this.parts = new Map();

    for (const l of this.spec.links || []) {
      const g = el('g', { class: `link${l.dashed ? ' dashed' : ''}${l.bad ? ' bad' : ''}` }, this.layers.links);
      const path = el('path', { d: this.linkPath(l) }, g);
      if (l.arrow !== false) path.setAttribute('marker-end', `url(#arrow-${this.host.dataset.scene})`);
      if (l.label) {
        const mid = this.linkMid(l);
        l.labelEl = el('text', { x: mid[0] + (l.labelDx || 0), y: mid[1] - 8 + (l.labelDy || 0), 'text-anchor': 'middle' }, g);
      }
      l.pathEl = path;
      this.parts.set(l.id, g);
    }

    for (const n of this.spec.nodes) {
      const layer = n.kind === 'group' ? this.layers.groups : this.layers.nodes;
      const g = el('g', { class: `node kind-${n.kind || 'plain'}` }, layer);
      el('rect', { x: n.x, y: n.y, width: n.w, height: n.h, rx: n.kind === 'group' ? 16 : 12 }, g);
      if (n.kind === 'group') {
        n.labelEl = el('text', { x: n.x + 14, y: n.y + 24 }, g);
      } else {
        const cy = n.y + n.h / 2;
        n.labelEl = el('text', { x: n.x + n.w / 2, y: n.sub ? cy - 4 : cy + 7, 'text-anchor': 'middle' }, g);
        if (n.sub) n.subEl = el('text', { x: n.x + n.w / 2, y: cy + 20, 'text-anchor': 'middle', class: 'sub' }, g);
      }
      this.parts.set(n.id, g);
    }

    this.host.prepend(this.svg);
    this.relabel();
    this.render(-1);
  }

  linkPoints(l) {
    const a = this.nodes.get(l.from);
    const b = this.nodes.get(l.to);
    const via = l.via || [];
    const first = via[0] || [b.x + b.w / 2, b.y + b.h / 2];
    const last = via[via.length - 1] || [a.x + a.w / 2, a.y + a.h / 2];
    // start/end pin a link to a point on the box edge, for loops and ports.
    return [l.start || edgePoint(a, first[0], first[1]), ...via, l.end || edgePoint(b, last[0], last[1])];
  }

  linkPath(l) {
    return this.linkPoints(l).map((p, i) => `${i ? 'L' : 'M'}${p[0].toFixed(1)},${p[1].toFixed(1)}`).join(' ');
  }

  linkMid(l) {
    const pts = this.linkPoints(l);
    const i = Math.floor((pts.length - 1) / 2);
    return [(pts[i][0] + pts[i + 1][0]) / 2, (pts[i][1] + pts[i + 1][1]) / 2];
  }

  relabel() {
    // A label longer than its box (a translation, usually) is shrunk to fit
    // rather than overflowing; widths are estimated because a slide that is
    // not displayed has no text metrics.
    const fit = (textEl, text, size, room) => {
      textEl.textContent = text;
      const est = text.length * size * 0.56;
      textEl.style.fontSize = est > room ? `${Math.max(13, (size * room) / est).toFixed(1)}px` : '';
    };
    for (const n of this.spec.nodes) {
      const room = n.kind === 'group' ? n.w - 28 : n.w - 20;
      fit(n.labelEl, this.label(n.label), n.kind === 'group' ? 17 : 22, room);
      if (n.subEl) fit(n.subEl, this.label(n.sub), 16, room);
    }
    for (const l of this.spec.links || []) if (l.labelEl) l.labelEl.textContent = this.label(l.label);
    this.render(this.step);
  }

  // A label starting with '=' is literal (a wire name, a code identifier).
  label(key) {
    if (!key) return '';
    return key.startsWith('=') ? key.slice(1) : this.text(key);
  }

  // The state after `upto` steps, with step `upto`'s marks only when withMarks.
  stateAt(upto, withMarks = true) {
    const hidden = new Set([...this.spec.nodes, ...(this.spec.links || [])].filter((p) => p.hidden).map((p) => p.id));
    const dim = new Set();
    const tone = new Map();
    let marks = [];
    let focus = [];
    for (let i = 0; i <= upto; i++) {
      const s = this.spec.steps[i];
      const last = i === upto;
      (s.show || []).forEach((id) => hidden.delete(id));
      (s.hide || []).forEach((id) => hidden.add(id));
      (s.dim || []).forEach((id) => dim.add(id));
      (s.undim || []).forEach((id) => dim.delete(id));
      // A step clears earlier marks as it starts; its own marks and tones
      // appear once its packets have arrived.
      if (s.clear) marks = [];
      if (!last || withMarks) {
        Object.entries(s.tone || {}).forEach(([id, t]) => (t ? tone.set(id, t) : tone.delete(id)));
        marks = marks.concat(s.marks || []);
      }
      if (last) focus = s.focus || [];
    }
    return { hidden, dim, tone, marks, focus };
  }

  render(upto, withMarks = true) {
    const st = this.stateAt(upto, withMarks);
    for (const [id, g] of this.parts) {
      g.classList.toggle('hidden', st.hidden.has(id));
      g.classList.toggle('dim', st.dim.has(id));
      g.classList.remove('hl', 'hl-bad', 'hl-good', 'pulse');
      const t = st.tone.get(id);
      if (t) g.classList.add(`hl-${t}`);
      else if (st.focus.includes(id)) g.classList.add('hl');
    }
    this.layers.marks.replaceChildren();
    for (const m of st.marks) this.drawMark(m);
    this.captions(upto);
  }

  drawMark(m) {
    const n = this.nodes.get(m.on);
    const text = this.label(m.text);
    const fs = 17;
    const w = Math.max(40, text.length * fs * (/^[A-Z0-9_]+$/.test(text) ? 0.72 : 0.6) + 24);
    const h = 30;
    const x = n.x + n.w / 2 - w / 2 + (m.dx || 0);
    const y = (m.below ? n.y + n.h + 8 : n.y - h - 8) + (m.dy || 0);
    const g = el('g', { class: `mark ${m.tone || 'info'}` }, this.layers.marks);
    el('rect', { x, y, width: w, height: h, rx: 15 }, g);
    const t = el('text', { x: x + w / 2, y: y + 20, 'text-anchor': 'middle' }, g);
    t.textContent = text;
  }

  // Marks the current caption. A class change alone is not announced, so
  // when the current step changes its caption's content is replaced by a copy
  // of itself: new content in the live region, read whole (aria-atomic).
  // Rendering the same caption again, as a replay does, announces nothing; a
  // language switch rebuilds the captions, so it announces the current one
  // in the new language.
  captions(upto) {
    const list = this.host.querySelector('.captions');
    if (!list) return;
    list.setAttribute('aria-live', 'polite');
    const items = list.querySelectorAll(':scope > li');
    items.forEach((li, i) => {
      li.classList.toggle('active', i === upto);
      li.classList.toggle('done', i < upto);
      li.setAttribute('aria-atomic', 'true');
    });
    const current = items[upto];
    if (current && this.announced !== current) {
      current.replaceChildren(...[...current.childNodes].map((n) => n.cloneNode(true)));
    }
    this.announced = current || null;
  }

  stop() {
    this.token++;
    for (const a of this.running) a.pause();
    this.running = [];
    clearTimeout(this.timer);
    this.layers.packets.replaceChildren();
  }

  // Jump to a step with no motion: rewinding, leaving a slide, printing.
  set(step) {
    this.stop();
    this.step = step;
    this.render(step);
  }

  // Resolves true after ms unless the scene was stopped meanwhile.
  wait(ms, token) {
    return new Promise((resolve) => {
      this.timer = setTimeout(() => resolve(token === this.token), ms);
    });
  }

  // Advance to a step, moving its packets before its marks appear, then, in
  // loop mode, keep replaying that step alone until stopped.
  async play(step) {
    const mode = motionMode();
    if (mode === 'off' || step < 0) return this.set(step);
    this.stop();
    const token = this.token;
    this.step = step;
    if (!(await this.run(step, token))) return;
    if (mode !== 'loop') return;
    const s = this.spec.steps[step];
    // A step with nothing moving would only blink its marks.
    if (!(s.packets || []).length && !(s.show || []).length) return;
    while (await this.wait(LOOP_PAUSE, token)) {
      if (!(await this.run(step, token, true))) return;
    }
  }

  // Plays one step from the state just before it. False once stopped.
  // The first run of a step builds it: its new nodes appear, then its packets
  // travel, then its marks. A replay changes nothing on screen but the
  // movement: nodes and marks stay put, packets travel again, and a step that
  // only shows nodes pulses their outline instead of making them blink, so the
  // text in them stays readable while the step loops.
  async run(step, token, replay = false) {
    this.step = step;
    this.render(step, replay);
    const s = this.spec.steps[step];
    const packets = s.packets || [];
    if (!packets.length && (s.show || []).length) {
      const before = this.stateAt(step - 1);
      const fresh = s.show.filter((id) => before.hidden.has(id)).map((id) => this.parts.get(id)).filter(Boolean);
      if (replay) {
        for (const g of fresh) g.classList.add('pulse');
        const ok = await this.wait(700 * PACE, token);
        for (const g of fresh) g.classList.remove('pulse');
        if (!ok) return false;
      } else {
        for (const g of fresh) g.classList.add('hidden');
        if (!(await this.wait(350 * PACE, token))) return false;
        this.render(step, false);
      }
    }
    let batch = [];
    const flush = async () => {
      await Promise.all(batch.map((p) => this.packet(p, token)));
      batch = [];
    };
    for (const p of packets) {
      if (!p.with) await flush();
      if (token !== this.token) return false;
      batch.push(p);
    }
    await flush();
    if (token !== this.token) return false;
    this.render(step);
    return true;
  }

  packet(p, token) {
    const link = this.spec.links.find((l) => l.id === p.link);
    if (!link) return Promise.resolve();
    const path = link.pathEl;
    const len = path.getTotalLength();
    const g = el('g', {}, this.layers.packets);
    const dot = el('circle', { r: 10, class: `packet ${p.tone || ''}` }, g);
    const label = p.label ? el('text', { class: 'packet-label', 'text-anchor': 'middle' }, g) : null;
    if (label) label.textContent = this.label(p.label);
    const at = (t) => {
      const d = p.reverse ? 1 - t : t;
      const pt = path.getPointAtLength(d * len);
      dot.setAttribute('cx', pt.x);
      dot.setAttribute('cy', pt.y);
      if (label) {
        label.setAttribute('x', pt.x);
        label.setAttribute('y', pt.y - 16);
      }
    };
    at(0);
    const o = { t: 0 };
    const duration = Math.min(1500, 450 + len * 1.4) * PACE;
    return new Promise((resolve) => {
      const done = () => {
        if (token === this.token) g.remove();
        resolve();
      };
      const anim = animate(o, {
        t: p.bounce ? [0, 0.8, 0] : 1,
        duration: p.bounce ? duration * 1.6 : duration,
        ease: 'inOutQuad',
        onUpdate: () => at(o.t),
        onComplete: done,
      });
      this.running.push(anim);
    });
  }
}

// Wires every scene on the page to reveal's fragment events: each scene owns
// one hidden fragment per step, so → plays the next step and ← rewinds one.
export class SceneDirector {
  constructor(reveal, registry, text) {
    this.reveal = reveal;
    markMotion();
    this.scenes = new Map();
    document.querySelectorAll('.reveal .scene[data-scene]').forEach((host) => {
      const spec = registry[host.dataset.scene];
      if (!spec) {
        console.warn(`scene ${host.dataset.scene}: no spec registered`);
        return;
      }
      spec.steps.forEach((_, i) => {
        const f = document.createElement('span');
        f.className = 'fragment scene-step';
        f.dataset.step = i;
        host.appendChild(f);
      });
      this.scenes.set(host, new Scene(host, spec, (k) => text(host.dataset.scene, k)));
    });

    const sceneOf = (frag) => frag.classList.contains('scene-step') && this.scenes.get(frag.closest('.scene'));
    reveal.on('fragmentshown', (e) => {
      for (const f of e.fragments || [e.fragment]) {
        const s = sceneOf(f);
        if (s) s.play(Number(f.dataset.step));
      }
    });
    reveal.on('fragmenthidden', (e) => {
      for (const f of e.fragments || [e.fragment]) {
        const s = sceneOf(f);
        if (s) s.set(Number(f.dataset.step) - 1);
      }
    });
    const sync = () => {
      // Only the current slide's scenes keep playing; returning to a slide
      // resumes the loop of the step it was left on.
      const current = reveal.getCurrentSlide();
      for (const [host, s] of this.scenes) {
        const shown = host.querySelectorAll('.scene-step.visible').length;
        if (printing()) s.set(s.spec.steps.length - 1);
        else if (current && current.contains(host)) s.play(shown - 1);
        else s.set(shown - 1);
      }
    };
    reveal.on('slidechanged', sync);
    reveal.on('ready', sync);
    this.sync = sync;
  }

  relabel() {
    for (const s of this.scenes.values()) s.relabel();
  }

  // Applies a new motion mode to the slide on screen at once.
  setMotion(mode) {
    rememberMotion(mode);
    this.sync();
  }
}
