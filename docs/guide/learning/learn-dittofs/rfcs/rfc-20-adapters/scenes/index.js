// The animated scenes of the RFC 20 deck. Labels are deck string keys, or
// literals after '=' (wire names, code names, stage names stay in English).

const S = 'scenes';

const bypass = {
  viewBox: [1180, 420],
  nodes: [
    { id: 'alice', x: 20, y: 40, w: 210, h: 76, kind: 'client', label: '=alice-pc', sub: `${S}.bypass.smbClient` },
    { id: 'build', x: 20, y: 260, w: 210, h: 76, kind: 'client', label: '=build01', sub: `${S}.bypass.nfsClient` },
    { id: 'smb', x: 330, y: 40, w: 220, h: 76, kind: 'adapter', label: `${S}.bypass.smbAdapter` },
    { id: 'nfs', x: 330, y: 260, w: 220, h: 76, kind: 'adapter', label: `${S}.bypass.nfsAdapter` },
    { id: 'svc', x: 650, y: 140, w: 270, h: 96, kind: 'service', label: `${S}.bypass.service`, sub: '=RFC 17' },
    { id: 'eng', x: 960, y: 280, w: 200, h: 76, kind: 'store', label: `${S}.bypass.engine` },
  ],
  links: [
    { id: 'l1', from: 'alice', to: 'smb' },
    { id: 'l2', from: 'smb', to: 'svc' },
    { id: 'l3', from: 'build', to: 'nfs' },
    { id: 'l4', from: 'nfs', to: 'svc' },
    { id: 'l5', from: 'svc', to: 'eng' },
    { id: 'bypass', from: 'nfs', to: 'eng', via: [[440, 395], [1060, 395]], dashed: true, bad: true, hidden: true },
  ],
  steps: [
    {
      focus: ['alice', 'smb', 'svc'],
      packets: [{ link: 'l1', label: '=CREATE' }, { link: 'l2' }],
      marks: [{ on: 'svc', text: `${S}.bypass.markDeny`, tone: 'info' }],
    },
    {
      focus: ['build', 'nfs', 'svc'],
      packets: [{ link: 'l3', label: '=WRITE' }, { link: 'l4' }, { link: 'l4', reverse: true, tone: 'bad' }, { link: 'l3', reverse: true, tone: 'bad' }],
      marks: [{ on: 'build', text: '=NFS3ERR_ACCES', tone: 'bad', below: true }],
    },
    {
      focus: ['nfs', 'eng'],
      show: ['bypass'],
      tone: { eng: 'bad' },
      clear: true,
      packets: [{ link: 'l3', label: '=WRITE' }, { link: 'bypass', tone: 'bad' }],
      marks: [
        { on: 'svc', text: `${S}.bypass.markDeny`, tone: 'info' },
        { on: 'eng', text: `${S}.bypass.markWritten`, tone: 'bad' },
      ],
    },
  ],
};

const lifecycle = {
  viewBox: [1180, 260],
  nodes: [
    { id: 'configured', x: 20, y: 100, w: 230, h: 76, label: '=configured' },
    { id: 'serving', x: 330, y: 100, w: 230, h: 76, kind: 'service', label: '=serving' },
    { id: 'draining', x: 640, y: 100, w: 230, h: 76, label: '=draining' },
    { id: 'stopped', x: 940, y: 100, w: 220, h: 76, label: '=stopped' },
  ],
  links: [
    { id: 'cs', from: 'configured', to: 'serving', label: '=Start', labelDy: -6 },
    { id: 'loop', from: 'serving', to: 'serving', start: [395, 100], via: [[395, 40], [495, 40]], end: [495, 100], label: '=Reconfigure', labelDy: -4 },
    { id: 'sd', from: 'serving', to: 'draining', label: '=Stop', labelDy: -6 },
    { id: 'dt', from: 'draining', to: 'stopped' },
  ],
  steps: [
    { focus: ['configured'], marks: [{ on: 'configured', text: `${S}.lifecycle.noSocket`, tone: 'info', below: true }] },
    { focus: ['serving'], packets: [{ link: 'cs' }], marks: [{ on: 'serving', text: `${S}.lifecycle.bound`, tone: 'good', below: true }] },
    { focus: ['serving'], packets: [{ link: 'loop' }], marks: [] },
    { focus: ['draining'], packets: [{ link: 'sd' }], marks: [{ on: 'draining', text: `${S}.lifecycle.noNew`, tone: 'warn', below: true }] },
    { focus: ['stopped'], packets: [{ link: 'dt' }], marks: [{ on: 'stopped', text: `${S}.lifecycle.dropped`, tone: 'info', below: true }] },
  ],
};

