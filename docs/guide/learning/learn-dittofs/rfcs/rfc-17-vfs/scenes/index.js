// The animated scenes of the RFC 17 deck. Labels are deck string keys, or
// literals after '=' (wire names, code names stay in English).

const S = 'scenes';

// The six steps of every call ('Start here' figure): alice-pc's SMB write
// passes all six; build01's NFSv3 write on an anonymous open is refused at the
// open-state check by alice's deny mode.
const stepBox = (id, i, label, sub = `=${i + 1}`) => ({ id, x: 410 + i * 130, y: 134, w: 118, h: 76, label, sub });

const sixsteps = {
  viewBox: [1200, 320],
  nodes: [
    { id: 'alice', x: 10, y: 14, w: 160, h: 76, kind: 'client', label: '=alice-pc', sub: '=SMB' },
    { id: 'build', x: 10, y: 230, w: 160, h: 76, kind: 'client', label: '=build01', sub: '=NFSv3' },
    { id: 'smb', x: 205, y: 14, w: 180, h: 76, kind: 'adapter', label: `${S}.sixsteps.smbAdapter` },
    { id: 'nfs', x: 205, y: 230, w: 180, h: 76, kind: 'adapter', label: `${S}.sixsteps.nfsAdapter` },
    { id: 'svc', x: 396, y: 104, w: 792, h: 120, kind: 'group', label: `${S}.sixsteps.service` },
    stepBox('route', 0, `${S}.sixsteps.route`, `${S}.sixsteps.clusterOnly`),
    stepBox('admit', 1, `${S}.sixsteps.admit`),
    stepBox('authz', 2, `${S}.sixsteps.authorise`),
    stepBox('open', 3, `${S}.sixsteps.openState`),
    stepBox('quota', 4, `${S}.sixsteps.quota`),
    { ...stepBox('engine', 5, `${S}.sixsteps.engine`), kind: 'store' },
  ],
  links: [
    { id: 'as', from: 'alice', to: 'smb' },
    { id: 'bn', from: 'build', to: 'nfs' },
    { id: 'sr', from: 'smb', to: 'route', via: [[295, 160]], end: [410, 160] },
    { id: 'nr', from: 'nfs', to: 'route', via: [[295, 186]], end: [410, 186] },
    { id: 'ra', from: 'route', to: 'admit' },
    { id: 'aa', from: 'admit', to: 'authz' },
    { id: 'ao', from: 'authz', to: 'open' },
    { id: 'oq', from: 'open', to: 'quota' },
    { id: 'qe', from: 'quota', to: 'engine' },
  ],
  steps: [
    {
      focus: ['alice', 'smb', 'open'],
      packets: [{ link: 'as', label: '=WRITE 64 KiB' }],
      marks: [{ on: 'open', text: `${S}.sixsteps.markDeny`, tone: 'info' }],
    },
    {
      focus: ['route', 'admit', 'authz', 'open', 'quota', 'engine'],
      packets: ['sr', 'ra', 'aa', 'ao', 'oq', 'qe'].map((link) => ({ link, tone: 'good' })),
      marks: [
        { on: 'engine', text: `${S}.sixsteps.markStaged`, tone: 'good', below: true, dx: -30 },
        { on: 'alice', text: '=OK', tone: 'good', below: true },
      ],
    },
    {
      focus: ['build', 'nfs', 'open'],
      tone: { open: 'bad' },
      packets: [
        { link: 'bn', label: '=WRITE' },
        ...['nr', 'ra', 'aa'].map((link) => ({ link })),
        { link: 'ao', tone: 'bad', bounce: true },
      ],
      marks: [{ on: 'open', text: '=ErrShareViolation', tone: 'bad', below: true }],
    },
    {
      focus: ['nfs', 'build'],
      packets: [{ link: 'nr', reverse: true, tone: 'bad' }, { link: 'bn', reverse: true, tone: 'bad' }],
      marks: [{ on: 'build', text: `${S}.sixsteps.markNfsError`, tone: 'bad' }],
    },
  ],
};

