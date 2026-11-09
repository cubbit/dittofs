//go:build linux

// Command smbfuzz is the differential SMB fuzzer DESIGN.md describes.
//
//	smbfuzz fixture up|down          bring the domain, Samba, DittoFS and mounts up or down
//	smbfuzz plan --round N           print round N's plan
//	smbfuzz run --from N --count M   run rounds N..N+M-1
//	smbfuzz run --plan FILE          run one plan file
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cubbit/dittofs/test/smbfuzz/fixture"
	"github.com/cubbit/dittofs/test/smbfuzz/fuzz"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "worker":
		err = worker()
	case "plan":
		err = planCmd(os.Args[2:])
	case "run":
		var code int
		code, err = runCmd(os.Args[2:])
		if err == nil {
			os.Exit(code)
		}
	case "fixture":
		err = fixtureCmd(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "smbfuzz:", err)
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: smbfuzz fixture up|down | plan --round N | run (--from N --count M | --plan FILE)")
	os.Exit(2)
}

func worker() error {
	var cfg fuzz.WorkerConfig
	if err := json.Unmarshal([]byte(os.Getenv(fuzz.WorkerEnv)), &cfg); err != nil {
		return fmt.Errorf("worker config: %w", err)
	}
	return fuzz.RunWorker(cfg, os.Stdin, os.Stdout)
}

func planCmd(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	round := fs.Uint64("round", 1, "round number")
	gen := fs.Int("gen", fuzz.LatestGen, "generator version")
	_ = fs.Parse(args)
	p, err := fuzz.Generate(*gen, *round)
	if err != nil {
		return err
	}
	fmt.Print(p.Format())
	return nil
}

func repoRoot() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return strings.TrimSpace(string(out))
}

func fixtureConfig(fs *flag.FlagSet, args []string) fixture.Config {
	state := fs.String("state", "/var/tmp/smbfuzz", "fixture state directory")
	auth := fs.String("auth", "ntlm", "ntlm or krb5")
	profile := fs.String("profile", "sd", "sd or posix")
	_ = fs.Parse(args)
	return fixture.Config{State: *state, Repo: repoRoot(), Auth: *auth, Profile: *profile, Log: os.Stderr}
}

func fixtureCmd(args []string) error {
	if len(args) < 1 {
		usage()
	}
	fs := flag.NewFlagSet("fixture", flag.ExitOnError)
	dfs := fs.String("dfs", "", "the dfs binary fixture up starts, built as the user")
	c := fixtureConfig(fs, args[1:])
	c.DFS = *dfs
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "up":
		env, err := fixture.Up(ctx, c)
		if err != nil {
			return err
		}
		fmt.Printf("fixture up: ref %s, dut %s\n", env.RefRoot, env.DutRoot)
		return nil
	case "down":
		return fixture.Down(ctx, c)
	}
	usage()
	return nil
}

func runCmd(args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	from := fs.Uint64("from", 1, "first round")
	count := fs.Uint64("count", 1, "number of rounds")
	gen := fs.Int("gen", fuzz.LatestGen, "generator version")
	planFile := fs.String("plan", "", "run this plan file instead of generated rounds")
	minimize := fs.Bool("minimise", true, "minimise stable failures")
	reports := fs.String("reports", "smbfuzz-reports", "where failure reports go")
	knownFile := fs.String("known", "", "known divergences (default test/smbfuzz/known.yaml)")
	stopFirst := fs.Bool("stop", false, "stop at the first new divergence")
	noProbe := fs.Bool("no-probe", false, "skip the fixture probes")
	c := fixtureConfig(fs, args)
	env, err := fixture.Load(c)
	if err != nil {
		return 2, err
	}
	c.Auth, c.Profile = env.Auth, env.Profile
	if *knownFile == "" {
		*knownFile = filepath.Join(c.Repo, "test/smbfuzz/known.yaml")
	}
	known, err := fuzz.LoadKnown(*knownFile)
	if err != nil {
		return 2, err
	}
	r, err := fuzz.NewRunner(*env, fixture.Hooks{C: c}, os.Stderr)
	if err != nil {
		return 2, err
	}
	defer r.Close()
	r.SetKnown(known)
	if !*noProbe {
		if err := r.Probe(); err != nil {
			fmt.Printf("NOT RUN: %v\n", err)
			return 2, nil
		}
	}
	var plans []*fuzz.Plan
	if *planFile != "" {
		f, err := os.Open(*planFile)
		if err != nil {
			return 2, err
		}
		p, err := fuzz.ParsePlan(f)
		_ = f.Close() // opened read-only: a failed close loses nothing
		if err != nil {
			return 2, err
		}
		plans = append(plans, p)
	} else {
		for n := *from; n < *from+*count; n++ {
			p, err := fuzz.Generate(*gen, n)
			if err != nil {
				return 2, err
			}
			plans = append(plans, p)
		}
	}
	kernel, _ := exec.Command("uname", "-r").Output()
	// The commit names what ran only when the tree is clean; "-dirty" says it
	// was built from uncommitted changes, which no commit reproduces.
	commit, _ := exec.Command("git", "-C", c.Repo, "describe", "--always", "--abbrev=9", "--dirty", "--exclude=*").Output()
	var passed, knownN, failed, notRun int
	for _, p := range plans {
		res := r.Run(p)
		switch {
		case res.Err != nil:
			notRun++
			fmt.Printf("round %d: NOT RUN: %v\n", p.Round, res.Err)
		case res.Diverge == nil && len(res.Known) > 0:
			knownN++
			fmt.Printf("round %d: ok but for known divergences %s\n", p.Round, strings.Join(res.Known, ", "))
		case res.Diverge == nil:
			passed++
			fmt.Printf("round %d: ok\n", p.Round)
		default:
			failed++
			fmt.Printf("round %d: DIVERGES at step %s: %s\n", p.Round, res.Diverge.Step, res.Signature())
			rp := &fuzz.Report{Round: p.Round, Gen: p.Gen, Profile: env.Profile, Auth: env.Auth,
				Kernel: strings.TrimSpace(string(kernel)), Commit: strings.TrimSpace(string(commit)), Original: res}
			stable, sigs := fuzz.Stability(r, res)
			rp.Stable, rp.Sigs = stable, sigs
			if stable && *minimize {
				rp.Minimized = fuzz.Minimize(r, res, os.Stderr)
			}
			dir, err := rp.Write(*reports)
			if err != nil {
				return 2, err
			}
			fmt.Printf("  report: %s\n", dir)
			if *stopFirst {
				goto done
			}
		}
	}
done:
	fmt.Printf("\n%d passed, %d known, %d diverged, %d not run\n", passed, knownN, failed, notRun)
	switch {
	case notRun > 0:
		return 2, nil
	case failed > 0:
		return 1, nil
	}
	return 0, nil
}
