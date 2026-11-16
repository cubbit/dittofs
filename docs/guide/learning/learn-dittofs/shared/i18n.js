// String loading and the small markup a translated string may carry.
// Strings never contain HTML: [[term]], [[term|surface text]], **bold** and
// `code` are the whole vocabulary, so a translator cannot break a slide.

export const LANGS = [
  { id: 'en', label: 'EN' },
  { id: 'it', label: 'IT' },
  { id: 'pt-BR', label: 'PT' },
];
export const SOURCE_LANG = 'en';

const ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
export const escapeHtml = (s) => String(s).replace(/[&<>"']/g, (c) => ESC[c]);

export async function loadJSON(url, { optional = false } = {}) {
  const res = await fetch(url, { cache: 'no-cache' });
  if (!res.ok) {
    if (optional) return null;
    throw new Error(`${url}: ${res.status}`);
  }
  return res.json();
}

export function lookup(obj, path) {
  let cur = obj;
  for (const part of path.split('.')) {
    if (cur == null || typeof cur !== 'object') return undefined;
    cur = cur[part];
  }
  return cur;
}

// A string table for one language with the source language behind it. get()
// reports whether the value fell back, so check mode can show it.
export class Strings {
  constructor(primary, fallback) {
    this.primary = primary || {};
    this.fallback = fallback || {};
  }
  get(key) {
    const v = lookup(this.primary, key);
    if (v !== undefined) return { value: v, missing: false };
    return { value: lookup(this.fallback, key), missing: true };
  }
}

const INLINE = /\[\[([a-z0-9-]+)(?:\|([^\]]+))?\]\]|\*\*(.+?)\*\*|`([^`]+)`/g;

// Renders one string to HTML. termName(id) gives a term's display name in the
// current language, used when the string does not supply surface text.
export function renderInline(src, termName) {
  let out = '';
  let last = 0;
  const re = new RegExp(INLINE);
  let m;
  while ((m = re.exec(src))) {
    out += escapeHtml(src.slice(last, m.index));
    if (m[1]) {
      const text = m[2] ?? termName(m[1]);
      out += `<span class="term" data-term="${m[1]}" tabindex="0" role="button">${escapeHtml(text)}</span>`;
    } else if (m[3]) {
      out += `<strong>${renderInline(m[3], termName)}</strong>`;
    } else {
      out += `<code>${escapeHtml(m[4])}</code>`;
    }
    last = re.lastIndex;
  }
  return out + escapeHtml(src.slice(last));
}

// Plain text of a string, for SVG labels, where markup cannot be rendered.
export function renderPlain(src, termName) {
  return String(src)
    .replace(/\[\[([a-z0-9-]+)(?:\|([^\]]+))?\]\]/g, (_, id, text) => text ?? termName(id))
    .replace(/\*\*(.+?)\*\*/g, '$1')
    .replace(/`([^`]+)`/g, '$1');
}

// Every term id a string table mentions, walked recursively.
export function collectTerms(obj, into = new Set()) {
  if (typeof obj === 'string') {
    for (const m of obj.matchAll(/\[\[([a-z0-9-]+)/g)) into.add(m[1]);
  } else if (Array.isArray(obj)) {
    obj.forEach((v) => collectTerms(v, into));
  } else if (obj && typeof obj === 'object') {
    Object.values(obj).forEach((v) => collectTerms(v, into));
  }
  return into;
}

export function pickLang() {
  const ids = LANGS.map((l) => l.id);
  const fromUrl = new URLSearchParams(location.search).get('lang');
  if (ids.includes(fromUrl)) return fromUrl;
  try {
    const saved = localStorage.getItem('learn-dittofs.lang');
    if (ids.includes(saved)) return saved;
  } catch { /* storage unavailable: fall through */ }
  const nav = (navigator.language || '').toLowerCase();
  if (nav.startsWith('it')) return 'it';
  if (nav.startsWith('pt')) return 'pt-BR';
  return SOURCE_LANG;
}

export function rememberLang(lang) {
  try { localStorage.setItem('learn-dittofs.lang', lang); } catch { /* not persisted */ }
  const url = new URL(location.href);
  url.searchParams.set('lang', lang);
  history.replaceState(null, '', url);
}
