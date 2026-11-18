// The animated scenes of the RFC 29 deck. Labels are deck string keys, or
// literals after '=' (commands, file, cell and test names stay in English).
// The running example is the RFC's own: the cell pjdfstest/badger-s3/4.1,
// with the test names of §4.6's illustrative summary.json.

const S = 'scenes';

// Today's chain of wrappers above, the dt path below (§1, §3).
const box = (id, col, row, label, sub, extra = {}) => ({
  id, x: 20 + col * 300, y: row ? 200 : 40, w: 260, h: 84, label, sub, ...extra,
});

const layers = {
  viewBox: [1200, 310],
  nodes: [
    box('ci1', 0, 0, `${S}.layers.ci`, '=conformance.yml', { kind: 'client' }),
    box('runsh', 1, 0, '=run.sh', `${S}.layers.dispatcher`),
    box('scripts', 2, 0, `${S}.layers.scripts`, '=setup-posix.sh …'),
    box('tool1', 3, 0, '=prove', '=pjdfstest', { kind: 'store' }),
    box('ci2', 0, 1, `${S}.layers.ci`, '=tests.yml', { kind: 'client', hidden: true }),
    box('dt', 1, 1, '=dt', `${S}.layers.goCmd`, { kind: 'service', hidden: true }),
    box('tool2', 2, 1, '=prove', '=pjdfstest', { kind: 'store', hidden: true }),
    box('fixture', 3, 1, `${S}.layers.fixture`, '=dfs · badger-s3', { kind: 'adapter', hidden: true }),
  ],
  links: [
    { id: 'a1', from: 'ci1', to: 'runsh' },
    { id: 'a2', from: 'runsh', to: 'scripts' },
    { id: 'a3', from: 'scripts', to: 'tool1' },
    { id: 'b1', from: 'ci2', to: 'dt', hidden: true },
    { id: 'b2', from: 'dt', to: 'tool2', hidden: true },
    { id: 'b3', from: 'tool2', to: 'fixture', hidden: true },
  ],
  steps: [
    {
      focus: ['ci1', 'runsh', 'scripts', 'tool1'],
      packets: [{ link: 'a1' }, { link: 'a2' }, { link: 'a3' }],
    },
    {
      focus: ['scripts'],
      tone: { scripts: 'bad' },
      marks: [
        { on: 'scripts', text: `${S}.layers.copied`, tone: 'bad', below: true },
        { on: 'tool1', text: `${S}.layers.green`, tone: 'warn', below: true },
      ],
    },
    {
      focus: ['ci2', 'dt', 'tool2', 'fixture'],
      clear: true,
      show: ['ci2', 'dt', 'tool2', 'fixture', 'b1', 'b2', 'b3'],
      dim: ['ci1', 'runsh', 'scripts', 'tool1', 'a1', 'a2', 'a3'],
      packets: [{ link: 'b1' }, { link: 'b2' }, { link: 'b3' }],
      marks: [{ on: 'dt', text: `${S}.layers.oneCmd`, tone: 'good', dy: -2 }],
    },
  ],
};

// A pull request breaks one pjdfstest test ('Start here · Scenarios').
const pr = {
  viewBox: [1200, 290],
  nodes: [
    { id: 'pr', x: 20, y: 100, w: 210, h: 84, kind: 'client', label: `${S}.pr.pr`, sub: '=tier: pr' },
    { id: 'plan', x: 290, y: 100, w: 200, h: 84, kind: 'service', label: '=dt plan' },
    { id: 'cell', x: 550, y: 100, w: 380, h: 84, label: '=pjdfstest/badger-s3/4.1', sub: `${S}.pr.cell` },
    { id: 'gate', x: 990, y: 100, w: 190, h: 84, kind: 'service', label: '=gate' },
  ],
  links: [
    { id: 'p1', from: 'pr', to: 'plan' },
    { id: 'p2', from: 'plan', to: 'cell' },
    { id: 'p3', from: 'cell', to: 'gate' },
  ],
  steps: [
    {
      focus: ['pr', 'plan', 'cell'],
      packets: [{ link: 'p1' }, { link: 'p2' }],
      marks: [{ on: 'cell', text: `${S}.pr.prodOnly`, tone: 'info' }],
    },
    {
      focus: ['cell'],
      clear: true,
      tone: { cell: 'bad' },
      marks: [
        { on: 'cell', text: '=chmod/12.t · new', tone: 'bad' },
        { on: 'cell', text: '=exit 1', tone: 'bad', below: true },
      ],
    },
    {
      focus: ['cell'],
      marks: [{ on: 'plan', text: `${S}.pr.noRetry`, tone: 'warn', below: true }],
    },
    {
      focus: ['gate'],
      tone: { gate: 'bad' },
      packets: [{ link: 'p3', tone: 'bad' }],
      marks: [{ on: 'gate', text: `${S}.pr.red`, tone: 'bad', below: true }],
    },
  ],
};

