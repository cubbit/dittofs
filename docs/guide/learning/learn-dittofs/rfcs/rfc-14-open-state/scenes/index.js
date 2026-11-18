// The animated scenes of the RFC 14 deck. Labels are deck string keys, or
// literals after '=' (wire names, code names, error names stay in English).

const S = 'scenes';

// alice-pc's persistent open across a restart of N1 (Start here, §4.2, §4.4, §8.1).
const grace = {
  viewBox: [700, 560],
  nodes: [
    { id: 'alice', x: 10, y: 40, w: 270, h: 76, kind: 'client', label: '=alice-pc', sub: `${S}.grace.smb` },
    { id: 'desk', x: 420, y: 40, w: 270, h: 76, kind: 'client', label: `${S}.grace.desk`, sub: `${S}.grace.smb` },
    { id: 'n1', x: 190, y: 232, w: 320, h: 96, kind: 'service', label: '=N1', sub: `${S}.grace.table` },
    { id: 'store', x: 190, y: 430, w: 320, h: 86, kind: 'store', label: `${S}.grace.store` },
  ],
  links: [
    { id: 'la', from: 'alice', to: 'n1' },
    { id: 'ld', from: 'desk', to: 'n1' },
    { id: 'ls', from: 'n1', to: 'store' },
  ],
  steps: [
    {
      focus: ['alice', 'n1', 'store'],
      packets: [{ link: 'la', label: '=CREATE' }, { link: 'ls' }],
      marks: [
        { on: 'n1', text: `${S}.grace.held`, tone: 'info' },
        { on: 'store', text: `${S}.grace.record`, tone: 'info', below: true },
      ],
    },
    {
      focus: ['n1'],
      clear: true,
      tone: { n1: 'bad' },
      marks: [
        { on: 'n1', text: `${S}.grace.lost`, tone: 'bad' },
        { on: 'store', text: `${S}.grace.kept`, tone: 'good', below: true },
      ],
    },
    {
      focus: ['n1', 'store'],
      clear: true,
      tone: { n1: null },
      packets: [{ link: 'ls', reverse: true, label: '=ServeShard' }],
      marks: [
        { on: 'n1', text: `${S}.grace.installed`, tone: 'info', below: true },
        { on: 'n1', text: `${S}.grace.inGrace`, tone: 'warn' },
      ],
    },
    {
      focus: ['desk', 'n1'],
      packets: [{ link: 'ld', label: `${S}.grace.openWrite` }, { link: 'ld', reverse: true, tone: 'warn', label: '=ErrGrace' }],
      marks: [{ on: 'desk', text: `${S}.grace.pending`, tone: 'warn', below: true }],
    },
    {
      focus: ['alice', 'n1'],
      tone: { alice: 'good' },
      packets: [{ link: 'la', label: `${S}.grace.reconnect` }, { link: 'la', reverse: true, tone: 'good' }],
      marks: [{ on: 'alice', text: `${S}.grace.back`, tone: 'good', below: true }],
    },
    {
      focus: ['desk', 'n1'],
      clear: true,
      tone: { desk: 'bad' },
      packets: [{ link: 'ld', label: `${S}.grace.openWrite` }, { link: 'ld', reverse: true, tone: 'bad' }],
      marks: [
        { on: 'n1', text: `${S}.grace.ended`, tone: 'good' },
        { on: 'desk', text: '=ErrShareViolation', tone: 'bad', below: true },
      ],
    },
  ],
};

