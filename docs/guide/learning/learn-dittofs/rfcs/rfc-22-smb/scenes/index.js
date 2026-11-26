// The animated scenes of the RFC 22 deck. Labels are deck string keys, or
// literals after '=' (wire names, node names and statuses stay in English).

const S = 'scenes';

// Where SMB state lives: what a sign-in, a tree connect and a CREATE leave on
// the gateway node, and what they leave at the primary (§2, §4.1, §6.1).
const placement = {
  viewBox: [1200, 330],
  nodes: [
    { id: 'alice', x: 20, y: 120, w: 220, h: 86, kind: 'client', label: '=alice-pc', sub: `${S}.placement.client` },
    { id: 'p1', x: 460, y: 120, w: 240, h: 86, kind: 'adapter', label: '=P1', sub: `${S}.common.gateway` },
    { id: 's1', x: 900, y: 120, w: 280, h: 86, kind: 'service', label: '=S1', sub: `${S}.common.primary` },
  ],
  links: [
    { id: 'l1', from: 'alice', to: 'p1' },
    { id: 'l2', from: 'p1', to: 's1' },
  ],
  steps: [
    {
      focus: ['alice', 'p1'],
      packets: [{ link: 'l1', label: '=SESSION_SETUP' }, { link: 'l1', reverse: true, tone: 'good' }],
      marks: [{ on: 'p1', text: `${S}.placement.keys`, tone: 'info' }],
    },
    {
      focus: ['alice', 'p1'],
      packets: [{ link: 'l1', label: '=TREE_CONNECT' }, { link: 'l1', reverse: true, tone: 'good' }],
      marks: [{ on: 'p1', text: `${S}.placement.tree`, tone: 'info', below: true }],
    },
    {
      focus: ['p1', 's1'],
      packets: [{ link: 'l1', label: '=CREATE' }, { link: 'l2' }],
      marks: [
        { on: 's1', text: `${S}.placement.open`, tone: 'good' },
        { on: 's1', text: `${S}.placement.durable`, tone: 'good', below: true },
      ],
    },
    {
      focus: ['alice'],
      packets: [{ link: 'l2', reverse: true, tone: 'good' }, { link: 'l1', reverse: true, tone: 'good', label: '=FileId' }],
      marks: [{ on: 'alice', text: `${S}.placement.fileid`, tone: 'info', below: true, dx: 70 }],
    },
  ],
};

// A conflicting CREATE waits on a lease break without holding a worker (§6.3,
// and step 7 of §2).
const brk = {
  viewBox: [1200, 330],
  nodes: [
    { id: 'alice', x: 20, y: 30, w: 240, h: 84, kind: 'client', label: '=alice-pc', sub: `${S}.brk.holder` },
    { id: 'desk', x: 20, y: 222, w: 240, h: 84, kind: 'client', label: `${S}.brk.desk` },
    { id: 'p1', x: 420, y: 30, w: 200, h: 84, kind: 'adapter', label: '=P1' },
    { id: 'p2', x: 420, y: 222, w: 200, h: 84, kind: 'adapter', label: '=P2' },
    { id: 's1', x: 870, y: 126, w: 300, h: 84, kind: 'service', label: '=S1', sub: `${S}.common.primary` },
  ],
  links: [
    { id: 'la', from: 'alice', to: 'p1' },
    { id: 'lp1', from: 'p1', to: 's1' },
    { id: 'ld', from: 'desk', to: 'p2' },
    { id: 'lp2', from: 'p2', to: 's1' },
  ],
  steps: [
    {
      focus: ['desk', 'p2', 's1'],
      packets: [
        { link: 'ld', label: '=CREATE' }, { link: 'lp2' },
        { link: 'lp2', reverse: true, tone: 'warn' },
        { link: 'ld', reverse: true, tone: 'warn', label: '=STATUS_PENDING' },
      ],
      marks: [{ on: 'p2', text: `${S}.brk.freed`, tone: 'good', below: true }],
    },
    {
      focus: ['s1', 'p1', 'alice'],
      packets: [{ link: 'lp1', reverse: true, tone: 'warn', label: '=LEASE_BREAK' }, { link: 'la', reverse: true, tone: 'warn' }],
      marks: [{ on: 'alice', text: `${S}.brk.giveUp`, tone: 'warn', below: true }],
    },
    {
      focus: ['p2', 's1'],
      packets: [{ link: 'lp2', bounce: true, tone: 'warn' }, { link: 'lp2', bounce: true, tone: 'warn' }],
      marks: [{ on: 'p2', text: `${S}.brk.backoff`, tone: 'info' }],
    },
    {
      focus: ['alice', 's1', 'desk'],
      clear: true,
      packets: [
        { link: 'la', label: '=ACK' }, { link: 'lp1' },
        { link: 'lp2', bounce: true, tone: 'bad' },
        { link: 'ld', reverse: true, tone: 'bad' },
      ],
      marks: [
        { on: 's1', text: `${S}.brk.deny`, tone: 'bad' },
        { on: 'desk', text: '=STATUS_SHARING_VIOLATION', tone: 'bad', dx: 90 },
      ],
    },
    {
      focus: ['s1', 'alice'],
      clear: true,
      packets: [{ link: 'lp2', bounce: true, tone: 'warn' }],
      marks: [
        { on: 's1', text: `${S}.brk.timeout`, tone: 'warn' },
        { on: 'alice', text: `${S}.brk.fenced`, tone: 'warn', below: true, dx: 60 },
      ],
    },
  ],
};

