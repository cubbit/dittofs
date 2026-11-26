// The animated scenes of the introduction deck. Labels are deck string keys, or
// literals after '=' (machine, bucket and block names stay in English).

const S = 'scenes';

// The whole node at a glance, after RFC 0 §1.3's layers: clients, adapters,
// the filesystem service, the three parts below it, then the journal's disk and
// the bucket. The control plane sits beside the data path, not in it.
const map = {
  viewBox: [1200, 400],
  nodes: [
    { id: 'alice', x: 10, y: 20, w: 160, h: 76, kind: 'client', label: '=alice-pc', sub: '=SMB' },
    { id: 'build', x: 10, y: 170, w: 160, h: 76, kind: 'client', label: '=build01', sub: '=NFS' },
    { id: 'dfsctl', x: 10, y: 318, w: 160, h: 70, kind: 'client', label: '=dfsctl' },
    { id: 'smb', x: 205, y: 20, w: 170, h: 76, kind: 'adapter', label: `${S}.map.smb`, sub: `${S}.map.adapter` },
    { id: 'nfs', x: 205, y: 170, w: 170, h: 76, kind: 'adapter', label: `${S}.map.nfs`, sub: `${S}.map.adapter` },
    { id: 'fs', x: 410, y: 58, w: 190, h: 150, kind: 'service', label: `${S}.map.fs`, sub: `${S}.map.fsSub` },
    { id: 'cp', x: 205, y: 318, w: 395, h: 70, label: `${S}.map.cp`, sub: `${S}.map.cpSub` },
    { id: 'open', x: 650, y: 8, w: 230, h: 70, label: `${S}.map.open` },
    { id: 'meta', x: 650, y: 98, w: 230, h: 70, kind: 'store', label: `${S}.map.meta` },
    { id: 'engine', x: 650, y: 188, w: 230, h: 70, kind: 'service', label: `${S}.map.engine` },
    { id: 'journal', x: 935, y: 140, w: 255, h: 76, kind: 'store', label: `${S}.map.journal`, sub: '=N1 · NVMe' },
    { id: 'bucket', x: 935, y: 250, w: 255, h: 76, kind: 'store', label: '=dfs-data', sub: `${S}.map.bucket` },
  ],
  links: [
    { id: 'as', from: 'alice', to: 'smb' },
    { id: 'bn', from: 'build', to: 'nfs' },
    { id: 'sf', from: 'smb', to: 'fs' },
    { id: 'nf', from: 'nfs', to: 'fs' },
    { id: 'fo', from: 'fs', to: 'open' },
    { id: 'fm', from: 'fs', to: 'meta' },
    { id: 'fe', from: 'fs', to: 'engine' },
    { id: 'ej', from: 'engine', to: 'journal' },
    { id: 'eb', from: 'engine', to: 'bucket' },
    { id: 'dc', from: 'dfsctl', to: 'cp' },
    { id: 'cm', from: 'cp', to: 'meta', dashed: true, start: [600, 353], via: [[625, 353], [625, 133]], end: [650, 133] },
    { id: 'em', from: 'engine', to: 'meta', start: [835, 188], end: [835, 168] },
  ],
  steps: [
    {
      focus: ['alice', 'build', 'smb', 'nfs', 'fs'],
      packets: [
        { link: 'as', label: '=SMB' }, { link: 'bn', label: '=NFS', with: true },
        { link: 'sf' }, { link: 'nf', with: true },
      ],
      marks: [{ on: 'fs', text: `${S}.map.decides`, tone: 'info', below: true }],
    },
    {
      focus: ['fs', 'open', 'meta'],
      packets: [{ link: 'fo' }, { link: 'fm', with: true }],
    },
    {
      focus: ['engine', 'journal', 'bucket'],
      packets: [{ link: 'fe', label: `${S}.map.bytes` }, { link: 'ej' }, { link: 'eb', label: `${S}.map.later` }],
      marks: [{ on: 'journal', text: `${S}.map.acked`, tone: 'good' }, { on: 'bucket', text: `${S}.map.offloaded`, tone: 'info', below: true }],
    },
    {
      focus: ['dfsctl', 'cp', 'meta'],
      clear: true,
      packets: [{ link: 'dc', label: '=REST' }, { link: 'cm' }],
      marks: [{ on: 'cp', text: `${S}.map.settings`, tone: 'info' }],
    },
  ],
};

