// Boots a deck: loads its strings and the glossary in the chosen language,
// expands generated slides (term lists, glossary), renders every string,
// starts reveal and the scenes, and offers the animation mode, the language
// switch and ?check=1.
//
// A deck's index.html calls boot({ scenes }) from its own folder, beside its
// deck.json and i18n/.

import Reveal from '../third-party/reveal.js-5.2.1/reveal.esm.js';
import Notes from '../third-party/reveal.js-5.2.1/plugin/notes/notes.esm.js';
import { LANGS, SOURCE_LANG, Strings, loadJSON, pickLang, rememberLang, collectTerms, renderPlain, escapeHtml } from './i18n.js';
import { Glossary, TermPopover } from './glossary.js';
import { MOTION_MODES, SceneDirector, motionMode } from './scene.js';

const SHARED = new URL('../', import.meta.url); // learn-dittofs/
const WIDTH = 1280;
const HEIGHT = 720;

async function loadLang(lang) {
  const get = (url, optional) => loadJSON(url, { optional });
  const [ui, uiEn, gl, glEn, deck, deckEn] = await Promise.all([
    get(new URL(`i18n/${lang}.json`, SHARED), true),
    get(new URL(`i18n/${SOURCE_LANG}.json`, SHARED)),
    get(new URL(`glossary/${lang}.json`, SHARED), true),
    get(new URL(`glossary/${SOURCE_LANG}.json`, SHARED)),
    get(`i18n/${lang}.json`, true),
    get(`i18n/${SOURCE_LANG}.json`),
  ]);
  return {
    ui: new Strings(ui, uiEn),
    glossaryText: gl,
    glossaryEn: glEn,
    deck: new Strings(deck, deckEn, ui?.deck, uiEn.deck),
    deckEn: [deckEn, uiEn.deck],
  };
}

// Generated slides are built once: how many there are depends on the deck,
// not on the language. Their text is filled by render().
function expand(meta, usedTerms) {
  document.querySelectorAll('.reveal .slides > section[data-term-list], .reveal .slides > section[data-glossary]').forEach((tpl) => {
    const kind = tpl.dataset.termList || 'glossary';
    const ids = kind === 'glossary' ? [...usedTerms] : meta.terms[kind] || [];
    const per = Number(tpl.dataset.perSlide || ids.length || 1);
    const pages = Math.max(1, Math.ceil(ids.length / per));
    for (let p = 0; p < pages; p++) {
      const s = tpl.cloneNode(true);
      s.dataset.page = p;
      s.dataset.pages = pages;
      s.removeAttribute('id');
      if (p === 0 && tpl.id) s.id = tpl.id;
      tpl.before(s);
    }
    tpl.remove();
  });
}

function fillGenerated(section, ids, glossary, lang, ui) {
  const kind = section.dataset.termList || 'glossary';
  const per = Number(section.dataset.perSlide || ids.length || 1);
  const page = Number(section.dataset.page || 0);
  const pages = Number(section.dataset.pages || 1);
  const sorted = kind === 'glossary' ? glossary.sortedIds(ids, lang) : ids;
  const mine = sorted.slice(page * per, page * per + per);
  const counter = section.querySelector('[data-page-counter]');
  if (counter) counter.textContent = pages > 1 ? ` (${page + 1}/${pages})` : '';
  const host = section.querySelector('[data-term-host]');
  const style = section.dataset.style || 'list';
  if (style === 'chips') {
    host.innerHTML = `<div class="chips">${mine.map((id) => `<span class="chip">${glossary.html(`[[${id}]]`)}</span>`).join('')}</div>`;
    return;
  }
  const field = kind === 'conflicts' ? 'clash' : kind === 'glossary' ? 'long' : 'short';
  host.innerHTML = `<dl class="term-list${kind === 'glossary' ? ' glossary' : ''}">${mine.map((id) => {
    const e = glossary.entry(id);
    const miss = e.missing ? ' class="i18n-missing"' : '';
    // `target` (where the code is going) stays data only: decks explain the
    // design, not the progress of the work.
    const code = kind === 'glossary' && e.code?.length
      ? `<div class="code">${escapeHtml(e.code.join(' · '))}</div>`
      : '';
    return `<div${miss} data-glossary-term="${id}"><dt>${glossary.html(`[[${id}]]`)}</dt><dd>${glossary.html(e[field] || e.short || '')}${code}</dd></div>`;
  }).join('')}</dl>`;
}

