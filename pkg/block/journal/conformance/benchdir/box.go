package benchdir

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Box is what each benchmark result is recorded with: the commit, the
// memory, the device holding the directory and its filesystem with the mount
// options, the kernel, and the device's own fsync latency. A field the
// platform cannot read cheaply is empty.
type Box struct {
	Commit, Memory, Device, Filesystem, MountOptions, Kernel, DeviceRaw string
}

// Describe reads the box for dir, a directory of the device under test. It
// writes and removes a scratch file in dir for the device's own figure.
func Describe(dir string) (Box, error) {
	raw, err := FsyncProbe(dir)
	if err != nil {
		return Box{}, err
	}

	b := platformBox(dir)
	b.Commit = gitCommit()
	b.DeviceRaw = raw
	return b, nil
}

// Print writes the box as key: value lines, which benchstat reads as the
// configuration of the results below them.
func (b Box) Print(w io.Writer) {
	_, _ = fmt.Fprintf(w, "commit: %s\n", b.Commit)
	_, _ = fmt.Fprintf(w, "memory: %s\n", b.Memory)
	_, _ = fmt.Fprintf(w, "device: %s\n", b.Device)
	_, _ = fmt.Fprintf(w, "filesystem: %s\n", b.Filesystem)
	_, _ = fmt.Fprintf(w, "mount-options: %s\n", b.MountOptions)
	_, _ = fmt.Fprintf(w, "kernel: %s\n", b.Kernel)
	_, _ = fmt.Fprintf(w, "device-raw: %s\n", b.DeviceRaw)
}

// FsyncProbe is the device's own figure for the run: the p50 and p99 of 64
// writes of 4 KiB each followed by an fsync, on a file in dir.
func FsyncProbe(dir string) (string, error) {
	f, err := os.CreateTemp(dir, "fsync-probe-")
	if err != nil {
		return "", fmt.Errorf("fsync probe: %w", err)
	}

	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()

	buf := make([]byte, 4096)

	var took []time.Duration

	for range 64 {
		start := time.Now()

		if _, err := f.Write(buf); err != nil {
			return "", fmt.Errorf("fsync probe write: %w", err)
		}

		if err := f.Sync(); err != nil {
			return "", fmt.Errorf("fsync probe sync: %w", err)
		}

		took = append(took, time.Since(start))
	}

	return fmt.Sprintf("write-4KiB-fsync p50=%v p99=%v", percentile(took, 50), percentile(took, 99)), nil
}

// gitCommit is the checkout's HEAD, with "-dirty" after it when the tree has
// changes, or "unknown" outside a checkout.
func gitCommit() string {
	out, err := exec.Command("git", "describe", "--always", "--dirty").Output()
	if err != nil {
		return "unknown"
	}

	return strings.TrimSpace(string(out))
}

// percentile is the p-th percentile of took, nearest rank; zero when empty.
func percentile(took []time.Duration, p int) time.Duration {
	if len(took) == 0 {
		return 0
	}

	s := slices.Clone(took)
	slices.Sort(s)
	rank := (p*len(s) + 99) / 100

	return s[max(rank, 1)-1]
}
