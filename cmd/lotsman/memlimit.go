package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// memoryLimitPercent is the share of the effective memory the Go runtime is
// asked to stay under. amneziawg-go draws ~64 KiB message buffers from
// unbounded pools, so without a limit the heap follows in-flight traffic and
// stays high. Around 40% keeps throughput and about halves the peak; below
// ~150 MiB the GC starts to cost a lot of it (docs/OPERATIONS.md).
const memoryLimitPercent = 40

// memoryLimit returns the soft memory limit for the Go runtime, or false when
// none should be set. env is $GOMEMLIMIT: a set one always wins, the runtime
// has applied it already. meminfo is /proc/meminfo; memoryMax is the content
// of the process's cgroup v2 memory.max, empty when there is none. The limit
// is memoryLimitPercent of MemTotal, or of memory.max when that is a number
// below it. Unreadable or garbage inputs mean no limit.
func memoryLimit(env, meminfo, memoryMax string) (int64, bool) {
	if env != "" {
		return 0, false
	}
	total, ok := memTotal(meminfo)
	if !ok {
		return 0, false
	}
	// "max" means unlimited and fails the parse like any garbage.
	if n, err := strconv.ParseInt(strings.TrimSpace(memoryMax), 10, 64); err == nil && n > 0 && n < total {
		total = n
	}
	return total / 100 * memoryLimitPercent, true
}

// memTotal returns the MemTotal line of /proc/meminfo in bytes.
func memTotal(meminfo string) (int64, bool) {
	sc := bufio.NewScanner(strings.NewReader(meminfo))
	for sc.Scan() {
		rest, found := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || kb <= 0 || kb > 1<<40 {
			return 0, false
		}
		return kb << 10, true
	}
	return 0, false
}

// cgroupMaxPath returns the path of the process's memory.max below
// cgroupRoot, given the content of /proc/self/cgroup. Only the cgroup v2
// entry (`0::<path>`) counts.
func cgroupMaxPath(cgroupRoot, selfCgroup string) (string, bool) {
	for _, line := range strings.Split(selfCgroup, "\n") {
		if path, found := strings.CutPrefix(line, "0::"); found {
			return filepath.Join(cgroupRoot, path, "memory.max"), true
		}
	}
	return "", false
}

// readMemoryLimit gathers the inputs of memoryLimit from procRoot (/proc) and
// cgroupRoot (/sys/fs/cgroup); tests point them at a fake tree. A cgroup that
// cannot be read only means there is no cap.
func readMemoryLimit(env, procRoot, cgroupRoot string) (int64, bool) {
	if env != "" {
		return 0, false
	}
	meminfo, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, false
	}
	var memoryMax []byte
	if self, err := os.ReadFile(filepath.Join(procRoot, "self", "cgroup")); err == nil {
		if path, ok := cgroupMaxPath(cgroupRoot, string(self)); ok {
			memoryMax, _ = os.ReadFile(path)
		}
	}
	return memoryLimit(env, string(meminfo), string(memoryMax))
}
