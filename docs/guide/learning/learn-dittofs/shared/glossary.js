// The shared glossary: language-neutral facts in glossary/terms.json, text per
// language in glossary/<lang>.json, English behind every other language.

import { escapeHtml, renderInline } from './i18n.js';

export const REPO_URL = 'https://github.com/cubbit/dittofs/blob/develop/';

export class Glossary {
  constructor(terms, text, fallback) {
    this.terms = terms || {};
    this.text = text || {};
    this.fallback = fallback || {};
  }

  has(id) {
    return id in this.terms;
  }

  // The entry in the current language; missing marks a fallback to English
  // or, worse, a term with no text at all.
  entry(id) {
    const own = this.text[id];
    const en = this.fallback[id];
    const base = { id, ...(this.terms[id] || {}) };
    if (own && own.name) return { ...base, ...en, ...own, missing: false };
    if (en) return { ...base, ...en, missing: true };
    return { ...base, name: id, short: '', long: '', missing: true, unknown: true };
  }

  name(id) {
    return this.entry(id).name;
  }

  html(src) {
    return renderInline(src ?? '', (id) => this.name(id));
  }

  sortedIds(ids, lang) {
    const coll = new Intl.Collator(lang, { sensitivity: 'base' });
    return [...ids].sort((a, b) => coll.compare(this.name(a), this.name(b)));
  }
}

// Popover shown when a term is clicked or activated by keyboard.
export class TermPopover {
  constructor({ glossary, ui, onGoToGlossary, inGlossary = () => true }) {
    this.glossary = glossary;
    this.ui = ui;
    this.onGoToGlossary = onGoToGlossary;
    this.inGlossary = inGlossary;
    this.el = document.createElement('div');
    this.el.className = 'term-pop';
    this.el.hidden = true;
    this.el.setAttribute('role', 'dialog');
    document.body.appendChild(this.el);

    document.addEventListener('click', (e) => {
      const term = e.target.closest('.reveal .term');
      if (term) {
        e.preventDefault();
        e.stopPropagation();
        this.toggle(term);
      } else if (!this.el.contains(e.target)) {
        this.hide();
      }
    }, true);
    document.addEventListener('keydown', (e) => {
      const term = e.target.closest && e.target.closest('.reveal .term');
      if (term && (e.key === 'Enter' || e.key === ' ')) {
        e.preventDefault();
        e.stopPropagation();
        this.toggle(term);
      } else if (e.key === 'Escape' && !this.el.hidden) {
        e.stopPropagation();
        this.hide();
      }
    }, true);
    window.addEventListener('resize', () => this.hide());
  }

  setGlossary(glossary, ui) {
    this.glossary = glossary;
    this.ui = ui;
    this.hide();
  }

  toggle(termEl) {
    if (!this.el.hidden && this.anchor === termEl) {
      this.hide();
      return;
    }
    this.show(termEl);
  }

  show(termEl) {
    const id = termEl.dataset.term;
    const e = this.glossary.entry(id);
    const t = (k) => this.ui(k);
    let html = `<h4>${escapeHtml(e.name)}</h4>`;
    html += `<p>${this.glossary.html(e.short || e.long || t('glossary.noText'))}</p>`;
    if (e.clash) html += `<p class="clash">⚠ ${this.glossary.html(e.clash)}</p>`;
    const meta = [];
    if (e.source) {
      const label = e.sourceLabel || e.source.split('/').pop();
      meta.push(`<span>${escapeHtml(t('glossary.source'))}: <a href="${REPO_URL}${encodeURI(e.source).replace(/%23/g, '#')}" target="_blank" rel="noopener">${escapeHtml(label)}</a></span>`);
    }
    if (e.code && e.code.length) {
      meta.push(`<span>${escapeHtml(t('glossary.codeToday'))}: ${e.code.map((c) => `<code>${escapeHtml(c)}</code>`).join(', ')}</span>`);
    }
    const jump = this.inGlossary(id);
    if (jump) meta.push(`<span><button class="link" type="button">${escapeHtml(t('glossary.open'))} →</button></span>`);
    html += `<div class="meta">${meta.join('')}</div>`;
    this.el.innerHTML = html;
    if (jump) {
      this.el.querySelector('button.link').addEventListener('click', () => {
        this.hide();
        this.onGoToGlossary(id);
      });
    }
    this.anchor = termEl;
    this.el.hidden = false;
    this.place(termEl);
  }

  place(termEl) {
    const r = termEl.getBoundingClientRect();
    const pop = this.el.getBoundingClientRect();
    const margin = 10;
    let left = Math.min(Math.max(margin, r.left), window.innerWidth - pop.width - margin);
    let top = r.bottom + 8;
    if (top + pop.height > window.innerHeight - margin) top = Math.max(margin, r.top - pop.height - 8);
    this.el.style.left = `${left}px`;
    this.el.style.top = `${top}px`;
  }

  hide() {
    this.el.hidden = true;
    this.anchor = null;
  }
}