// A quiz slide is `<section data-quiz="key" data-answer="a|b|c">`: the
// question, the three options and one explanation per option come from the
// deck's strings under `key`; the right letter sits in the HTML, so no
// translation can move it. The question and options are rebuilt on every
// render, and the pick lives on the section (data-picked), so a language
// switch keeps it. The verdict is a live region created once per quiz and only
// ever changes its text, so a screen reader announces the result of a pick.
const QUIZ_LETTERS = ['a', 'b', 'c'];

function renderQuiz(section, strings, missing, glossary, ui, plain) {
  const key = section.dataset.quiz;
  const answer = section.dataset.answer;
  let fellBack = false;
  const get = (k) => {
    const { value, missing: miss } = strings.get(`${key}.${k}`);
    if (miss) { fellBack = true; missing.push(`${key}.${k}`); }
    return value;
  };
  const question = get('question');
  const options = get('options') || [];
  const why = get('why') || [];
  section.classList.toggle('quiz-invalid', !QUIZ_LETTERS.includes(answer)
    || !question || options.length !== 3 || why.length !== 3);
  let body = section.querySelector(':scope > .quiz-body');
  if (!body) {
    body = document.createElement('div');
    body.className = 'quiz-body';
    body.innerHTML = '<div class="quiz-content"></div><p class="quiz-verdict" aria-live="polite"></p>';
    section.prepend(body);
  }
  const content = body.querySelector(':scope > .quiz-content');
  const live = body.querySelector(':scope > .quiz-verdict');
  body.classList.toggle('i18n-missing', fellBack);
  const picked = section.dataset.picked;
  const verdict = !picked ? ui('quiz.hint')
    : picked === answer ? ui('quiz.right')
      : ui('quiz.wrong').replace('{answer}', answer.toUpperCase());
  // Options render as plain text: a term link inside a button would nest
  // one control in another. The question and the explanations carry links.
  content.innerHTML = `<div class="kicker">${escapeHtml(ui('quiz.kicker'))}</div>`
    + `<h2>${glossary.html(question || '')}</h2>`
    + `<ol class="quiz-options">${options.map((o, i) => {
      const l = QUIZ_LETTERS[i];
      const cls = [l === answer ? 'right' : 'wrong', l === picked ? 'picked' : ''].join(' ');
      return `<li class="quiz-option ${cls}" data-option="${l}">`
        + `<button type="button"${picked ? ' aria-disabled="true"' : ''}${l === picked ? ' aria-pressed="true"' : ''}><span class="letter">${l.toUpperCase()}</span><span class="text">${escapeHtml(plain(o))}</span></button>`
        + `<p class="why">${glossary.html(why[i] || '')}</p></li>`;
    }).join('')}</ol>`;
  if (live.textContent !== verdict) live.textContent = verdict;
  section.classList.toggle('quiz-answered', Boolean(picked));
  section.classList.toggle('quiz-correct', picked === answer);
}

