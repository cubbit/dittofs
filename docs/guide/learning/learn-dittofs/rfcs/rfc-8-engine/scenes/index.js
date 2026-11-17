// The animated scenes of the RFC 8 deck. Labels are deck string keys, or
// literals after '=' (stage names, call names, block names stay in English).

const S = 'scenes';

// The engine in the middle: the journal above, the metadata store below, the
// bucket to the right, as in the RFC's own 'Start here' diagram.
const cast = (scene, withClient = true) => {
  const off = withClient ? 0 : -260;
  const nodes = [
    { id: 'engine', x: 300 + off, y: 120, w: 230, h: 84, kind: 'service', label: `${S}.cast.engine`, sub: '=N1' },
    { id: 'journal', x: 640 + off / 2, y: 16, w: 250, h: 84, kind: 'store', label: `${S}.cast.journal`, sub: '=NVMe' },
    { id: 'meta', x: 640 + off / 2, y: 224, w: 250, h: 84, kind: 'store', label: `${S}.cast.meta` },
    { id: 'bucket', x: 970, y: 120, w: 210, h: 84, kind: 'store', label: '=dfs-data', sub: `${S}.cast.bucket` },
  ];
  const links = [
    { id: 'ej', from: 'engine', to: 'journal' },
    { id: 'em', from: 'engine', to: 'meta' },
    { id: 'eb', from: 'engine', to: 'bucket' },
  ];
  if (withClient) {
    nodes.unshift({ id: 'alice', x: 20, y: 120, w: 200, h: 84, kind: 'client', label: '=alice-pc', sub: `${S}.cast.client` });
    links.unshift({ id: 'ae', from: 'alice', to: 'engine' });
  }
  return { nodes, links, viewBox: [1200, 360], ...scene };
};

const journey = cast({
  steps: [
    {
      focus: ['alice', 'engine', 'journal'],
      packets: [
        { link: 'ae', label: '=WRITE' }, { link: 'ej' },
        { link: 'ej', reverse: true, tone: 'good' }, { link: 'ae', reverse: true, tone: 'good' },
      ],
      marks: [{ on: 'journal', text: `${S}.journey.dirty`, tone: 'warn', below: true }],
    },
    {
      focus: ['alice', 'engine', 'journal', 'meta'],
      packets: [
        { link: 'ae', label: '=FLUSH' }, { link: 'ej', label: '=Sync' },
        { link: 'em', label: `${S}.journey.existence` },
        { link: 'ae', reverse: true, tone: 'good' },
      ],
      marks: [{ on: 'meta', text: `${S}.journey.existence`, tone: 'info', below: true }],
    },
    {
      focus: ['engine', 'journal', 'meta', 'bucket'],
      packets: [
        { link: 'ej', reverse: true, label: '=offer' }, { link: 'eb', label: '=block' },
        { link: 'em', label: '=commit refs' }, { link: 'ej', label: '=report', tone: 'good' },
      ],
      marks: [
        { on: 'bucket', text: '=B1', tone: 'info', below: true },
        { on: 'journal', text: `${S}.journey.offloaded`, tone: 'good', below: true },
      ],
    },
    {
      focus: ['engine', 'journal'],
      clear: true,
      packets: [{ link: 'ej', label: '=Release' }],
      marks: [
        { on: 'journal', text: `${S}.journey.released`, tone: 'warn', below: true },
        { on: 'bucket', text: '=B1', tone: 'info', below: true },
      ],
    },
    {
      focus: ['alice', 'engine', 'journal', 'meta', 'bucket'],
      clear: true,
      packets: [
        { link: 'ae', label: '=READ' }, { link: 'ej', label: '=1' }, { link: 'ej', reverse: true, tone: 'warn' },
        { link: 'em', label: '=2' }, { link: 'em', reverse: true },
        { link: 'eb', label: '=3' }, { link: 'eb', reverse: true, tone: 'good' },
        { link: 'ae', reverse: true, tone: 'good' },
      ],
      marks: [
        { on: 'journal', text: `${S}.journey.notHeld`, tone: 'warn', below: true },
        { on: 'meta', text: `${S}.journey.ref`, tone: 'info', below: true },
        { on: 'bucket', text: `${S}.journey.verified`, tone: 'good', below: true },
      ],
    },
  ],
});

