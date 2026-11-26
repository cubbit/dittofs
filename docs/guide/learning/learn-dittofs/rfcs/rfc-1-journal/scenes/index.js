// The animated scenes of the RFC 1 deck. Labels are deck string keys, or
// literals after '=' (call names, versions and file names stay in English).

const S = 'scenes';

// alice-pc talks to the engine; the engine drives the journal beside it and
// reaches the bucket over the top, as in the RFC's own 'Start here' diagram.
const cast = (scene) => ({
  nodes: [
    { id: 'alice', x: 20, y: 110, w: 220, h: 84, kind: 'client', label: '=alice-pc', sub: `${S}.cast.client` },
    { id: 'engine', x: 330, y: 110, w: 250, h: 84, kind: 'service', label: `${S}.cast.engine` },
    { id: 'journal', x: 640, y: 110, w: 250, h: 84, kind: 'store', label: `${S}.cast.journal`, sub: '=N1 · NVMe' },
    { id: 'bucket', x: 950, y: 110, w: 230, h: 84, kind: 'store', label: '=dfs-data', sub: `${S}.cast.bucket` },
  ],
  links: [
    { id: 'ae', from: 'alice', to: 'engine' },
    { id: 'ej', from: 'engine', to: 'journal' },
    { id: 'eb', from: 'engine', to: 'bucket', start: [455, 110], via: [[455, 40], [1065, 40]], end: [1065, 110] },
  ],
  viewBox: [1200, 270],
  ...scene,
});

const careless = cast({
  steps: [
    {
      focus: ['alice', 'engine', 'journal'],
      packets: [
        { link: 'ae', label: '=WRITE' }, { link: 'ej' },
        { link: 'ej', reverse: true, tone: 'good' }, { link: 'ae', reverse: true, tone: 'good' },
      ],
      marks: [{ on: 'journal', text: `${S}.careless.v41dirty`, tone: 'info', below: true }],
    },
    {
      focus: ['engine', 'journal', 'bucket'],
      packets: [{ link: 'ej', reverse: true, label: '=offer v41' }, { link: 'eb', label: '=block' }],
      marks: [{ on: 'bucket', text: '=v41', tone: 'info', below: true }],
    },
    {
      focus: ['alice', 'journal'],
      clear: true,
      packets: [{ link: 'ae', label: '=WRITE' }, { link: 'ej' }],
      marks: [
        { on: 'journal', text: `${S}.careless.v42dirty`, tone: 'warn', below: true },
        { on: 'bucket', text: '=v41', tone: 'info', below: true },
      ],
    },
    {
      focus: ['engine', 'journal'],
      clear: true,
      tone: { journal: 'bad' },
      packets: [{ link: 'ej', label: '=report', tone: 'bad' }],
      marks: [
        { on: 'journal', text: `${S}.careless.byPosition`, tone: 'bad', below: true },
        { on: 'bucket', text: '=v41', tone: 'info', below: true },
      ],
    },
    {
      focus: ['journal'],
      clear: true,
      tone: { journal: 'good' },
      packets: [{ link: 'ej', label: '=report', tone: 'good' }],
      marks: [
        { on: 'journal', text: `${S}.careless.staysDirty`, tone: 'good', below: true },
        { on: 'bucket', text: '=v41', tone: 'info', below: true },
      ],
    },
  ],
});

const fill = cast({
  steps: [
    {
      focus: ['alice', 'engine', 'journal'],
      packets: [{ link: 'ae', label: '=READ' }, { link: 'ej' }, { link: 'ej', reverse: true, tone: 'warn' }],
      marks: [{ on: 'journal', text: `${S}.fill.missing`, tone: 'info', below: true }],
    },
    {
      focus: ['engine', 'bucket'],
      packets: [{ link: 'eb', label: `${S}.fill.fetch` }],
    },
    {
      focus: ['alice', 'journal'],
      clear: true,
      packets: [{ link: 'ae', label: '=WRITE' }, { link: 'ej' }],
      marks: [{ on: 'journal', text: `${S}.fill.changed`, tone: 'warn', below: true }],
    },
    {
      focus: ['engine', 'journal'],
      clear: true,
      tone: { journal: 'good' },
      packets: [{ link: 'eb', reverse: true }, { link: 'ej', label: '=Fill v50', tone: 'bad', bounce: true }],
      marks: [{ on: 'journal', text: `${S}.fill.refused`, tone: 'bad', below: true }],
    },
  ],
});