const top = (id, i, label, sub) => ({ id, x: 30 + i * 163, y: 74, w: 140, h: 66, label, sub });
const bottom = (id, x, label) => ({ id, x, y: 324, w: 140, h: 66, label });

const pipeline = {
  viewBox: [1200, 410],
  nodes: [
    { id: 'g1', x: 14, y: 36, w: 1150, h: 120, kind: 'group', label: `${S}.pipeline.g1` },
    { id: 'g2', x: 988, y: 176, w: 176, h: 112, kind: 'group', label: `${S}.pipeline.g2` },
    { id: 'g3', x: 340, y: 290, w: 824, h: 112, kind: 'group', label: `${S}.pipeline.g3` },
    top('frame', 0, '=frame'),
    top('header', 1, '=decode', '=header'),
    top('unwrap', 2, '=unwrap'),
    top('admit', 3, '=admit'),
    top('replay', 4, '=replay', '=check'),
    top('args', 5, '=decode', '=args'),
    top('resolve', 6, '=resolve', '=IDs'),
    { id: 'call', x: 1008, y: 210, w: 140, h: 66, kind: 'service', label: '=call', sub: '=service' },
    bottom('map', 845, '=map error'),
    bottom('encode', 682, '=encode'),
    bottom('wrap', 519, '=wrap'),
    bottom('send', 356, '=send'),
    { id: 'client', x: 30, y: 324, w: 140, h: 66, kind: 'client', label: '=client' },
  ],
  links: [
    { id: 'in', from: 'client', to: 'frame' },
    { id: 'a', from: 'frame', to: 'header' },
    { id: 'b', from: 'header', to: 'unwrap' },
    { id: 'c', from: 'unwrap', to: 'admit' },
    { id: 'd', from: 'admit', to: 'replay' },
    { id: 'e', from: 'replay', to: 'args' },
    { id: 'f', from: 'args', to: 'resolve' },
    { id: 'g', from: 'resolve', to: 'call' },
    { id: 'h', from: 'call', to: 'map', via: [[1078, 357]] },
    { id: 'i', from: 'map', to: 'encode' },
    { id: 'j', from: 'encode', to: 'wrap' },
    { id: 'k', from: 'wrap', to: 'send' },
    { id: 'out', from: 'send', to: 'client' },
  ],
  steps: [
    {
      focus: ['frame', 'header', 'unwrap', 'admit', 'replay', 'args', 'resolve'],
      packets: ['in', 'a', 'b', 'c', 'd', 'e', 'f'].map((link) => ({ link })),
    },
    {
      focus: ['call'],
      packets: [{ link: 'g' }],
      marks: [{ on: 'call', text: `${S}.pipeline.reachesFile`, tone: 'info', dx: -190 }],
    },
    {
      focus: ['map', 'encode', 'wrap', 'send'],
      packets: ['h', 'i', 'j', 'k', 'out'].map((link) => ({ link, tone: 'good' })),
    },
    {
      focus: ['args'],
      clear: true,
      tone: { args: 'bad' },
      packets: [...['in', 'a', 'b', 'c', 'd'].map((link) => ({ link, tone: 'bad' })), { link: 'e', tone: 'bad', bounce: true }],
      marks: [{ on: 'args', text: '=GARBAGE_ARGS', tone: 'bad', below: true, dy: 6 }],
    },
  ],
};

const row = (suffix, y) => [
  { id: `c${suffix}`, x: 20, y, w: 170, h: 70, kind: 'client', label: `${S}.slow.client${suffix}` },
  { id: `r${suffix}`, x: 270, y, w: 170, h: 70, label: '=reader' },
  { id: `w${suffix}`, x: 520, y, w: 170, h: 70, kind: 'adapter', label: '=workers' },
  { id: `o${suffix}`, x: 770, y, w: 170, h: 70, label: '=writer' },
];
const rowLinks = (s, y) => [
  { id: `in${s}`, from: `c${s}`, to: `r${s}` },
  { id: `rw${s}`, from: `r${s}`, to: `w${s}` },
  { id: `wo${s}`, from: `w${s}`, to: `o${s}` },
  { id: `out${s}`, from: `o${s}`, to: `c${s}`, via: [[855, y], [105, y]] },
  { id: `sv${s}`, from: `w${s}`, to: 'svc' },
];