const order = cast({
  steps: [
    {
      focus: ['engine', 'meta'],
      packets: [{ link: 'em', label: '=?' }, { link: 'em', reverse: true }],
      marks: [{ on: 'meta', text: `${S}.order.uncarved`, tone: 'info', below: true }],
    },
    {
      focus: ['journal', 'meta', 'bucket'],
      packets: [{ link: 'eb', label: '=B' }, { link: 'em', label: '=commit' }, { link: 'ej', label: '=Release' }],
      marks: [
        { on: 'meta', text: `${S}.order.uncarved`, tone: 'info', below: true },
        { on: 'journal', text: `${S}.order.released`, tone: 'warn', below: true },
        { on: 'bucket', text: '=B', tone: 'info', below: true },
      ],
    },
    {
      focus: ['engine', 'journal'],
      tone: { engine: 'bad' },
      packets: [{ link: 'ej', label: '=?' }, { link: 'ej', reverse: true, tone: 'bad' }],
      marks: [{ on: 'engine', text: `${S}.order.falseLost`, tone: 'bad', below: true }],
    },
    {
      focus: ['engine', 'journal', 'meta', 'bucket'],
      clear: true,
      tone: { engine: 'good' },
      packets: [
        { link: 'ej', label: '=1' }, { link: 'ej', reverse: true },
        { link: 'em', label: '=2' }, { link: 'em', reverse: true },
        { link: 'eb', reverse: true, tone: 'good' },
      ],
      marks: [
        { on: 'meta', text: `${S}.order.carved`, tone: 'good', below: true },
        { on: 'engine', text: `${S}.order.remote`, tone: 'good', below: true },
      ],
    },
  ],
}, false);

// The offload pipeline's seven states in a row, with the edge back to Dirty.
const states = ['Capture', 'Carve', 'Assemble', 'Intent', 'Put', 'Commit', 'Mark'];
const sid = (i) => states[i].toLowerCase();
const pipeline = {
  viewBox: [1200, 330],
  nodes: [
    ...states.map((name, i) => ({
      id: sid(i), x: 15 + i * 168, y: 40, w: 150, h: 80,
      kind: i === 6 ? 'store' : 'service', label: `=${name}`,
    })),
    { id: 'dirty', x: 500, y: 236, w: 200, h: 76, kind: 'store', label: '=Dirty', sub: `${S}.pipeline.reoffered` },
  ],
  links: [
    ...states.slice(1).map((_, i) => ({ id: `l${i}`, from: sid(i), to: sid(i + 1) })),
    { id: 'fail', from: 'put', to: 'dirty', start: [762, 120], via: [[762, 274]], end: [700, 274], dashed: true, bad: true },
    { id: 'again', from: 'dirty', to: 'capture', start: [500, 274], via: [[90, 274]], end: [90, 120], dashed: true },
  ],
  steps: [
    {
      focus: ['capture'],
      marks: [{ on: 'capture', text: `${S}.pipeline.guard`, tone: 'info', below: true }],
    },
    {
      focus: ['carve', 'assemble'],
      packets: [{ link: 'l0' }, { link: 'l1' }],
      marks: [
        { on: 'carve', text: `${S}.pipeline.chunks`, tone: 'info', below: true },
        { on: 'assemble', text: `${S}.pipeline.plan`, tone: 'info', below: true },
      ],
    },
    {
      focus: ['intent', 'put'],
      clear: true,
      packets: [{ link: 'l2' }, { link: 'l3', label: '=N1' }],
      marks: [
        { on: 'intent', text: '=Intent(N1)', tone: 'info', below: true },
        { on: 'put', text: `${S}.pipeline.sameName`, tone: 'info', below: true },
      ],
    },
    {
      focus: ['commit', 'mark'],
      clear: true,
      tone: { mark: 'good' },
      packets: [{ link: 'l4' }, { link: 'l5', label: '=report', tone: 'good' }],
      marks: [
        { on: 'commit', text: `${S}.pipeline.perBlock`, tone: 'info', below: true },
        { on: 'mark', text: `${S}.pipeline.evictable`, tone: 'good', below: true },
      ],
    },
    {
      focus: ['put', 'dirty', 'capture'],
      clear: true,
      tone: { mark: null, put: 'bad' },
      packets: [{ link: 'fail', tone: 'bad' }, { link: 'again', label: '=N2' }],
      marks: [{ on: 'put', text: `${S}.pipeline.abandoned`, tone: 'bad' }],
    },
  ],
};

