// The animated scenes of the RFC 16 deck. Labels are deck string keys, or
// literals after '=' (key prefixes, call names and file names stay in English).

const S = 'scenes';

// Who holds which view: consumers on top, one store, one KV, two backends.
const views = {
  viewBox: [1200, 480],
  nodes: [
    { id: 'adapters', x: 10, y: 20, w: 220, h: 84, kind: 'adapter', label: `${S}.views.adapters`, sub: '=NFS · SMB' },
    { id: 'fs', x: 250, y: 20, w: 220, h: 84, kind: 'service', label: `${S}.views.fs`, sub: '=Namespace · Files · Capacity' },
    { id: 'engine', x: 490, y: 20, w: 220, h: 84, kind: 'service', label: `${S}.views.engine`, sub: '=Existence · Content · Lifecycle' },
    { id: 'gc', x: 730, y: 20, w: 220, h: 84, kind: 'service', label: '=GC', sub: '=Blocks' },
    { id: 'mgmt', x: 970, y: 20, w: 220, h: 84, kind: 'service', label: `${S}.views.mgmt`, sub: '=ControlPlane' },
    { id: 'store', x: 250, y: 180, w: 940, h: 90, kind: 'store', label: `${S}.views.store`, sub: `${S}.views.storeSub` },
    { id: 'kv', x: 490, y: 310, w: 460, h: 74, kind: 'service', label: '=KV', sub: '=Update · View · Limits' },
    { id: 'emb', x: 250, y: 410, w: 420, h: 64, kind: 'store', label: `${S}.views.embedded`, sub: `${S}.views.oneNode` },
    { id: 'rep', x: 770, y: 410, w: 420, h: 64, kind: 'store', label: `${S}.views.replicated`, sub: `${S}.views.cluster` },
  ],
  links: [
    { id: 'afs', from: 'adapters', to: 'fs' },
    { id: 'fss', from: 'fs', to: 'store', start: [360, 104], end: [360, 180] },
    { id: 'es', from: 'engine', to: 'store', start: [600, 104], end: [600, 180] },
    { id: 'gs', from: 'gc', to: 'store', start: [840, 104], end: [840, 180] },
    { id: 'ms', from: 'mgmt', to: 'store', start: [1080, 104], end: [1080, 180] },
    { id: 'sk', from: 'store', to: 'kv', start: [720, 270], end: [720, 310] },
    { id: 'ke', from: 'kv', to: 'emb', start: [600, 384], end: [460, 410] },
    { id: 'kr', from: 'kv', to: 'rep', start: [840, 384], end: [980, 410], dashed: true },
  ],
  steps: [
    {
      focus: ['adapters', 'fs'],
      packets: [{ link: 'afs', label: '=vfs.Service' }],
      marks: [{ on: 'adapters', text: `${S}.views.noView`, tone: 'warn', below: true }],
    },
    {
      focus: ['fs', 'engine', 'gc', 'mgmt', 'store'],
      clear: true,
      packets: [{ link: 'fss' }, { link: 'es', with: true }, { link: 'gs', with: true }, { link: 'ms', with: true }],
    },
    {
      focus: ['fs', 'engine', 'gc'],
    },
    {
      focus: ['store', 'kv'],
      packets: [{ link: 'sk' }],
    },
    {
      focus: ['kv', 'emb', 'rep'],
      packets: [{ link: 'ke' }, { link: 'kr', with: true }],
    },
  ],
};