function render(ctx) {
  const { strings, glossary, lang, meta, usedTerms, ui } = ctx;
  document.documentElement.lang = lang;
  const missing = [];
  // The RFC provenance commit lives in deck.json alone; {commit} fills each
  // language's cover text from it, so the two cannot drift apart.
  const commit = escapeHtml(String(meta?.rfc?.commit ?? '').slice(0, 7));
  const val = (el, key) => {
    const { value, missing: miss } = strings.get(key);
    el.classList.toggle('i18n-missing', miss);
    if (miss) missing.push(key);
    if (value === undefined) console.warn(`missing string ${key}`);
    return typeof value === 'string' ? value.replaceAll('{commit}', commit) : value;
  };

  document.querySelectorAll('.reveal [data-i18n]').forEach((el) => {
    const v = val(el, el.dataset.i18n);
    el.innerHTML = v === undefined ? escapeHtml(`⟦${el.dataset.i18n}⟧`) : glossary.html(v);
  });
  document.querySelectorAll('.reveal [data-i18n-list]').forEach((el) => {
    const v = val(el, el.dataset.i18nList) || [];
    const frag = el.hasAttribute('data-fragments');
    const prev = [...el.children].map((li) => li.classList.contains('visible'));
    el.innerHTML = v.map((s, i) => `<li class="${frag ? `fragment${prev[i] ? ' visible' : ''}` : ''}">${glossary.html(s)}</li>`).join('');
  });
  document.querySelectorAll('.reveal [data-i18n-table]').forEach((el) => {
    const rows = val(el, el.dataset.i18nTable) || [];
    const [head, ...body] = rows;
    el.innerHTML = `<thead><tr>${(head || []).map((c) => `<th>${glossary.html(c)}</th>`).join('')}</tr></thead>`
      + `<tbody>${body.map((r) => `<tr>${r.map((c) => `<td>${glossary.html(c)}</td>`).join('')}</tr>`).join('')}</tbody>`;
  });

  document.querySelectorAll('.reveal .slides > section[data-term-list], .reveal .slides > section[data-glossary]').forEach((s) => {
    const kind = s.dataset.termList || 'glossary';
    const ids = kind === 'glossary' ? [...usedTerms] : meta.terms[kind] || [];
    fillGenerated(s, ids, glossary, lang, ui);
  });

  const plain = (src) => renderPlain(src, (id) => glossary.name(id));
  document.querySelectorAll('.reveal .slides > section[data-quiz]').forEach((s) => {
    renderQuiz(s, strings, missing, glossary, ui, plain);
  });

  // Only a term's first use on a slide is marked.
  document.querySelectorAll('.reveal .slides > section').forEach((s) => {
    const seen = new Set();
    s.querySelectorAll('.term').forEach((t) => {
      if (t.closest('.chip') || t.closest('dt')) return;
      t.classList.toggle('term-quiet', seen.has(t.dataset.term));
      seen.add(t.dataset.term);
    });
  });

  document.querySelectorAll('.reveal .term').forEach((t) => {
    if (!glossary.has(t.dataset.term)) {
      t.classList.add('i18n-missing');
      missing.push(`glossary:${t.dataset.term}`);
    }
  });
  return missing;
}

const icon = (d) => `<svg viewBox="0 0 16 16" width="15" height="15" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="${d}"/></svg>`;
const ICON_START = icon('M3 3v10M13 3 7 8l6 5z');
const ICON_HOME = icon('M2.5 7.5 8 3l5.5 4.5M4 6.5V13h3v-3.5h2V13h3V6.5');
// The animation modes: replay, play once then stop, still.
const ICON_MOTION = {
  loop: icon('M13 8a5 5 0 1 1-1.5-3.6M13 2.5v2.5h-2.5'),
  once: icon('M3.5 3.5v9l6.5-4.5zM12.5 3.5v9'),
  off: icon('M4.5 4.5h7v7h-7z'),
};