// alice's profile disk on node N1, as in RFC 0's 'One write, followed end to
// end': the filesystem service in front, the engine in the middle, the journal
// and the metadata store beside it, the bucket at the far end.
const node = (scene) => ({
  viewBox: [1200, 300],
  nodes: [
    { id: 'alice', x: 10, y: 112, w: 160, h: 76, kind: 'client', label: '=alice-pc', sub: '=SMB' },
    { id: 'fs', x: 215, y: 112, w: 200, h: 76, kind: 'service', label: `${S}.node.fs`, sub: `${S}.node.fsSub` },
    { id: 'engine', x: 460, y: 112, w: 170, h: 76, kind: 'service', label: `${S}.node.engine` },
    { id: 'journal', x: 680, y: 14, w: 240, h: 76, kind: 'store', label: `${S}.node.journal`, sub: '=N1 · NVMe' },
    { id: 'meta', x: 680, y: 210, w: 240, h: 76, kind: 'store', label: `${S}.node.meta` },
    { id: 'bucket', x: 970, y: 112, w: 220, h: 76, kind: 'store', label: '=dfs-data', sub: `${S}.node.bucket` },
  ],
  links: [
    { id: 'af', from: 'alice', to: 'fs' },
    { id: 'fe', from: 'fs', to: 'engine' },
    { id: 'ej', from: 'engine', to: 'journal' },
    { id: 'em', from: 'engine', to: 'meta' },
    { id: 'eb', from: 'engine', to: 'bucket' },
  ],
  ...scene,
});

const write = node({
  steps: [
    {
      focus: ['alice', 'fs', 'engine', 'journal'],
      packets: [
        { link: 'af', label: '=WRITE 64 KiB' }, { link: 'fe' }, { link: 'ej' },
        { link: 'ej', reverse: true, tone: 'good' }, { link: 'fe', reverse: true, tone: 'good' }, { link: 'af', reverse: true, tone: 'good' },
      ],
      marks: [{ on: 'journal', text: '=Dirty', tone: 'warn', below: true }],
    },
    {
      focus: ['alice', 'journal', 'meta'],
      packets: [
        { link: 'af', label: '=FLUSH' }, { link: 'fe' },
        { link: 'ej', label: '=sync' }, { link: 'em', label: `${S}.write.existence`, with: true },
        { link: 'af', reverse: true, tone: 'good' },
      ],
      marks: [{ on: 'journal', text: `${S}.write.durable`, tone: 'good', below: true }],
    },
    {
      focus: ['engine', 'journal', 'bucket', 'meta'],
      clear: true,
      packets: [
        { link: 'ej', reverse: true, label: '=offer' },
        { link: 'eb', label: `${S}.write.putBlock` },
        { link: 'eb', reverse: true, tone: 'good' },
        { link: 'em', label: `${S}.write.chunksBlock` },
        { link: 'ej', label: '=report', tone: 'good' },
      ],
      marks: [
        { on: 'bucket', text: '=K1', tone: 'info', below: true },
        { on: 'journal', text: '=Resident', tone: 'good', below: true },
      ],
    },
    {
      focus: ['journal'],
      clear: true,
      dim: ['journal'],
      marks: [
        { on: 'bucket', text: '=K1', tone: 'info', below: true },
        { on: 'journal', text: `${S}.write.evicted`, tone: 'info', below: true },
      ],
    },
  ],
});

const read = node({
  steps: [
    {
      focus: ['alice', 'fs', 'engine', 'journal'],
      packets: [{ link: 'af', label: '=READ' }, { link: 'fe' }, { link: 'ej', bounce: true, tone: 'warn' }],
      marks: [{ on: 'journal', text: `${S}.read.notHeld`, tone: 'warn', below: true }],
    },
    {
      focus: ['engine', 'meta'],
      packets: [{ link: 'em', bounce: true }],
      marks: [
        { on: 'journal', text: `${S}.read.notHeld`, tone: 'warn', below: true },
        { on: 'meta', text: `${S}.read.chunkC`, tone: 'info' },
      ],
    },
    {
      focus: ['engine', 'bucket', 'alice'],
      clear: true,
      packets: [
        { link: 'eb', label: '=GET K1' }, { link: 'eb', reverse: true, tone: 'good' },
        { link: 'fe', reverse: true, tone: 'good' }, { link: 'af', reverse: true, tone: 'good' },
      ],
      marks: [{ on: 'engine', text: `${S}.read.verified`, tone: 'good', below: true }],
    },
    {
      focus: ['engine', 'journal'],
      clear: true,
      packets: [{ link: 'ej', label: '=fill' }],
      marks: [{ on: 'journal', text: `${S}.read.neverOver`, tone: 'info', below: true }],
    },
    {
      focus: ['engine', 'meta', 'journal', 'alice'],
      clear: true,
      tone: { engine: 'bad' },
      packets: [
        { link: 'ej', bounce: true, tone: 'warn' }, { link: 'em', bounce: true, tone: 'warn' },
        { link: 'fe', reverse: true, tone: 'bad' }, { link: 'af', reverse: true, tone: 'bad' },
      ],
      marks: [
        { on: 'meta', text: `${S}.read.writtenNoChunk`, tone: 'warn' },
        { on: 'alice', text: `${S}.read.error`, tone: 'bad', below: true },
      ],
    },
  ],
});