// The data.bin scenario: one table serves both protocols (Start here, §7).
const cross = {
  viewBox: [1180, 340],
  nodes: [
    { id: 'editor', x: 20, y: 40, w: 230, h: 76, kind: 'client', label: `${S}.cross.editor`, sub: '=SMB' },
    { id: 'build', x: 20, y: 222, w: 230, h: 76, kind: 'client', label: '=build01', sub: '=NFS' },
    { id: 'smb', x: 360, y: 40, w: 210, h: 76, kind: 'adapter', label: `${S}.cross.smbAdapter` },
    { id: 'nfs', x: 360, y: 222, w: 210, h: 76, kind: 'adapter', label: `${S}.cross.nfsAdapter` },
    { id: 'table', x: 760, y: 118, w: 360, h: 104, kind: 'service', label: '=data.bin', sub: `${S}.cross.table` },
  ],
  links: [
    { id: 'es', from: 'editor', to: 'smb' },
    { id: 'st', from: 'smb', to: 'table' },
    { id: 'bn', from: 'build', to: 'nfs' },
    { id: 'nt', from: 'nfs', to: 'table' },
  ],
  steps: [
    {
      focus: ['editor', 'smb', 'table'],
      packets: [{ link: 'es', label: '=LOCK 0–4K' }, { link: 'st' }],
      marks: [{ on: 'table', text: `${S}.cross.smbLock`, tone: 'info' }],
    },
    {
      focus: ['build', 'nfs', 'table'],
      packets: [{ link: 'bn', label: '=WRITE 0–4K' }, { link: 'nt' }, { link: 'nt', reverse: true, tone: 'bad' }, { link: 'bn', reverse: true, tone: 'bad' }],
      marks: [{ on: 'build', text: `${S}.cross.mandatory`, tone: 'bad', below: true }],
    },
    {
      focus: ['build', 'editor', 'table'],
      clear: true,
      packets: [
        { link: 'bn', label: '=LOCK 8–12K' }, { link: 'nt' },
        { link: 'es', label: '=WRITE 8–12K' }, { link: 'st', tone: 'good' },
      ],
      marks: [
        { on: 'table', text: `${S}.cross.smbLock`, tone: 'info' },
        { on: 'table', text: `${S}.cross.nfsLock`, tone: 'info', below: true },
        { on: 'editor', text: `${S}.cross.advisory`, tone: 'good', below: true },
      ],
    },
    {
      focus: ['editor', 'smb', 'table'],
      packets: [{ link: 'es', label: '=LOCK 8–12K' }, { link: 'st' }, { link: 'st', reverse: true, tone: 'bad' }, { link: 'es', reverse: true, tone: 'bad' }],
      marks: [{ on: 'smb', text: `${S}.cross.lockRefused`, tone: 'bad', below: true }],
    },
  ],
};

// A recall, answered and then unanswered (§5.2, §5.3).
const recall = {
  viewBox: [1180, 340],
  nodes: [
    { id: 'holder', x: 20, y: 40, w: 250, h: 76, kind: 'client', label: `${S}.recall.holder`, sub: `${S}.recall.holderSub` },
    { id: 'other', x: 20, y: 222, w: 250, h: 76, kind: 'client', label: `${S}.recall.other` },
    { id: 'n1', x: 640, y: 122, w: 300, h: 96, kind: 'service', label: '=N1', sub: `${S}.recall.primary` },
  ],
  links: [
    { id: 'h', from: 'holder', to: 'n1' },
    { id: 'o', from: 'other', to: 'n1' },
  ],
  steps: [
    {
      focus: ['other', 'n1', 'holder'],
      packets: [
        { link: 'o', label: '=OPEN' }, { link: 'o', reverse: true, tone: 'warn', label: '=ErrDelay' },
        { link: 'h', reverse: true, tone: 'warn', label: `${S}.recall.recall`, with: true },
      ],
      marks: [{ on: 'n1', text: `${S}.recall.deadline`, tone: 'warn' }],
    },
    {
      focus: ['holder', 'n1'],
      packets: [{ link: 'h', label: '=WRITE' }, { link: 'h', label: '=AckRecall', tone: 'good' }],
      marks: [{ on: 'holder', text: `${S}.recall.flushed`, tone: 'good', below: true }],
    },
    {
      focus: ['other', 'n1'],
      tone: { other: 'good' },
      packets: [{ link: 'o', label: '=OPEN' }, { link: 'o', reverse: true, tone: 'good' }],
      marks: [{ on: 'other', text: `${S}.recall.granted`, tone: 'good', below: true }],
    },
    {
      focus: ['holder', 'n1'],
      clear: true,
      tone: { other: null, holder: 'bad' },
      packets: [{ link: 'h', reverse: true, tone: 'warn', label: `${S}.recall.recall` }],
      marks: [
        { on: 'holder', text: `${S}.recall.silent`, tone: 'bad', below: true },
        { on: 'n1', text: `${S}.recall.lease35`, tone: 'warn' },
        { on: 'n1', text: `${S}.recall.deleg90`, tone: 'bad', below: true },
      ],
    },
    {
      focus: ['holder', 'n1'],
      clear: true,
      packets: [{ link: 'h', label: '=WRITE' }, { link: 'h', reverse: true, tone: 'bad', label: '=ErrRevoked' }],
      marks: [
        { on: 'n1', text: `${S}.recall.deleg90`, tone: 'bad', below: true },
        { on: 'holder', text: `${S}.recall.fenced`, tone: 'bad', below: true },
      ],
    },
  ],
};

export default { grace, cross, recall };