// alice's day on a continuously available share: a network blip, a lost
// gateway node, a lost primary — and what a durable open would have met (§2,
// §4.3, §6.4).
const failover = {
  viewBox: [1200, 400],
  nodes: [
    { id: 'alice', x: 20, y: 160, w: 210, h: 84, kind: 'client', label: '=alice-pc' },
    { id: 'p1', x: 380, y: 50, w: 210, h: 84, kind: 'adapter', label: '=P1', sub: `${S}.common.gateway` },
    { id: 'p2', x: 380, y: 270, w: 210, h: 84, kind: 'adapter', label: '=P2', sub: `${S}.common.gateway` },
    { id: 's1', x: 820, y: 50, w: 250, h: 84, kind: 'service', label: '=S1', sub: `${S}.common.primary` },
    { id: 's2', x: 820, y: 270, w: 250, h: 84, kind: 'service', label: `${S}.failover.newPrimary`, hidden: true },
  ],
  links: [
    { id: 'a1', from: 'alice', to: 'p1' },
    { id: 'a2', from: 'alice', to: 'p2', hidden: true },
    { id: 'p1s1', from: 'p1', to: 's1' },
    { id: 'p2s1', from: 'p2', to: 's1', hidden: true },
    { id: 'p2s2', from: 'p2', to: 's2', hidden: true },
  ],
  steps: [
    {
      focus: ['alice', 'p1', 's1'],
      packets: [
        { link: 'a1', label: '=WRITE' }, { link: 'p1s1' },
        { link: 'p1s1', reverse: true, tone: 'good' }, { link: 'a1', reverse: true, tone: 'good' },
      ],
      marks: [{ on: 's1', text: `${S}.failover.synced`, tone: 'good' }],
    },
    {
      focus: ['alice', 'p1'],
      clear: true,
      packets: [
        { link: 'a1', bounce: true, tone: 'bad' },
        { link: 'a1', label: '=DH2C' }, { link: 'p1s1' },
        { link: 'p1s1', reverse: true, tone: 'good' }, { link: 'a1', reverse: true, tone: 'good' },
      ],
      marks: [
        { on: 'p1', text: `${S}.failover.previous`, tone: 'info' },
        { on: 's1', text: `${S}.failover.match`, tone: 'good' },
      ],
    },
    {
      focus: ['p2', 's1'],
      clear: true,
      tone: { p1: 'bad' },
      dim: ['a1', 'p1s1'],
      show: ['a2', 'p2s1'],
      packets: [
        { link: 'a2', label: '=DH2C' }, { link: 'p2s1' },
        { link: 'p2s1', reverse: true, tone: 'good' }, { link: 'a2', reverse: true, tone: 'good' },
      ],
      marks: [
        { on: 'p1', text: `${S}.failover.witness`, tone: 'info' },
        { on: 's1', text: `${S}.failover.noGrace`, tone: 'good' },
      ],
    },
    {
      focus: ['s2'],
      clear: true,
      tone: { s1: 'bad' },
      dim: ['p2s1'],
      show: ['s2', 'p2s2'],
      packets: [
        { link: 'a2', label: '=DH2C' }, { link: 'p2s2' },
        { link: 'p2s2', reverse: true, tone: 'good' }, { link: 'a2', reverse: true, tone: 'good' },
      ],
      marks: [{ on: 's2', text: `${S}.failover.persistent`, tone: 'good', below: true }],
    },
    {
      focus: ['alice', 's2'],
      clear: true,
      packets: [{ link: 'a2' }, { link: 'p2s2', bounce: true, tone: 'bad' }, { link: 'a2', reverse: true, tone: 'bad' }],
      marks: [
        { on: 's2', text: `${S}.failover.durable`, tone: 'bad', below: true },
        { on: 'alice', text: '=STATUS_OBJECT_NAME_NOT_FOUND', tone: 'bad', dx: 60 },
      ],
    },
  ],
};

export default { placement, brk, failover };