// alice's three operations and the records each one writes, grouped by scope as
// in the RFC's own key figure. Records light up as a step writes them.
const E = `${S}.example`;
// A record: label is a string key under the example (or a literal after '='),
// sub is a string key under the example.
const rec = (id, x, y, label, sub, w = 200) => ({
  id, x, y, w, h: 72,
  label: label.startsWith('=') ? label : `${E}.${label}`,
  sub: `${E}.${sub}`,
});
const example = {
  viewBox: [1200, 500],
  nodes: [
    { id: 'alice', x: 10, y: 205, w: 200, h: 84, kind: 'client', label: '=alice-pc', sub: `${S}.cast.client` },
    { id: 'gf7', x: 240, y: 10, w: 440, h: 225, kind: 'group', label: `${E}.groupF7` },
    rec('fFile', 255, 50, '=File', 'fileF7'),
    rec('fOv', 465, 50, 'overwrite', 'overwriteSub'),
    rec('fRef', 255, 142, 'ref', 'refSub'),
    rec('fFence', 465, 142, 'fence', 'fenceSub'),
    { id: 'gd1', x: 240, y: 255, w: 440, h: 225, kind: 'group', label: `${E}.groupDir` },
    rec('d9', 255, 295, '=File (d9)', 'newDir'),
    rec('dEntry', 465, 295, '=Entry', 'entry'),
    rec('dDelta', 255, 387, 'timeDelta', 'timeDeltaSub'),
    rec('dFile', 465, 387, '=File (d1)', 'fileD1'),
    { id: 'gS', x: 700, y: 10, w: 235, h: 225, kind: 'group', label: `${E}.perShare` },
    rec('sVf', 717, 50, 'versionFloor', 'versionFloorSub'),
    rec('sUd', 717, 142, 'usageDelta', 'usageDeltaSub'),
    { id: 'gN', x: 700, y: 255, w: 235, h: 225, kind: 'group', label: `${E}.serverWide` },
    rec('nNode', 717, 295, '=N1', 'nodeRecord'),
    { id: 'gC', x: 955, y: 10, w: 235, h: 470, kind: 'group', label: `${E}.perNamespace` },
    rec('cI', 972, 50, 'putIntent', 'putIntentSub'),
    rec('cC', 972, 150, 'chunk', 'chunkSub'),
    rec('cB', 972, 250, 'block', 'blockSub'),
    rec('cCR', 972, 350, 'reverseRef', 'reverseRefSub'),
  ],
  links: [
    { id: 'af', from: 'alice', to: 'gf7', start: [110, 205], via: [[110, 122]], end: [240, 122] },
    { id: 'ad', from: 'alice', to: 'gd1', start: [110, 289], via: [[110, 367]], end: [240, 367] },
  ],
  steps: [
    {
      focus: ['fFile', 'fOv'],
      tone: { fFile: 'good', fOv: 'good' },
      packets: [{ link: 'af', label: '=flush' }],
    },
    {
      focus: ['fFile', 'fOv', 'sVf'],
      tone: { sVf: 'good' },
    },
    {
      focus: ['fFile', 'fOv', 'sVf', 'fFence', 'nNode'],
      marks: [
        { on: 'fFence', text: `${E}.guarded`, tone: 'info', below: true },
        { on: 'nNode', text: `${E}.guarded`, tone: 'info', below: true },
      ],
    },
    {
      clear: true,
      focus: ['cI'],
      tone: { fFile: null, fOv: null, sVf: null, cI: 'good' },
    },
    {
      focus: ['fRef', 'cC', 'cB'],
      tone: { cI: null, fRef: 'good', cC: 'good', cB: 'good' },
      dim: ['cI'],
    },
    {
      focus: ['fRef', 'cC', 'cB', 'cCR'],
      tone: { cCR: 'good' },
      dim: ['fFile', 'fOv'],
      marks: [{ on: 'fOv', text: `${E}.untouched`, tone: 'warn' }],
    },
    {
      clear: true,
      focus: ['d9', 'dEntry', 'dDelta', 'sUd'],
      undim: ['fFile', 'fOv', 'cI'],
      tone: { fRef: null, cC: null, cB: null, cCR: null, d9: 'good', dEntry: 'good', dDelta: 'good', sUd: 'good' },
      packets: [{ link: 'ad', label: '=mkdir' }],
      marks: [{ on: 'dFile', text: `${E}.guarded`, tone: 'info', below: true }],
    },
  ],
};