// The bucket of a badger-s3 cell refuses writes ('Start here · Scenarios', §4.2).
const bucket = {
  viewBox: [1200, 300],
  nodes: [
    { id: 'dt', x: 20, y: 120, w: 200, h: 84, kind: 'service', label: '=dt', sub: '=pjdfstest/badger-s3/4.1' },
    { id: 'dfs', x: 320, y: 120, w: 260, h: 84, kind: 'adapter', label: '=dfs', sub: `${S}.bucket.fixture` },
    { id: 's3', x: 680, y: 120, w: 240, h: 84, kind: 'store', label: '=localstack', sub: `${S}.bucket.bucket` },
    { id: 'tool', x: 980, y: 120, w: 200, h: 84, kind: 'client', label: '=prove', sub: '=pjdfstest' },
  ],
  links: [
    { id: 'd1', from: 'dt', to: 'dfs' },
    { id: 'd2', from: 'dfs', to: 's3' },
    { id: 'd3', from: 'dt', to: 'tool', start: [120, 120], via: [[120, 40], [1080, 40]], end: [1080, 120], dashed: true },
  ],
  steps: [
    {
      focus: ['dt', 'dfs'],
      packets: [{ link: 'd1' }, { link: 'd1', reverse: true, tone: 'good' }],
      marks: [{ on: 'dfs', text: `${S}.bucket.answers`, tone: 'good', below: true }],
    },
    {
      focus: ['dfs', 's3'],
      tone: { s3: 'bad' },
      packets: [{ link: 'd2', label: '=PUT', tone: 'bad', bounce: true }],
      marks: [{ on: 's3', text: `${S}.bucket.refused`, tone: 'bad', below: true }],
    },
    {
      focus: ['dt'],
      dim: ['tool', 'd3'],
      tone: { dt: 'bad' },
      marks: [
        { on: 'tool', text: `${S}.bucket.noTest`, tone: 'warn', below: true },
        { on: 'dt', text: `${S}.bucket.exit2`, tone: 'bad', below: true },
      ],
    },
    {
      focus: ['dfs'],
      marks: [{ on: 'dfs', text: `${S}.bucket.noRetry`, tone: 'warn', dy: -2 }],
    },
  ],
};

// After a merge: a flaky test, then a real break ('Start here · Scenarios', §4.7, §4.8).
const develop = {
  viewBox: [1200, 260],
  nodes: [
    { id: 'merge', x: 20, y: 30, w: 230, h: 80, kind: 'client', label: `${S}.develop.merge`, sub: '=tier: merge' },
    { id: 'cell', x: 320, y: 30, w: 360, h: 80, label: '=pjdfstest/badger-s3/4.1', sub: '=--retry 1' },
    { id: 'history', x: 760, y: 30, w: 260, h: 80, kind: 'store', label: '=dt history' },
    { id: 'issue', x: 350, y: 165, w: 300, h: 80, kind: 'service', label: '=develop is red', sub: '=label: develop-red' },
    { id: 'own', x: 760, y: 165, w: 420, h: 80, kind: 'service', label: `${S}.develop.own`, sub: '=CELL TEST fails on develop' },
  ],
  links: [
    { id: 'm1', from: 'merge', to: 'cell' },
    { id: 'h1', from: 'cell', to: 'history' },
    { id: 'i1', from: 'cell', to: 'issue' },
    { id: 't1', from: 'issue', to: 'own' },
  ],
  steps: [
    {
      focus: ['merge', 'cell'],
      tone: { cell: 'bad' },
      packets: [{ link: 'm1' }],
      marks: [{ on: 'cell', text: '=rename/09.t ✗', tone: 'bad', below: true, dx: -90 }],
    },
    {
      focus: ['cell', 'history'],
      tone: { cell: 'good' },
      packets: [{ link: 'h1', tone: 'good' }],
      marks: [
        { on: 'cell', text: `${S}.develop.retryPass`, tone: 'good', below: true, dx: 110 },
        { on: 'history', text: `${S}.develop.flaky`, tone: 'info', below: true },
      ],
    },
    {
      focus: ['merge', 'cell', 'issue'],
      clear: true,
      tone: { cell: 'bad', issue: 'bad' },
      packets: [{ link: 'm1' }, { link: 'i1', tone: 'bad' }],
      marks: [{ on: 'cell', text: '=chmod/12.t ✗ · exit 1', tone: 'bad', dy: -2 }],
    },
    {
      focus: ['issue', 'own'],
      packets: [{ link: 't1', tone: 'bad' }],
      marks: [{ on: 'own', text: `${S}.develop.twice`, tone: 'warn', dy: -2 }],
    },
    {
      focus: ['issue'],
      clear: true,
      tone: { cell: 'good', issue: 'good' },
      marks: [{ on: 'issue', text: `${S}.develop.closed`, tone: 'good', dx: -175, dy: -2 }],
    },
  ],
};

export default { layers, pr, bucket, develop };
