package benchdir

import (
	"os"
	"path/filepath"
	"strings"
)

// platformBox reads the box's device, filesystem, mount options, memory and
// kernel from /proc.
func platformBox(dir string) Box {
	b := Box{
		Memory: memTotal(),
		Kernel: "linux " + readTrim("/proc/sys/kernel/osrelease"),
	}
	b.Device, b.Filesystem, b.MountOptions = mountOf(dir)
	return b
}

// mountOf is the source, filesystem type and mount options of the mount
// holding dir, from /proc/self/mountinfo: the longest mount point that
// prefixes it.
func mountOf(dir string) (source, fstype, options string) {
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "unknown", "unknown", "unknown"
	}

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "unknown", "unknown", "unknown"
	}

	best := ""
	source, fstype, options = "unknown", "unknown", "unknown"

	for line := range strings.Lines(string(data)) {
		pre, post, ok := strings.Cut(line, " - ")

		fields, tail := strings.Fields(pre), strings.Fields(post)
		if !ok || len(fields) < 6 || len(tail) < 2 {
			continue
		}

		point := fields[4]

		inside := abs == point || strings.HasPrefix(abs, strings.TrimSuffix(point, "/")+"/")
		if inside && len(point) > len(best) {
			best, source, fstype, options = point, tail[1], tail[0], fields[5]
		}
	}

	return source, fstype, options
}

// memTotal is the box's memory, as /proc/meminfo's MemTotal line gives it.
func memTotal() string {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "unknown"
	}

	for line := range strings.Lines(string(data)) {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			return strings.Join(strings.Fields(v), " ")
		}
	}

	return "unknown"
}

// readTrim is the file's content without surrounding space, or "unknown".
func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unknown"
	}

	return strings.TrimSpace(string(data))
}