// One file's offsets drawn to scale: 0 at x=100, 4K at x=400, 8K at x=700, 12K at x=1000.
const bar = (id, y, from, to, label, sub, extra = {}) => ({
  id, x: 100 + from * 75, y, w: (to - from) * 75, h: 70, label, sub, hidden: true, ...extra,
});

const extents = {
  viewBox: [1200, 330],
  nodes: [
    bar('w7', 20, 0, 8, '=write v7', '=(0, 8K)'),
    bar('w8', 120, 4, 12, '=write v8', '=(4K, 8K)'),
    bar('h7', 230, 0, 4, '=v7', '=(0, 4K)', { kind: 'store' }),
    bar('h8', 230, 4, 12, '=v8', '=(4K, 8K)', { kind: 'store' }),
  ],
  links: [],
  steps: [
    { focus: ['w7'], show: ['w7'] },
    { focus: ['w8'], show: ['w8'] },
    {
      focus: ['h7', 'h8'],
      show: ['h7', 'h8'],
      dim: ['w7'],
      marks: [{ on: 'w7', text: `${S}.extents.notHeld`, tone: 'warn', dx: 150 }],
    },
  ],
};

const chain = [
  ['client', null], ['dirty', 'WriteAt'], ['offered', 'Offload'], ['offloaded', 'report'],
  ['unreclaimed', 'Release'], ['freed', 'Repack'],
];
const life = {
  viewBox: [1200, 230],
  nodes: chain.map(([id, call], i) => ({
    id,
    x: 15 + i * 197,
    y: 60,
    w: 170,
    h: 84,
    kind: id === 'client' ? 'client' : id === 'freed' ? 'store' : undefined,
    label: id === 'client' ? '=alice-pc' : `${S}.life.${id}`,
    sub: call ? `=${call}` : undefined,
  })),
  links: chain.slice(1).map(([id], i) => ({ id, from: chain[i][0], to: id })),
  steps: [
    { focus: ['dirty'], packets: [{ link: 'dirty' }], marks: [{ on: 'dirty', text: `${S}.life.neverReleased`, tone: 'warn', below: true }] },
    { focus: ['offered'], packets: [{ link: 'offered' }], marks: [{ on: 'offered', text: `${S}.life.frozen`, tone: 'info', below: true }] },
    { focus: ['offloaded'], packets: [{ link: 'offloaded', tone: 'good' }], marks: [{ on: 'offloaded', text: `${S}.life.evictable`, tone: 'good', below: true }] },
    { focus: ['unreclaimed'], packets: [{ link: 'unreclaimed' }], marks: [{ on: 'unreclaimed', text: `${S}.life.stillOnDisk`, tone: 'warn', below: true }] },
    { focus: ['freed'], packets: [{ link: 'freed', tone: 'good' }], marks: [{ on: 'freed', text: `${S}.life.spaceBack`, tone: 'good', below: true }] },
  ],
};

const repack = {
  viewBox: [1200, 330],
  nodes: [
    { id: 'old', x: 30, y: 70, w: 300, h: 110, label: '=0041.seg', sub: `${S}.repack.sealed` },
    { id: 'new', x: 450, y: 70, w: 300, h: 110, kind: 'store', label: '=0042.seg', sub: `${S}.repack.active` },
    { id: 'fs', x: 880, y: 70, w: 290, h: 110, kind: 'service', label: `${S}.repack.free`, sub: `${S}.repack.fs` },
  ],
  links: [
    { id: 'cp', from: 'old', to: 'new' },
    { id: 'un', from: 'old', to: 'fs', via: [[180, 280], [1025, 280]], dashed: true },
  ],
  steps: [
    { focus: ['old'], marks: [{ on: 'old', text: `${S}.repack.live20`, tone: 'warn', below: true }] },
    {
      focus: ['old', 'new'],
      packets: [{ link: 'cp', tone: 'good' }, { link: 'cp', tone: 'good' }, { link: 'cp', tone: 'good' }],
      marks: [{ on: 'new', text: `${S}.repack.copied`, tone: 'info', below: true }],
    },
    {
      focus: ['fs'],
      clear: true,
      dim: ['old', 'cp'],
      packets: [{ link: 'un', tone: 'good' }],
      marks: [{ on: 'fs', text: `${S}.repack.back`, tone: 'good', below: true }],
    },
  ],
};

export default { careless, fill, extents, life, repack };