// Two creates in one directory: guards share; one folded name conflicts.
const G = `${S}.guards`;
const guards = {
  viewBox: [1200, 360],
  nodes: [
    { id: 'alice', x: 20, y: 40, w: 230, h: 84, kind: 'client', label: '=alice-pc', sub: `${S}.cast.client` },
    { id: 'build', x: 20, y: 240, w: 230, h: 84, kind: 'client', label: '=build01', sub: `${S}.cast.build` },
    { id: 'dir', x: 470, y: 40, w: 280, h: 84, kind: 'store', label: '=d1', sub: `${G}.dir` },
    { id: 'ea', x: 900, y: 40, w: 280, h: 72, kind: 'store', label: '=Entry a.txt', hidden: true },
    { id: 'eb', x: 900, y: 150, w: 280, h: 72, kind: 'store', label: '=Entry b.txt', hidden: true },
    { id: 'rep', x: 640, y: 240, w: 360, h: 84, kind: 'store', label: '=Entry report.docx', hidden: true },
  ],
  links: [
    { id: 'ad', from: 'alice', to: 'dir' },
    { id: 'bd', from: 'build', to: 'dir' },
    { id: 'ar', from: 'alice', to: 'rep', hidden: true },
    { id: 'br', from: 'build', to: 'rep', hidden: true },
  ],
  steps: [
    {
      focus: ['alice', 'build', 'dir'],
      packets: [{ link: 'ad', label: '=Guard' }, { link: 'bd', label: '=Guard', with: true }],
      marks: [{ on: 'dir', text: `${G}.twoGuards`, tone: 'info', below: true }],
    },
    {
      focus: ['ea', 'eb'],
      show: ['ea', 'eb'],
      tone: { alice: 'good', build: 'good' },
      marks: [
        { on: 'alice', text: `${G}.commit`, tone: 'good', below: true },
        { on: 'build', text: `${G}.commit`, tone: 'good', below: true },
      ],
    },
    {
      clear: true,
      focus: ['rep'],
      hide: ['ea', 'eb', 'ad', 'bd'],
      show: ['rep', 'ar', 'br'],
      dim: ['dir'],
      tone: { alice: null, build: 'bad', rep: 'bad' },
      packets: [{ link: 'ar', label: '=Set' }, { link: 'br', label: '=Set', with: true }],
      marks: [
        { on: 'rep', text: `${G}.conflict`, tone: 'bad', below: true },
        { on: 'alice', text: `${G}.commit`, tone: 'good', below: true },
      ],
    },
    {
      focus: ['build', 'rep'],
      tone: { build: null, rep: 'good' },
      packets: [{ link: 'br', label: '=retry', bounce: true }],
      marks: [{ on: 'build', text: `${G}.taken`, tone: 'warn', below: true }],
    },
  ],
};

// Group commit: three ready commits share one sync, then return.
const C = `${S}.commit`;
const commit = {
  viewBox: [1200, 330],
  nodes: [
    { id: 't1', x: 20, y: 10, w: 240, h: 80, kind: 'service', label: '=Update', sub: `${C}.t1` },
    { id: 't2', x: 20, y: 125, w: 240, h: 80, kind: 'service', label: '=Update', sub: `${C}.t2` },
    { id: 't3', x: 20, y: 240, w: 240, h: 80, kind: 'service', label: '=Update', sub: `${C}.t3` },
    { id: 'gc', x: 440, y: 125, w: 300, h: 80, kind: 'service', label: `${C}.group`, sub: `${C}.oneSync` },
    { id: 'dev', x: 880, y: 125, w: 300, h: 80, kind: 'store', label: '=N1 · NVMe', sub: `${C}.device` },
  ],
  links: [
    { id: 'l1', from: 't1', to: 'gc' },
    { id: 'l2', from: 't2', to: 'gc' },
    { id: 'l3', from: 't3', to: 'gc' },
    { id: 'ld', from: 'gc', to: 'dev' },
  ],
  steps: [
    {
      focus: ['t1', 't2', 't3', 'gc'],
      packets: [{ link: 'l1' }, { link: 'l2', with: true }, { link: 'l3', with: true }],
      marks: [{ on: 'gc', text: `${C}.waiting`, tone: 'info', below: true }],
    },
    {
      clear: true,
      focus: ['gc', 'dev'],
      packets: [{ link: 'ld', label: '=sync' }],
      marks: [{ on: 'dev', text: `${C}.synced`, tone: 'info', below: true }],
    },
    {
      clear: true,
      focus: ['t1', 't2', 't3'],
      tone: { t1: 'good', t2: 'good', t3: 'good' },
      packets: [
        { link: 'l1', reverse: true, tone: 'good' },
        { link: 'l2', reverse: true, tone: 'good', with: true },
        { link: 'l3', reverse: true, tone: 'good', with: true },
      ],
      marks: [{ on: 'gc', text: `${C}.returned`, tone: 'good', below: true }],
    },
    {
      clear: true,
      focus: ['dev'],
      tone: { dev: 'good' },
      marks: [{ on: 'dev', text: `${C}.restart`, tone: 'warn', below: true }],
    },
  ],
};

export default { views, example, guards, commit };