const restart = node({
  steps: [
    {
      focus: ['engine', 'meta', 'bucket'],
      dim: ['alice', 'fs'],
      packets: [{ link: 'em', label: `${S}.restart.intent` }, { link: 'eb', label: '=PUT K7' }],
      marks: [{ on: 'bucket', text: `${S}.restart.k7Half`, tone: 'warn', below: true }],
    },
    {
      focus: ['engine'],
      tone: { engine: 'bad' },
      marks: [
        { on: 'engine', text: `${S}.restart.crash`, tone: 'bad', below: true },
        { on: 'journal', text: '=Dirty', tone: 'warn', below: true },
      ],
    },
    {
      focus: ['journal'],
      clear: true,
      tone: { engine: null },
      marks: [
        { on: 'journal', text: `${S}.restart.rebuilt`, tone: 'good', below: true },
        { on: 'bucket', text: `${S}.restart.k7Half`, tone: 'warn', below: true },
      ],
    },
    {
      focus: ['engine', 'journal', 'meta', 'alice', 'fs'],
      undim: ['alice', 'fs'],
      packets: [{ link: 'ej', reverse: true }, { link: 'em', label: `${S}.restart.existence` }],
      marks: [
        { on: 'meta', text: `${S}.restart.intentGone`, tone: 'info' },
        { on: 'alice', text: `${S}.restart.serving`, tone: 'good', below: true },
      ],
    },
    {
      focus: ['engine', 'bucket'],
      clear: true,
      packets: [{ link: 'ej', reverse: true, label: '=offer' }, { link: 'eb', label: '=PUT K8' }, { link: 'eb', reverse: true, tone: 'good' }],
      marks: [
        { on: 'bucket', text: `${S}.restart.k7Gone`, tone: 'good', below: true },
      ],
    },
  ],
});

// One read served partly from the journal and partly from the bucket, drawn
// as the 1 MiB it asks for (not to scale: the 64 KiB is drawn wider so its
// label fits). Offsets are relative to 2 GiB.
const seg = (id, y, x0, x1, label, sub, extra = {}) => ({ id, x: x0, y, w: x1 - x0, h: 70, label, sub, ...extra });
const partial = {
  viewBox: [800, 520],
  nodes: [
    seg('req', 10, 20, 780, '=READ 1 MiB', `${S}.partial.request`, { kind: 'client' }),
    seg('held', 130, 20, 190, `${S}.partial.journal`, '=[0, 64 KiB)', { kind: 'store', hidden: true }),
    seg('c1', 260, 20, 210, '=C1', '=K1', { hidden: true }),
    seg('c2', 260, 210, 400, '=C2', '=K1', { hidden: true }),
    seg('c3', 260, 400, 590, '=C3', '=K1', { hidden: true }),
    seg('c4', 260, 590, 780, '=C4', '=K1', { hidden: true }),
    seg('r0', 400, 20, 190, `${S}.partial.fromJournal`, '=[0, 64 KiB)', { kind: 'store', hidden: true }),
    seg('r1', 400, 190, 780, `${S}.partial.fromChunks`, '=[64 KiB, 1 MiB)', { kind: 'service', hidden: true }),
  ],
  links: [],
  steps: [
    {
      focus: ['req', 'held'],
      show: ['held'],
      marks: [{ on: 'req', text: `${S}.partial.missing`, tone: 'warn', below: true, dx: 200 }],
    },
    {
      focus: ['c1', 'c2', 'c3', 'c4'],
      show: ['c1', 'c2', 'c3', 'c4'],
      marks: [{ on: 'c3', text: `${S}.partial.remote`, tone: 'info', below: true }],
    },
    {
      focus: ['c1', 'c2', 'c3', 'c4'],
      clear: true,
      tone: { c1: 'good', c2: 'good', c3: 'good', c4: 'good' },
      marks: [{ on: 'c3', text: `${S}.partial.only4`, tone: 'good', below: true }],
    },
    {
      focus: ['c1', 'r0', 'r1'],
      clear: true,
      show: ['r0', 'r1'],
      marks: [{ on: 'c1', text: `${S}.partial.dropped`, tone: 'warn', dx: 150 }],
    },
    {
      focus: ['held', 'r1'],
      clear: true,
      marks: [
        { on: 'r1', text: `${S}.partial.fill`, tone: 'info', below: true },
        { on: 'held', text: `${S}.partial.neverOver`, tone: 'good', below: true, dx: 230, dy: -50 },
      ],
    },
  ],
};

export default { map, write, read, partial, restart };