// The top-right bar: back to the first slide, back to the home page, the
// animation mode and the language switch. The returned function marks the
// pressed buttons and relabels the bar in a new language.
function chrome({ lang, motion, ui, onStart, onLang, onMotion }) {
  const bar = document.createElement('div');
  bar.className = 'deck-chrome';
  bar.setAttribute('role', 'group');
  const start = document.createElement('button');
  start.type = 'button';
  start.className = 'nav';
  // Drawn, not a glyph: the slide font has no "first slide" character.
  start.innerHTML = ICON_START;
  start.addEventListener('click', onStart);
  const home = document.createElement('a');
  home.className = 'nav';
  home.innerHTML = ICON_HOME;
  const sep = () => Object.assign(document.createElement('span'), { className: 'sep' });
  bar.append(start, home, sep());

  // A three-way switch, like the language one: each mode is a button, the
  // current one pressed.
  const motionGroup = document.createElement('span');
  motionGroup.className = 'motion';
  motionGroup.setAttribute('role', 'group');
  for (const m of MOTION_MODES) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'icon';
    b.dataset.motion = m;
    b.innerHTML = ICON_MOTION[m];
    b.addEventListener('click', () => onMotion(m));
    motionGroup.appendChild(b);
  }
  bar.append(motionGroup, sep());

  for (const l of LANGS) {
    const b = document.createElement('button');
    b.type = 'button';
    b.textContent = l.label;
    b.dataset.lang = l.id;
    b.addEventListener('click', () => onLang(l.id));
    bar.appendChild(b);
  }
  document.body.appendChild(bar);

  const current = { lang, motion, ui };
  const update = (next = {}) => {
    Object.assign(current, next);
    const label = current.ui;
    start.title = label('chrome.start');
    home.title = label('chrome.home');
    start.setAttribute('aria-label', start.title);
    home.setAttribute('aria-label', home.title);
    home.href = `${new URL('../index.html', import.meta.url).pathname}?lang=${current.lang}`;
    motionGroup.setAttribute('aria-label', label('chrome.motion'));
    motionGroup.querySelectorAll('button').forEach((b) => {
      b.title = `${label('chrome.motion')}: ${label(`chrome.motionModes.${b.dataset.motion}`)}`;
      b.setAttribute('aria-label', b.title);
      b.setAttribute('aria-pressed', String(b.dataset.motion === current.motion));
    });
    bar.querySelectorAll('button[data-lang]').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.lang === current.lang)));
  };
  update();
  return update;
}