const slow = {
  viewBox: [1200, 380],
  nodes: [
    ...row('A', 60),
    ...row('B', 250),
    { id: 'svc', x: 1000, y: 150, w: 185, h: 80, kind: 'service', label: `${S}.slow.service` },
  ],
  links: [...rowLinks('A', 22), ...rowLinks('B', 358)],
  steps: [
    {
      focus: ['rA', 'wA', 'oA', 'rB', 'wB', 'oB'],
      packets: [
        { link: 'inA' }, { link: 'inB', with: true },
        { link: 'rwA' }, { link: 'rwB', with: true },
        { link: 'svA' }, { link: 'svB', with: true },
        { link: 'svA', reverse: true, tone: 'good' }, { link: 'svB', reverse: true, tone: 'good', with: true },
        { link: 'woA', tone: 'good' }, { link: 'woB', tone: 'good', with: true },
        { link: 'outA', tone: 'good' }, { link: 'outB', tone: 'good', with: true },
      ],
    },
    {
      focus: ['cA', 'oA'],
      packets: [{ link: 'woA', tone: 'good' }, { link: 'woA', tone: 'good' }],
      marks: [{ on: 'cA', text: `${S}.slow.notReading`, tone: 'warn', below: true, dy: 4 }, { on: 'oA', text: `${S}.slow.queueFull`, tone: 'warn', below: true, dy: 4 }],
    },
    {
      focus: ['rA'],
      dim: ['inA'],
      packets: [{ link: 'inA', bounce: true, tone: 'bad' }],
      marks: [{ on: 'rA', text: `${S}.slow.notRead`, tone: 'warn', below: true, dy: 4 }],
    },
    {
      focus: ['cA'],
      clear: true,
      tone: { cA: 'bad' },
      dim: ['rA', 'wA', 'oA', 'rwA', 'woA', 'outA', 'svA'],
      marks: [{ on: 'oA', text: `${S}.slow.closed`, tone: 'bad', below: true, dy: 4 }],
    },
    {
      focus: ['cB', 'rB', 'wB', 'oB'],
      tone: { cB: 'good' },
      packets: [{ link: 'inB' }, { link: 'rwB' }, { link: 'svB' }, { link: 'svB', reverse: true, tone: 'good' }, { link: 'woB', tone: 'good' }, { link: 'outB', tone: 'good' }],
      marks: [{ on: 'cB', text: `${S}.slow.unaffected`, tone: 'good' }],
    },
  ],
};

const wire = {
  viewBox: [1200, 372],
  nodes: [
    { id: 'script', x: 20, y: 146, w: 160, h: 80, kind: 'client', label: `${S}.wire.script` },
    { id: 'ref', x: 250, y: 50, w: 200, h: 80, label: `${S}.wire.reference` },
    { id: 'ditto', x: 250, y: 242, w: 200, h: 80, kind: 'service', label: '=DittoFS' },
    { id: 'capR', x: 510, y: 50, w: 170, h: 80, label: `${S}.wire.capture` },
    { id: 'capD', x: 510, y: 242, w: 170, h: 80, label: `${S}.wire.capture` },
    { id: 'norm', x: 760, y: 146, w: 200, h: 80, label: `${S}.wire.normalise` },
    { id: 'cmp', x: 1010, y: 146, w: 170, h: 80, label: `${S}.wire.compare` },
  ],
  links: [
    { id: 'sr', from: 'script', to: 'ref' },
    { id: 'sd', from: 'script', to: 'ditto' },
    { id: 'rc', from: 'ref', to: 'capR' },
    { id: 'dc', from: 'ditto', to: 'capD' },
    { id: 'rn', from: 'capR', to: 'norm' },
    { id: 'dn', from: 'capD', to: 'norm' },
    { id: 'nc', from: 'norm', to: 'cmp' },
  ],
  steps: [
    {
      focus: ['script', 'ref', 'ditto'],
      packets: [{ link: 'sr', label: '=QUERY_DIRECTORY' }, { link: 'sd', with: true }],
    },
    {
      focus: ['capR', 'capD'],
      packets: [{ link: 'rc' }, { link: 'dc', with: true }],
      marks: [
        { on: 'capR', text: '=STATUS_BUFFER_OVERFLOW', tone: 'info' },
        { on: 'capD', text: '=STATUS_INVALID_PARAMETER', tone: 'info', below: true },
      ],
    },
    {
      focus: ['norm'],
      packets: [{ link: 'rn' }, { link: 'dn', with: true }, { link: 'nc' }],
    },
    {
      focus: ['cmp'],
      tone: { cmp: 'bad' },
      marks: [{ on: 'cmp', text: `${S}.wire.red`, tone: 'bad', below: true, dx: -20 }],
    },
  ],
};

export default { bypass, lifecycle, pipeline, slow, wire };
