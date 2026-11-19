package nodemetrics

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The readers below take the filesystem root so tests can point them at a
// directory of fixture files; production passes "/". Each returns ok=false
// when its file is missing or does not parse, and the caller leaves the
// matching fields out.

// cpuTimes is the aggregate "cpu" line of /proc/stat, in clock ticks.
type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

// busy is time spent running. guest and guest_nice are not added: the kernel
// already counts them in user and nice.
func (t cpuTimes) busy() uint64 { return t.user + t.nice + t.system + t.irq + t.softirq }

func (t cpuTimes) total() uint64 { return t.busy() + t.idle + t.iowait + t.steal }

// readProcStat returns the aggregate CPU times and the number of CPUs listed.
func readProcStat(root string) (times cpuTimes, cpus int, ok bool) {
	data, err := os.ReadFile(filepath.Join(root, "proc", "stat"))
	if err != nil {
		return cpuTimes{}, 0, false
	}
	return parseProcStat(data)
}

func parseProcStat(data []byte) (times cpuTimes, cpus int, ok bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			if _, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu")); err == nil {
				cpus++
			}
			continue
		}
		// user nice system idle iowait irq softirq steal: older kernels stop
		// early (before steal until 2.6.11), and the missing ones stay 0.
		if len(fields) < 5 {
			return cpuTimes{}, 0, false
		}
		values := make([]uint64, 8)
		for i := range values {
			if i+1 >= len(fields) {
				break
			}
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return cpuTimes{}, 0, false
			}
			values[i] = v
		}
		times = cpuTimes{user: values[0], nice: values[1], system: values[2], idle: values[3],
			iowait: values[4], irq: values[5], softirq: values[6], steal: values[7]}
		ok = true
	}
	return times, cpus, ok
}

func readLoadAvg(root string) (*Load, bool) {
	data, err := os.ReadFile(filepath.Join(root, "proc", "loadavg"))
	if err != nil {
		return nil, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return nil, false
	}
	var values [3]float64
	for i := range values {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return nil, false
		}
		values[i] = v
	}
	return &Load{Load1: values[0], Load5: values[1], Load15: values[2]}, true
}

func readMemInfo(root string) (*Memory, bool) {
	data, err := os.ReadFile(filepath.Join(root, "proc", "meminfo"))
	if err != nil {
		return nil, false
	}
	var mem Memory
	var haveTotal bool
	for line := range strings.SplitSeq(string(data), "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found || (key != "MemTotal" && key != "MemAvailable") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		n := kib * 1024
		if key == "MemTotal" {
			mem.TotalBytes, haveTotal = n, true
		} else {
			mem.AvailableBytes = &n
		}
	}
	if !haveTotal {
		return nil, false
	}
	return &mem, true
}

// cgroupPath returns the process's cgroup v2 path from /proc/self/cgroup, the
// "0::<path>" line. Hosts still on the v1 hierarchy have none and get no
// cgroup fields. A path that climbs out of the cgroup namespace ("..") is
// ignored too: it cannot be found under this mount.
func cgroupPath(root string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(root, "proc", "self", "cgroup"))
	if err != nil {
		return "", false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		path, found := strings.CutPrefix(strings.TrimSpace(line), "0::")
		if !found {
			continue
		}
		if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
			return "", false
		}
		return path, true
	}
	return "", false
}

func cgroupDir(root, path string) string {
	return filepath.Join(root, "sys", "fs", "cgroup", filepath.FromSlash(path))
}

// cgroupCPUStat is the part of cpu.stat the snapshot reports.
type cgroupCPUStat struct {
	usageUsec, nrPeriods, nrThrottled, throttledUsec uint64
}

// readCgroupCPUStat reads cpu.stat. Without the cpu controller enabled for
// the cgroup only usage_usec is there, and the throttling counters stay 0.
func readCgroupCPUStat(dir string) (cgroupCPUStat, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return cgroupCPUStat{}, false
	}
	var stat cgroupCPUStat
	var haveUsage bool
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "usage_usec":
			stat.usageUsec, haveUsage = v, true
		case "nr_periods":
			stat.nrPeriods = v
		case "nr_throttled":
			stat.nrThrottled = v
		case "throttled_usec":
			stat.throttledUsec = v
		}
	}
	return stat, haveUsage
}

// readCPUMax reads cpu.max ("$MAX $PERIOD") as cores, nil without a quota or
// without the file.
func readCPUMax(dir string) *float64 {
	data, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] == "max" {
		return nil
	}
	quota, errQuota := strconv.ParseFloat(fields[0], 64)
	period, errPeriod := strconv.ParseFloat(fields[1], 64)
	if errQuota != nil || errPeriod != nil || quota <= 0 || period <= 0 {
		return nil
	}
	cores := quota / period
	return &cores
}

// readCgroupMemory reads memory.current, memory.high and memory.max; it needs
// memory.current, and leaves a limit out when it is "max" or unreadable.
func readCgroupMemory(dir string) (*CgroupMemory, bool) {
	current, ok := readCgroupUint(filepath.Join(dir, "memory.current"))
	if !ok {
		return nil, false
	}
	mem := &CgroupMemory{CurrentBytes: current}
	if high, ok := readCgroupUint(filepath.Join(dir, "memory.high")); ok {
		mem.HighBytes = &high
	}
	if limit, ok := readCgroupUint(filepath.Join(dir, "memory.max")); ok {
		mem.MaxBytes = &limit
	}
	return mem, true
}

// readCgroupUint reads a single-number cgroup file; "max" counts as absent.
func readCgroupUint(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