// The group commit split: the half without file 17 commits at every level.
const col = (i) => 20 + i * 170;
const sizes = ['32', '16', '8', '4', '2'];
const levels = [
  { id: 'b0', x: col(0), y: 115, w: 120, h: 80, label: '=64', sub: `${S}.split.files` },
  ...sizes.flatMap((n, i) => [
    { id: `a${i + 1}`, x: col(i + 1), y: 30, w: 120, h: 80, label: `=${n}`, hidden: true },
    { id: `b${i + 1}`, x: col(i + 1), y: 200, w: 120, h: 80, label: `=${n}`, sub: '=+ file 17', hidden: true },
  ]),
  { id: 'a6', x: col(6), y: 30, w: 130, h: 80, label: '=1', hidden: true },
  { id: 'b6', x: col(6), y: 200, w: 130, h: 80, label: '=file 17', hidden: true },
];
const splitLinks = [];
for (let i = 0; i < 6; i++) {
  splitLinks.push({ id: `la${i}`, from: `b${i}`, to: `a${i + 1}`, hidden: true });
  splitLinks.push({ id: `lb${i}`, from: `b${i}`, to: `b${i + 1}`, hidden: true });
}
const mid = [2, 3, 4, 5];
const split = {
  viewBox: [1200, 330],
  nodes: levels,
  links: splitLinks,
  steps: [
    {
      focus: ['b0'],
      tone: { b0: 'bad' },
      marks: [{ on: 'b0', text: `${S}.split.conflict`, tone: 'bad', below: true }],
    },
    {
      focus: ['a1', 'b1'],
      clear: true,
      show: ['a1', 'b1', 'la0', 'lb0'],
      tone: { a1: 'good', b1: 'bad' },
      packets: [{ link: 'la0', tone: 'good' }, { link: 'lb0', tone: 'bad', with: true }],
      marks: [{ on: 'a1', text: `${S}.split.commits`, tone: 'good' }],
    },
    {
      focus: mid.flatMap((i) => [`a${i}`, `b${i}`]),
      clear: true,
      dim: ['b0', 'b1'],
      show: mid.flatMap((i) => [`a${i}`, `b${i}`, `la${i - 1}`, `lb${i - 1}`]),
      tone: Object.fromEntries(mid.flatMap((i) => [[`a${i}`, 'good'], [`b${i}`, 'bad']])),
      packets: mid.map((i) => ({ link: `lb${i - 1}`, tone: 'bad' })),
    },
    {
      focus: ['a6', 'b6'],
      dim: mid.map((i) => `b${i}`),
      show: ['a6', 'b6', 'la5', 'lb5'],
      tone: { a6: 'good', b6: 'bad' },
      packets: [{ link: 'la5', tone: 'good' }, { link: 'lb5', tone: 'bad', with: true }],
      marks: [{ on: 'b6', text: `${S}.split.alone`, tone: 'warn', below: true, dx: -30 }],
    },
  ],
};

export default { journey, order, pipeline, split };