// (cluster) The gateway routes; Serve fences and deduplicates at the primary;
// the filesystem decides. RFC 17 §2, §2.1, §4.7; RFC 15 §4.3.
const route = {
  viewBox: [1200, 330],
  nodes: [
    { id: 'gA', x: 190, y: 6, w: 590, h: 316, kind: 'group', label: `${S}.route.nodeA` },
    { id: 'gB', x: 810, y: 6, w: 380, h: 316, kind: 'group', label: `${S}.route.nodeB` },
    { id: 'client', x: 10, y: 46, w: 150, h: 76, kind: 'client', label: '=build01' },
    { id: 'adapter', x: 220, y: 46, w: 210, h: 76, kind: 'adapter', label: `${S}.route.adapter` },
    { id: 'gw', x: 220, y: 196, w: 210, h: 76, kind: 'service', label: '=vfs.Gateway' },
    { id: 'serveA', x: 520, y: 196, w: 230, h: 76, label: '=Serve', sub: `${S}.route.checked` },
    { id: 'fsA', x: 520, y: 46, w: 230, h: 76, kind: 'service', label: '=Filesystem', sub: `${S}.route.ownFs` },
    { id: 'serveB', x: 880, y: 196, w: 250, h: 76, label: '=Serve', sub: `${S}.route.fenceDedup` },
    { id: 'fsB', x: 880, y: 46, w: 250, h: 76, kind: 'service', label: '=Filesystem', sub: `${S}.route.decides` },
  ],
  links: [
    { id: 'ca', from: 'client', to: 'adapter' },
    { id: 'ag', from: 'adapter', to: 'gw', label: '=vfs.Service', labelDx: -62, labelDy: 10 },
    { id: 'gs', from: 'gw', to: 'serveA' },
    { id: 'sf', from: 'serveA', to: 'fsA' },
    { id: 'gt', from: 'gw', to: 'serveB', via: [[325, 300], [1005, 300]], label: '=Transport', labelDy: -2 },
    { id: 'sfB', from: 'serveB', to: 'fsB' },
  ],
  steps: [
    {
      focus: ['adapter', 'gw', 'serveA', 'fsA'],
      packets: [{ link: 'ca' }, { link: 'ag' }, { link: 'gs' }, { link: 'sf' }],
      marks: [{ on: 'serveA', text: `${S}.route.markLocal`, tone: 'info' }],
    },
    {
      focus: ['gw', 'serveB', 'fsB'],
      clear: true,
      packets: [{ link: 'ag' }, { link: 'gt', label: `${S}.route.envelope` }, { link: 'sfB' }],
      marks: [{ on: 'serveB', text: `${S}.route.markFence`, tone: 'good' }],
    },
    {
      focus: ['gw', 'serveB'],
      clear: true,
      packets: [
        { link: 'gt', reverse: true, tone: 'bad', bounce: true },
        { link: 'gt', label: `${S}.route.sameId` },
        { link: 'gt', reverse: true, tone: 'good' },
      ],
      marks: [{ on: 'serveB', text: `${S}.route.markDedup`, tone: 'good' }],
    },
    {
      focus: ['gw', 'adapter'],
      clear: true,
      packets: [{ link: 'gt', tone: 'bad', bounce: true }, { link: 'gs' }, { link: 'gs', reverse: true, tone: 'good' }, { link: 'ag', reverse: true, tone: 'good' }],
      marks: [
        { on: 'serveB', text: `${S}.route.markStale`, tone: 'bad' },
        { on: 'adapter', text: `${S}.route.markNever`, tone: 'good', below: true, dx: 110 },
      ],
    },
  ],
};

// A recall without a waiting worker (§3.2's figure).
const recall = {
  viewBox: [1200, 320],
  nodes: [
    { id: 'alice', x: 20, y: 14, w: 200, h: 76, kind: 'client', label: '=alice-pc', sub: `${S}.recall.holdsLease` },
    { id: 'build', x: 20, y: 230, w: 200, h: 76, kind: 'client', label: '=build01' },
    { id: 'smb', x: 320, y: 14, w: 210, h: 76, kind: 'adapter', label: `${S}.recall.smbAdapter` },
    { id: 'nfs', x: 320, y: 230, w: 210, h: 76, kind: 'adapter', label: `${S}.recall.nfsAdapter` },
    { id: 'svc', x: 700, y: 115, w: 320, h: 90, kind: 'service', label: `${S}.recall.service`, sub: `${S}.recall.atPrimary` },
  ],
  links: [
    { id: 'as', from: 'alice', to: 'smb' },
    { id: 'ss', from: 'smb', to: 'svc' },
    { id: 'bn', from: 'build', to: 'nfs' },
    { id: 'ns', from: 'nfs', to: 'svc' },
  ],
  steps: [
    {
      focus: ['build', 'nfs', 'svc'],
      packets: [{ link: 'bn', label: '=WRITE' }, { link: 'ns' }, { link: 'ss', reverse: true, label: '=RecallGrant', tone: 'warn' }, { link: 'as', reverse: true, label: `${S}.recall.leaseBreak`, tone: 'warn' }],
      marks: [{ on: 'svc', text: `${S}.recall.markRecall`, tone: 'warn' }],
    },
    {
      focus: ['svc', 'nfs', 'build'],
      packets: [{ link: 'ns', reverse: true, label: '=ErrDelay', tone: 'warn' }, { link: 'bn', reverse: true, tone: 'warn' }],
      marks: [{ on: 'build', text: '=NFS3ERR_JUKEBOX', tone: 'warn' }],
    },
    {
      focus: ['alice', 'smb', 'svc'],
      clear: true,
      packets: [{ link: 'as', label: '=ACK' }, { link: 'ss' }],
      marks: [{ on: 'svc', text: '=AckRecall', tone: 'good' }],
    },
    {
      focus: ['build', 'nfs', 'svc'],
      packets: [{ link: 'bn', label: `${S}.recall.retry` }, { link: 'ns' }, { link: 'ns', reverse: true, tone: 'good' }, { link: 'bn', reverse: true, tone: 'good' }],
      marks: [{ on: 'build', text: '=OK', tone: 'good' }],
    },
    {
      focus: ['alice', 'svc'],
      clear: true,
      tone: { alice: 'bad' },
      packets: [{ link: 'ss', reverse: true, label: '=RecallGrant', tone: 'warn' }, { link: 'as', reverse: true, tone: 'warn' }],
      marks: [
        { on: 'alice', text: `${S}.recall.markSilent`, tone: 'bad', below: true },
        { on: 'svc', text: `${S}.recall.markForced`, tone: 'bad' },
      ],
    },
  ],
};

export default { sixsteps, route, recall };
