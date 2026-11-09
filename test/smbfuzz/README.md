# smbfuzz — differential SMB fuzzing against Samba

The tool [DESIGN.md](DESIGN.md) describes. It runs
random 12-step plans of file system calls, as several domain users, on a Samba share and on
a DittoFS share joined to the same Active Directory domain, and compares every errno and
every output. Each round starts from a tree of directories and files with content, listed in
the plan's `# tree` lines, and its steps draw their paths from a fixed pool that also holds
names nothing created yet.

Linux only, as root, with `docker` (or `podman`), `mount.cifs` and `keyutils`; `kinit` for
`--auth krb5`.

```sh
go build -o smbfuzz ./test/smbfuzz/cmd/smbfuzz

./smbfuzz fixture up                     # DC + Samba member + DittoFS + both cifs mounts
./smbfuzz plan --round 42                # print test 42 without running it
./smbfuzz run --from 1 --count 200       # run tests 1..200
./smbfuzz run --plan smbfuzz-reports/round-42/minimized.plan
./smbfuzz fixture down
```

`fixture up` takes `--auth ntlm|krb5` and `--profile sd|posix`; `run` reads them back from
the fixture's state (`/var/tmp/smbfuzz` unless `--state`).

Test N is always the same plan for a generator version (`--gen`, default the newest). A
diverging round is replayed three times; a stable one is minimised, and its report —
original and minimised plan, both sides' trace, the replay command — goes to
`smbfuzz-reports/round-N/`. An issue for a divergence quotes the report's summary line and
pastes `minimized.plan` whole: the plan is the reproducer (DESIGN.md §12.4). Exit status: 0 nothing new, 1 a new divergence, 2 rounds not run.

- `known.yaml` — accepted divergences, one per signature, each with its reason.
- `corpus/` — minimised plans of fixed bugs, replayed with `run --plan`.

| Package | What |
|---|---|
| `sd/` | the fuzzer's own security descriptor codec and SDDL with symbolic trustees |
| `fuzz/` | plans, domains, generator, worker, runner, comparison, minimiser, reports |
| `fixture/` | the topology: AD-DC container, Samba member (`ref.sh`), DittoFS, mounts |
| `cmd/smbfuzz/` | the command |