// Measures every slide in the current language: content taller or wider than
// the canvas, and strings that fell back to English.
function check(reveal, missing) {
  const here = reveal.getIndices();
  const results = [];
  // A quiz is measured answered, its tallest state: every explanation shown.
  document.documentElement.classList.add('quiz-measure');
  reveal.getSlides().forEach((s, i) => {
    reveal.slide(i);
    const over = Math.max(s.scrollHeight - s.clientHeight, 0);
    const wide = Math.max(s.scrollWidth - s.clientWidth, 0);
    const pre = [...s.querySelectorAll('pre')].some((p) => p.scrollWidth > p.clientWidth + 1);
    const quiz = s.classList.contains('quiz-invalid');
    s.classList.toggle('overflowing', over > 1 || wide > 1 || pre || quiz);
    const miss = [...s.querySelectorAll('.i18n-missing')].length;
    if (over > 1 || wide > 1 || pre || quiz || miss) {
      results.push({ slide: i + 1, id: s.id || '', over, wide, pre, quiz, missing: miss });
    }
  });
  document.documentElement.classList.remove('quiz-measure');
  reveal.slide(here.h, here.v, here.f);
  let box = document.querySelector('.check-report');
  if (!box) {
    box = document.createElement('div');
    box.className = 'check-report';
    document.body.appendChild(box);
  }
  const keys = [...new Set(missing)];
  box.classList.toggle('clean', !results.length && !keys.length);
  box.innerHTML = results.length || keys.length
    ? results.map((r) => `slide ${r.slide}${r.id ? ` #${r.id}` : ''}: ${r.over ? `${r.over}px too tall ` : ''}${r.wide ? `${r.wide}px too wide ` : ''}${r.pre ? 'code too wide ' : ''}${r.quiz ? 'quiz needs a question, 3 options, 3 explanations and data-answer a|b|c ' : ''}${r.missing ? `${r.missing} fallback string(s)` : ''}`).join('<br>')
      + (keys.length ? `<br>fallback keys: ${keys.map(escapeHtml).join(', ')}` : '')
    : `${escapeHtml(document.documentElement.lang)}: every slide fits, no fallback`;
  return { lang: document.documentElement.lang, slides: results, missing: keys };
}

export async function boot({ scenes = {} } = {}) {
  const terms = await loadJSON(new URL('glossary/terms.json', SHARED));
  const meta = await loadJSON('deck.json');
  let lang = pickLang();
  let loaded = await loadLang(lang);

  // The glossary slides list every term the deck mentions, and every term a
  // popover of theirs mentions (short and clash texts), so a popover's links
  // land. Links inside a long text are not followed: one broad term such as
  // `durable` would pull a whole other RFC's vocabulary into the deck. Such a
  // term still opens a popover, without the jump to the glossary.
  const usedTerms = collectTerms(loaded.deckEn);
  for (const k of ['known', 'new', 'conflicts']) (meta.terms[k] || []).forEach((id) => usedTerms.add(id));
  for (let size = -1; size !== usedTerms.size;) {
    size = usedTerms.size;
    [...usedTerms].forEach((id) => {
      const e = loaded.glossaryEn[id] || {};
      collectTerms([e.short, e.clash], usedTerms);
    });
  }
  expand(meta, usedTerms);

  const ctx = () => {
    const glossary = new Glossary(terms, loaded.glossaryText, loaded.glossaryEn);
    const ui = (k) => loaded.ui.get(k).value ?? k;
    return { strings: loaded.deck, glossary, lang, meta, usedTerms, ui };
  };
  let c = ctx();
  let missing = render(c);
  // The tab's title follows the language; the <title> in the HTML is only
  // what shows before the strings load.
  const htmlTitle = document.title;
  const retitle = () => {
    document.title = renderPlain(loaded.deck.get('title').value || htmlTitle, (id) => c.glossary.name(id));
  };
  retitle();

  const checkMode = new URLSearchParams(location.search).has('check');
  if (checkMode) document.documentElement.classList.add('check-mode');

  // A pick is final: the first click reveals which option is right and every
  // option's explanation. Not picking is fine too — → moves on as usual.
  const pick = (button) => {
    const option = button.closest('.quiz-option');
    const section = option.closest('section[data-quiz]');
    if (section.dataset.picked) return;
    section.dataset.picked = option.dataset.option;
    renderQuiz(section, c.strings, missing, c.glossary, c.ui, (src) => renderPlain(src, (id) => c.glossary.name(id)));
    section.querySelector(`.quiz-option[data-option="${section.dataset.picked}"] > button`)?.focus();
  };
  document.addEventListener('click', (e) => {
    const button = e.target.closest('.reveal .quiz-option > button');
    if (button) pick(button);
  });
  // Enter and Space answer the focused option; reveal would otherwise also
  // read Space as "next slide".
  document.addEventListener('keydown', (e) => {
    const button = e.target.closest && e.target.closest('.reveal .quiz-option > button');
    if (button && (e.key === 'Enter' || e.key === ' ')) {
      e.preventDefault();
      e.stopPropagation();
      pick(button);
    }
  }, true);

  const reveal = new Reveal({
    hash: true,
    width: WIDTH,
    height: HEIGHT,
    margin: 0.04,
    center: false,
    display: 'flex',
    transition: 'fade',
    transitionSpeed: 'fast',
    slideNumber: 'c/t',
    plugins: [Notes],
  });
  await reveal.initialize();

  // "Open in the glossary" jumps far ahead, so it leaves a way back: the
  // slide it left becomes a history entry of its own, and the browser's Back
  // returns to it (reveal follows the hash change). Ordinary navigation still
  // only replaces the current entry, as reveal's hash mode does.
  let jumpedFrom = null;
  const goToGlossary = (id, fromTerm) => {
    const target = document.querySelector(`section[data-glossary] [data-glossary-term="${id}"]`);
    const section = target && target.closest('section');
    if (!section) return;
    // reveal rewrites the hash at most once a second, so the URL may still
    // name an earlier slide, or name the glossary only later: both entries
    // are written here, the way reveal names a slide (its id, else its index).
    const url = (slide) => {
      const u = new URL(location.href);
      u.hash = `/${slide.id || reveal.getSlides().indexOf(slide)}`;
      return u;
    };
    history.replaceState(null, '', url(reveal.getCurrentSlide()));
    history.pushState(null, '', url(section));
    // Kept as a position, not the element: a language switch re-renders it.
    const fromSlide = fromTerm?.closest('.slides > section');
    jumpedFrom = fromSlide ? { slide: fromSlide, nth: [...fromSlide.querySelectorAll('.term')].indexOf(fromTerm) } : null;
    reveal.slide(reveal.getSlides().indexOf(section));
    target.querySelector('.term')?.focus({ preventScroll: true });
  };
  window.addEventListener('popstate', () => {
    // reveal follows a hashchange, but Chrome fires none when the entry was
    // replaced since (a language switch replaces it), so the slide the entry
    // names is shown here; reveal ignores a hashchange for the slide on screen.
    const name = decodeURIComponent(location.hash.replace(/^#\/?/, ''));
    const slides = reveal.getSlides();
    const named = name && document.getElementById(name);
    const index = named ? slides.indexOf(named.closest('.slides > section')) : Number(name || 0);
    if (index >= 0 && index < slides.length && index !== reveal.getIndices().h) reveal.slide(index);
    // An entry pushed before a language switch still names the old language:
    // the page keeps the one on screen and says so in the URL.
    if (new URL(location.href).searchParams.get('lang') !== lang) rememberLang(lang);
    const from = jumpedFrom;
    jumpedFrom = null;
    if (from && reveal.getCurrentSlide() === from.slide) from.slide.querySelectorAll('.term')[from.nth]?.focus({ preventScroll: true });
  });
  const popover = new TermPopover({ glossary: c.glossary, ui: c.ui, onGoToGlossary: goToGlossary, inGlossary: (id) => usedTerms.has(id) });

  const plain = (key) => renderPlain(loaded.deck.get(key).value ?? key, (id) => c.glossary.name(id));
  const director = new SceneDirector(reveal, scenes, (_scene, key) => plain(key));
  director.sync();

  // A language is switched to only once its strings have loaded: a failed
  // load keeps the language on screen, and its buttons, as they were.
  let wanted = lang;
  const switchLang = async (next) => {
    if (next === lang && wanted === lang) return;
    wanted = next;
    let fresh;
    try {
      fresh = await loadLang(next);
    } catch (err) {
      console.error(`language ${next}: ${err}`);
      if (wanted === next) wanted = lang;
      return;
    }
    if (wanted !== next) return; // a later click won
    lang = next;
    loaded = fresh;
    c = ctx();
    missing = render(c);
    retitle();
    popover.setGlossary(c.glossary, c.ui);
    director.relabel();
    rememberLang(lang);
    updateChrome({ lang, ui: c.ui });
    reveal.layout();
    if (checkMode) window.deckCheckResult = check(reveal, missing);
  };
  const updateChrome = chrome({
    lang,
    motion: motionMode(),
    ui: c.ui,
    onStart: () => reveal.slide(0),
    onLang: switchLang,
    onMotion: (mode) => {
      director.setMotion(mode);
      updateChrome({ motion: motionMode() });
    },
  });
  rememberLang(lang);

  window.deck = { reveal, check: () => check(reveal, missing), lang: () => lang };
  if (checkMode) window.deckCheckResult = check(reveal, missing);
}
