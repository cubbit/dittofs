// The animated scenes of the RFC 2 deck. Labels are deck string keys, or
// literals after '=' (chunk names, sizes and the bucket's name stay as they are).

const S = 'scenes';

// The running example, left to right as in the RFC's own 'Start here' diagram:
// the journal's offer, the chunker, the carver, the block assembler, the bucket.
const stage = [
  ['journal', 'store', `${S}.cast.journal`, '=builds'],
  ['chunker', 'service', `${S}.cast.chunker`, `${S}.cast.where`],
  ['carver', 'service', `${S}.cast.carver`, `${S}.cast.what`],
  ['assembler', 'service', `${S}.cast.assembler`, `${S}.cast.pack`],
  ['bucket', 'store', '=dfs-data', `${S}.cast.bucket`],
];
const pipeline = {
  viewBox: [1200, 250],
  nodes: stage.map(([id, kind, label, sub], i) => ({ id, kind, label, sub, x: 12 + i * 240, y: 70, w: 196, h: 84 })),
  links: stage.slice(1).map(([id], i) => ({ id: `to-${id}`, from: stage[i][0], to: id })),
  steps: [
    {
      focus: ['journal', 'chunker'],
      packets: [{ link: 'to-chunker', label: '=offer' }],
      marks: [{ on: 'journal', text: `${S}.pipeline.files`, tone: 'info', below: true }],
    },
    {
      focus: ['chunker'],
      marks: [{ on: 'chunker', text: '=2,000 + ~160', tone: 'info', below: true }],
    },
    {
      focus: ['chunker', 'carver'],
      packets: [{ link: 'to-carver' }, { link: 'to-carver', with: true }],
      marks: [{ on: 'carver', text: `${S}.pipeline.ids`, tone: 'info', below: true }],
    },
    {
      focus: ['carver', 'assembler'],
      packets: [{ link: 'to-assembler' }],
      marks: [{ on: 'assembler', text: `${S}.pipeline.blocks`, tone: 'info', below: true }],
    },
    {
      focus: ['assembler', 'bucket'],
      packets: [{ link: 'to-bucket', label: '=block', tone: 'good' }],
      marks: [{ on: 'bucket', text: `${S}.pipeline.puts`, tone: 'good', below: true }],
    },
  ],
};

// A file's bytes drawn as bars: px is one unit of the drawing, not a byte count.
const bar = (id, x, y, w, label, extra = {}) => ({ id, x, y, w, h: 60, label, hidden: true, ...extra });
const tag = (id, y, label) => ({ id, x: 6, y, w: 120, h: 60, kind: 'group', label, hidden: true });

// One insert near the start, cut at a fixed size and cut by content.
const shift = {
  viewBox: [1200, 370],
  nodes: [
    tag('fixedTag', 20, `${S}.shift.fixed`),
    bar('fA', 140, 20, 260, '=A'), bar('fB', 400, 20, 260, '=B'), bar('fC', 660, 20, 260, '=C'), bar('fD', 920, 20, 260, '=D'),
    bar('gA', 140, 100, 260, "=A'"), bar('gB', 400, 100, 260, "=B'"), bar('gC', 660, 100, 260, "=C'"), bar('gD', 920, 100, 260, "=D'"),
    tag('cdcTag', 200, `${S}.shift.content`),
    bar('cP', 140, 200, 220, '=P'), bar('cQ', 360, 200, 330, '=Q'), bar('cR', 690, 200, 240, '=R'), bar('cS', 930, 200, 230, '=S'),
    bar('dP', 140, 280, 240, "=P'"), bar('dQ', 380, 280, 330, '=Q'), bar('dR', 710, 280, 240, '=R'), bar('dS', 950, 280, 230, '=S'),
  ],
  links: [],
  steps: [
    { focus: ['fA', 'fB', 'fC', 'fD'], show: ['fixedTag', 'fA', 'fB', 'fC', 'fD'] },
    {
      focus: ['gA', 'gB', 'gC', 'gD'],
      show: ['gA', 'gB', 'gC', 'gD'],
      tone: { gA: 'bad', gB: 'bad', gC: 'bad', gD: 'bad' },
    },
    { focus: ['cP', 'cQ', 'cR', 'cS'], show: ['cdcTag', 'cP', 'cQ', 'cR', 'cS'], dim: ['fA', 'fB', 'fC', 'fD', 'gA', 'gB', 'gC', 'gD'] },
    {
      focus: ['dP', 'dQ', 'dR', 'dS'],
      show: ['dP', 'dQ', 'dR', 'dS'],
      tone: { dP: 'bad', dQ: 'good', dR: 'good', dS: 'good' },
    },
  ],
};

// One chunk's search, drawn to scale: 1 px per KiB from x = 120, so the 64 KiB
// minimum ends at 184 and the 1 MiB maximum at 1144.
const search = {
  viewBox: [1200, 300],
  nodes: [
    bar('skip', 120, 50, 64, '=Min'),
    bar('found', 184, 50, 226, `${S}.search.searched`),
    bar('long', 120, 190, 1024, `${S}.search.repeating`),
  ],
  links: [],
  steps: [
    { focus: ['skip'], show: ['skip'], marks: [{ on: 'skip', text: `${S}.search.notTested`, tone: 'info', dx: 40 }] },
    { focus: ['found'], show: ['found'], marks: [{ on: 'found', text: `${S}.search.window`, tone: 'info', below: true }] },
    {
      focus: ['found'],
      tone: { found: 'good' },
      marks: [{ on: 'found', text: `${S}.search.cut`, tone: 'good', dx: 113 }],
    },
    {
      focus: ['long'],
      show: ['long'],
      dim: ['skip', 'found'],
      tone: { long: 'bad' },
      marks: [{ on: 'long', text: `${S}.search.max`, tone: 'warn', dx: 400 }],
    },
  ],
};

// A growing log: the first offer stops at its byte limit, its tail stays
// Dirty, and the next offer starts on the last content-chosen cut.
const tail = {
  viewBox: [1200, 400],
  nodes: [
    bar('offer1', 60, 10, 760, `${S}.tail.offer1`, { hidden: false, kind: 'group' }),
    bar('c1', 60, 90, 260, '=C1'),
    bar('c2', 320, 90, 300, '=C2'),
    bar('held', 620, 90, 200, `${S}.tail.held`),
    bar('offer2', 620, 210, 560, `${S}.tail.offer2`, { kind: 'group' }),
    bar('c3', 620, 290, 280, '=C3'),
    bar('c4', 900, 290, 260, '=C4'),
  ],
  links: [],
  steps: [
    { focus: ['offer1'] },
    { focus: ['c1', 'c2'], show: ['c1', 'c2'], tone: { c1: 'good', c2: 'good' } },
    { focus: ['held'], show: ['held'], marks: [{ on: 'held', text: `${S}.tail.dirty`, tone: 'warn', below: true }] },
    {
      focus: ['offer2', 'c3', 'c4'],
      show: ['offer2', 'c3', 'c4'],
      dim: ['held'],
      tone: { c3: 'good', c4: 'good' },
      clear: true,
    },
  ],
};

export default { pipeline, shift, search, tail };
