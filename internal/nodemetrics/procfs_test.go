package nodemetrics

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFixture creates root/rel with content, making the directories.
func writeFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseProcStat(t *testing.T) {
	stat := "cpu  100 5 50 1000 20 3 7 400 0 0\n" +
		"cpu0 50 2 25 500 10 1 3 200 0 0\n" +
		"cpu1 50 3 25 500 10 2 4 200 0 0\n" +
		"intr 12345 0 0\nctxt 999\nbtime 1700000000\nprocesses 42\n"
	times, cpus, ok := parseProcStat([]byte(stat))
	if !ok || cpus != 2 {
		t.Fatalf("parse = %+v cpus=%d ok=%v", times, cpus, ok)
	}
	want := cpuTimes{user: 100, nice: 5, system: 50, idle: 1000, iowait: 20, irq: 3, softirq: 7, steal: 400}
	if times != want {
		t.Fatalf("times = %+v, want %+v", times, want)
	}
	if times.busy() != 165 || times.total() != 1585 {
		t.Fatalf("busy=%d total=%d: guest time must not be counted twice", times.busy(), times.total())
	}

	// A kernel that stops before steal still parses, with steal 0.
	old, _, ok := parseProcStat([]byte("cpu  1 2 3 4 5 6 7\ncpu0 1 2 3 4 5 6 7\n"))
	if !ok || old.steal != 0 || old.softirq != 7 {
		t.Fatalf("short cpu line = %+v ok=%v", old, ok)
	}
	for _, bad := range []string{"", "intr 1\n", "cpu  1 2 x 4 5\n", "cpu  1 2\n"} {
		if _, _, ok := parseProcStat([]byte(bad)); ok {
			t.Fatalf("%q should not parse", bad)
		}
	}
}

func TestReadLoadAndMemory(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "proc/loadavg", "3.10 2.95 2.50 4/812 123456\n")
	writeFixture(t, root, "proc/meminfo", "MemTotal:        8131072 kB\nMemFree:          500000 kB\nMemAvailable:    4065536 kB\n")
	load, ok := readLoadAvg(root)
	if !ok || *load != (Load{Load1: 3.10, Load5: 2.95, Load15: 2.50}) {
		t.Fatalf("load = %+v ok=%v", load, ok)
	}
	mem, ok := readMemInfo(root)
	if !ok || mem.TotalBytes != 8131072*1024 || mem.AvailableBytes == nil || *mem.AvailableBytes != 4065536*1024 {
		t.Fatalf("memory = %+v ok=%v", mem, ok)
	}

	// Kernels before 3.14 have no MemAvailable.
	writeFixture(t, root, "proc/meminfo", "MemTotal: 1024 kB\n")
	if mem, ok := readMemInfo(root); !ok || mem.AvailableBytes != nil || mem.TotalBytes != 1024*1024 {
		t.Fatalf("memory without MemAvailable = %+v ok=%v", mem, ok)
	}
}

func TestCgroupPath(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		content string
		want    string
		ok      bool
	}{
		{"0::/system.slice/clirelay2-8319.service\n", "/system.slice/clirelay2-8319.service", true},
		// Hybrid hierarchy: the v2 line is last.
		{"12:cpu,cpuacct:/system.slice/x.service\n0::/system.slice/x.service\n", "/system.slice/x.service", true},
		// Inside a container with its own cgroup namespace.
		{"0::/\n", "/", true},
		// v1 only: nothing to read.
		{"4:memory:/user.slice\n3:cpu,cpuacct:/user.slice\n", "", false},
		// Outside the namespace root.
		{"0::/../../system.slice/x.service\n", "", false},
	}
	for _, tc := range cases {
		writeFixture(t, root, "proc/self/cgroup", tc.content)
		got, ok := cgroupPath(root)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("cgroupPath(%q) = %q, %v; want %q, %v", tc.content, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := cgroupPath(t.TempDir()); ok {
		t.Fatal("a missing /proc/self/cgroup must not yield a path")
	}
}

func TestReadCgroupFiles(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "cpu.stat", "usage_usec 25500000\nuser_usec 20000000\nsystem_usec 5500000\nnr_periods 100\nnr_throttled 12\nthrottled_usec 300000\nnr_bursts 0\nburst_usec 0\n")
	writeFixture(t, dir, "cpu.max", "700000 100000\n")
	writeFixture(t, dir, "memory.current", "1610612736\n")
	writeFixture(t, dir, "memory.high", "max\n")
	writeFixture(t, dir, "memory.max", "4294967296\n")

	stat, ok := readCgroupCPUStat(dir)
	if !ok || stat != (cgroupCPUStat{usageUsec: 25500000, nrPeriods: 100, nrThrottled: 12, throttledUsec: 300000}) {
		t.Fatalf("cpu.stat = %+v ok=%v", stat, ok)
	}
	if quota := readCPUMax(dir); quota == nil || *quota != 7 {
		t.Fatalf("cpu.max = %v, want 7 cores", quota)
	}
	mem, ok := readCgroupMemory(dir)
	if !ok || mem.CurrentBytes != 1610612736 || mem.HighBytes != nil || mem.MaxBytes == nil || *mem.MaxBytes != 4294967296 {
		t.Fatalf("memory = %+v ok=%v", mem, ok)
	}

	writeFixture(t, dir, "cpu.max", "max 100000\n")
	if quota := readCPUMax(dir); quota != nil {
		t.Fatalf("an unlimited cpu.max must have no quota, got %v", *quota)
	}
	// Without the cpu controller only the usage is there.
	writeFixture(t, dir, "cpu.stat", "usage_usec 10\nuser_usec 6\nsystem_usec 4\n")
	if stat, ok := readCgroupCPUStat(dir); !ok || stat != (cgroupCPUStat{usageUsec: 10}) {
		t.Fatalf("cpu.stat without the controller = %+v ok=%v", stat, ok)
	}
}
