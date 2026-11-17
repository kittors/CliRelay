package nodemetrics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mountInfoFixture is a node like n143 on 2026-11-17: the root on ext4,
// /tmp a tmpfs of about 2 GB, and a path with a space to exercise the octal
// escapes.
const mountInfoFixture = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,discard,errors=remount-ro
24 22 0:5 / /dev rw,nosuid,relatime shared:2 - devtmpfs udev rw,size=1977660k
31 22 0:27 / /tmp rw,nosuid,nodev shared:13 - tmpfs tmpfs rw,nr_inodes=1048576,inode64
40 22 8:15 / /boot/efi rw,relatime shared:20 - vfat /dev/sda15 rw,fmask=0077
41 22 8:17 / /mnt/data\040disk rw,relatime shared:21 - xfs /dev/sdb1 rw
`

func TestParseMountInfo(t *testing.T) {
	mounts := parseMountInfo([]byte(mountInfoFixture + "garbage line\n12 1 x:y / /bad rw - ext4 /dev/x rw\n"))
	if len(mounts) != 5 {
		t.Fatalf("parsed %d mounts, want 5: %+v", len(mounts), mounts)
	}
	tmp := mounts[2]
	if tmp.point != "/tmp" || tmp.fsType != "tmpfs" || tmp.major != 0 || tmp.minor != 27 {
		t.Fatalf("tmp mount = %+v", tmp)
	}
	if got := mounts[4].point; got != "/mnt/data disk" {
		t.Fatalf("escaped mount point = %q, want %q", got, "/mnt/data disk")
	}
	for _, m := range mounts {
		if m.readOnly {
			t.Fatalf("%s read-only, but every filesystem in the fixture is mounted rw", m.point)
		}
	}
}

// Whether a filesystem is read-only comes from its super options, not from
// the options of the mount. The first line is / as clirelay-hostagent sees it
// on the database host under ProtectSystem=strict (2026-11-17): a read-only
// bind mount of a filesystem still mounted read-write.
func TestParseMountInfoReadsReadOnlyFromTheSuperOptions(t *testing.T) {
	mounts := parseMountInfo([]byte("2559 2465 252:1 / / ro,nosuid,relatime shared:786 master:1 - ext4 /dev/vda1 rw,discard,errors=remount-ro\n" +
		"60 22 8:33 / /srv rw,relatime shared:30 - ext4 /dev/sdc1 ro,errors=remount-ro\n" +
		"61 22 11:0 / /media/cd ro,relatime - iso9660 /dev/sr0 ro,relatime\n"))
	if len(mounts) != 3 {
		t.Fatalf("parsed %d mounts, want 3: %+v", len(mounts), mounts)
	}
	if mounts[0].readOnly {
		t.Fatalf("a read-only view of a read-write filesystem counted as read-only: %+v", mounts[0])
	}
	if !mounts[1].readOnly || !mounts[2].readOnly {
		t.Fatalf("filesystems whose super options say ro: %+v, %+v; want both read-only", mounts[1], mounts[2])
	}
}

func TestMountForPicksTheLongestMountOfTheSameDevice(t *testing.T) {
	mounts := parseMountInfo([]byte(mountInfoFixture))
	if got := mountFor(mounts, "/tmp/clirelay-body", 0, 27); got.point != "/tmp" || got.fsType != "tmpfs" {
		t.Fatalf("mount of /tmp/clirelay-body = %+v", got)
	}
	if got := mountFor(mounts, "/opt/clirelay2/logs", 8, 1); got.point != "/" {
		t.Fatalf("mount of /opt/clirelay2/logs = %+v", got)
	}
	// /tmpx is not under /tmp.
	if got := mountFor(mounts, "/tmpx", 8, 1); got.point != "/" {
		t.Fatalf("mount of /tmpx = %+v", got)
	}
	// A device missing from mountinfo (a partial view in a container) falls
	// back to the longest mount point the path lies under.
	if got := mountFor(mounts, "/tmp/x", 9, 9); got.point != "/tmp" {
		t.Fatalf("fallback mount = %+v", got)
	}
	// The later of two mounts at the same point hides the earlier.
	stacked := append(parseMountInfo([]byte(mountInfoFixture)), mountEntry{major: 0, minor: 27, point: "/tmp", fsType: "overlay"})
	if got := mountFor(stacked, "/tmp/x", 0, 27); got.fsType != "overlay" {
		t.Fatalf("stacked mount = %+v", got)
	}
}

// fakeFS answers statPath for directories created under a test root: each
// directory maps to a filesystem by the longest matching prefix.
type fakeFS struct {
	byPrefix map[string]fsStat
	calls    []string
}

func (f *fakeFS) stat(path string) (fsStat, error) {
	f.calls = append(f.calls, path)
	best := ""
	for prefix := range f.byPrefix {
		if pathWithin(path, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return fsStat{}, errors.New("no such filesystem")
	}
	return f.byPrefix[best], nil
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// diskCollector returns a collector whose filesystems are fake: the root
// directory "/" and everything outside base on "root", base/tmp on a tmpfs.
func diskCollector(t *testing.T) (*Collector, *testClock, string, *fakeFS) {
	t.Helper()
	root := t.TempDir()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mkdirs(t, filepath.Join(base, "opt", "logs"), filepath.Join(base, "tmp"))
	// The root on ext4 (8:1) and base/tmp a tmpfs (0:27), as /tmp is on n143.
	writeFixture(t, root, "proc/self/mountinfo", "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"+
		"31 22 0:27 / "+filepath.Join(base, "tmp")+" rw,nosuid,nodev shared:13 - tmpfs tmpfs rw,inode64\n")
	collector, clock := newTestCollector(t, root)
	fs := &fakeFS{byPrefix: map[string]fsStat{
		// 30 GiB in 4 KiB blocks: 18 GiB free, 16 GiB of it for users.
		"/": {major: 8, minor: 1, blockSize: 4096, blocks: 30 << 18, free: 18 << 18, avail: 16 << 18, files: 2_000_000, filesFree: 1_900_000},
		// 2 GiB tmpfs: 1.5 GiB free.
		filepath.Join(base, "tmp"): {major: 0, minor: 27, blockSize: 4096, blocks: 2 << 18, free: 3 << 17, avail: 3 << 17, files: 1 << 20, filesFree: 1<<20 - 40},
	}}
	collector.statPath = fs.stat
	return collector, clock, base, fs
}

func TestCollectorReportsOneEntryPerFilesystem(t *testing.T) {
	collector, _, base, _ := diskCollector(t)
	logs := filepath.Join(base, "opt", "logs")
	collector.SetSources(Sources{DiskPaths: func() []DiskPath {
		return []DiskPath{
			{Use: DiskUseWorkdir, Path: filepath.Join(base, "opt")},
			{Use: DiskUseLogs, Path: logs},
			{Use: DiskUseLogs, Path: logs},                                             // listed twice: reported once
			{Use: DiskUseBodyCache, Path: filepath.Join(base, "tmp", "clirelay-body")}, // not created yet
			{Use: DiskUseExtra, Path: "  "},                                            // blank: ignored
		}
	}})
	collector.sample(context.Background())
	snap, _ := collector.Latest()
	if len(snap.Disks) != 2 {
		t.Fatalf("disks = %+v, want the root filesystem and the tmpfs", snap.Disks)
	}
	rootDisk, tmp := snap.Disks[0], snap.Disks[1]
	if !hasDiskUse(rootDisk, DiskUseRoot) {
		t.Fatalf("first disk is %+v, want the root filesystem first", rootDisk)
	}
	if rootDisk.TotalBytes != 30<<30 || rootDisk.UsedBytes != 12<<30 || rootDisk.AvailableBytes != 16<<30 {
		t.Fatalf("root sizes = total %d used %d avail %d", rootDisk.TotalBytes, rootDisk.UsedBytes, rootDisk.AvailableBytes)
	}
	if rootDisk.InodesTotal == nil || *rootDisk.InodesTotal != 2_000_000 || *rootDisk.InodesFree != 1_900_000 {
		t.Fatalf("root inodes = %v / %v", rootDisk.InodesTotal, rootDisk.InodesFree)
	}
	if got := len(rootDisk.Paths); got != 3 {
		t.Fatalf("root paths = %+v, want root, workdir and logs once", rootDisk.Paths)
	}
	if tmp.FSType != "tmpfs" || tmp.Mount != filepath.Join(base, "tmp") {
		t.Fatalf("tmp disk = %+v", tmp)
	}
	if rootDisk.FSType != "ext4" || rootDisk.Mount != "/" {
		t.Fatalf("root disk = %+v", rootDisk)
	}
	if len(tmp.Paths) != 1 || tmp.Paths[0].Use != DiskUseBodyCache || !strings.HasSuffix(tmp.Paths[0].Path, "clirelay-body") {
		t.Fatalf("tmp paths = %+v: a directory not created yet is reported where it would be created", tmp.Paths)
	}
	// The JSON the console reads.
	raw, _ := json.Marshal(tmp)
	for _, field := range []string{`"fs_type":"tmpfs"`, `"use":"body_cache"`, `"inodes_total":1048576`, `"available_bytes":1610612736`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("disk JSON %s lacks %s", raw, field)
		}
	}
}

// statfs inside a sandbox reports the read-only view; the disk is read-only
// only once the filesystem itself is.
func TestCollectorTakesReadOnlyFromTheFilesystemNotTheView(t *testing.T) {
	collector, _, _, fs := diskCollector(t)
	view := fs.byPrefix["/"]
	view.readOnly = true // what statfs says under ProtectSystem=strict
	fs.byPrefix["/"] = view
	collector.sample(context.Background())
	snap, _ := collector.Latest()
	if len(snap.Disks) == 0 || snap.Disks[0].Mount != "/" || snap.Disks[0].ReadOnly {
		t.Fatalf("disks = %+v, want / reported writable: only the view of it is read-only", snap.Disks)
	}

	// The kernel remounted it read-only after errors.
	writeFixture(t, collector.root, "proc/self/mountinfo", "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 ro,errors=remount-ro\n")
	collector.sample(context.Background())
	snap, _ = collector.Latest()
	if len(snap.Disks) == 0 || !snap.Disks[0].ReadOnly {
		t.Fatalf("disks = %+v, want / read-only once its super options say ro", snap.Disks)
	}
}

func TestCollectorLeavesDisksOutWhenNoFilesystemCanBeRead(t *testing.T) {
	collector, _, _, _ := diskCollector(t)
	collector.statPath = func(string) (fsStat, error) { return fsStat{}, errDisksUnsupported }
	collector.sample(context.Background())
	snap, _ := collector.Latest()
	if snap.Disks != nil {
		t.Fatalf("disks = %+v, want none", snap.Disks)
	}
	raw, _ := json.Marshal(snap)
	if strings.Contains(string(raw), `"disks"`) {
		t.Fatalf("snapshot JSON %s has a disks field", raw)
	}
}

func TestDiskLeavesInodesOutWithoutAnInodeTable(t *testing.T) {
	disk := diskFromStat(fsStat{blockSize: 4096, blocks: 100, free: 40, avail: 30})
	if disk.InodesTotal != nil || disk.InodesFree != nil {
		t.Fatalf("inodes = %v / %v, want none for a filesystem that reports 0", disk.InodesTotal, disk.InodesFree)
	}
	if disk.UsedBytes != 60*4096 || disk.AvailableBytes != 30*4096 {
		t.Fatalf("disk = %+v", disk)
	}
}

// The growth covers the last hour from one point a minute, and only appears
// once ten minutes are watched: before that a log rotation or a large body
// dominates it.
func TestDiskGrowthCoversTheLastHour(t *testing.T) {
	collector, clock, _, fs := diskCollector(t)
	rootStat := fs.byPrefix["/"]
	setFree := func(freeBlocks uint64) {
		s := rootStat
		s.free, s.avail = freeBlocks, freeBlocks
		fs.byPrefix["/"] = s
	}
	sampleRoot := func() Disk {
		collector.sample(context.Background())
		snap, _ := collector.Latest()
		return snap.Disks[0]
	}

	setFree(18 << 18)
	if disk := sampleRoot(); disk.Growth != nil {
		t.Fatalf("growth at start = %+v", disk.Growth)
	}
	// Nine minutes later, 1 GiB more in use, and it stays: no growth is
	// reported before half an hour of history.
	clock.Advance(9 * time.Minute)
	setFree(17 << 18)
	if disk := sampleRoot(); disk.Growth != nil {
		t.Fatalf("growth after 9 minutes = %+v", disk.Growth)
	}
	clock.Advance(20 * time.Minute)
	if disk := sampleRoot(); disk.Growth != nil {
		t.Fatalf("growth after 29 minutes = %+v, want none before 30", disk.Growth)
	}
	clock.Advance(time.Minute)
	if disk := sampleRoot(); disk.Growth == nil || disk.Growth.WindowSeconds != 1800 || disk.Growth.UsedDeltaBytes != 1<<30 {
		t.Fatalf("growth after 30 minutes = %+v, want +1 GiB over 1800 s", disk.Growth)
	}
	// Samples between the minute points do not pile up in the history.
	for range 11 {
		clock.Advance(5 * time.Second)
		sampleRoot()
	}
	history := collector.diskHistory
	for key, points := range history {
		if len(points) > 4 {
			t.Fatalf("history %q holds %d points after 11 minutes, want one a minute", key, len(points))
		}
	}
	// Two hours on, 2 GiB freed since the hour mark: the window has slid
	// past the start.
	for range 24 {
		clock.Advance(5 * time.Minute)
		sampleRoot()
	}
	setFree(19 << 18)
	disk := sampleRoot()
	if disk.Growth == nil || disk.Growth.WindowSeconds > 3600 || disk.Growth.WindowSeconds < 3300 || disk.Growth.UsedDeltaBytes != -2<<30 {
		t.Fatalf("growth after two hours = %+v, want -2 GiB over about an hour", disk.Growth)
	}
}

// A file that comes and goes, like a large request body in the tmpfs body
// cache, lifts single samples but not the low-water marks, so it is not
// growth; space that stays is.
func TestDiskGrowthIgnoresSpaceThatComesAndGoes(t *testing.T) {
	collector, clock, _, fs := diskCollector(t)
	rootStat := fs.byPrefix["/"]
	setFree := func(freeBlocks uint64) {
		s := rootStat
		s.free, s.avail = freeBlocks, freeBlocks
		fs.byPrefix["/"] = s
	}
	sampleRoot := func() Disk {
		collector.sample(context.Background())
		snap, _ := collector.Latest()
		return snap.Disks[0]
	}

	// 40 quiet minutes at 12 GiB used, one sample a minute (t = 0 … 39).
	setFree(18 << 18)
	for range 40 {
		sampleRoot()
		clock.Advance(time.Minute)
	}
	// 2 GiB of files for a single sample (t = 40): no growth.
	setFree(16 << 18)
	if g := sampleRoot().Growth; g == nil || g.UsedDeltaBytes != 0 {
		t.Fatalf("growth with a passing 2 GiB file = %+v, want 0", g)
	}
	setFree(18 << 18)
	clock.Advance(time.Minute)
	sampleRoot()

	// Then 256 MiB more every minute for a quarter of an hour (t = 42 … 56).
	free := uint64(18 << 18)
	for range 15 {
		clock.Advance(time.Minute)
		free -= 1 << 16
		setFree(free)
		sampleRoot()
	}
	// The low-water marks are t = 0 (12 GiB) and t = 46, the oldest point of
	// the last ten minutes, after five steps: +1.25 GiB over 46 minutes.
	g := sampleRoot().Growth
	if g == nil || g.WindowSeconds != 46*60 || g.UsedDeltaBytes != 5<<28 {
		t.Fatalf("growth = %+v, want +1.25 GiB over 2760 s", g)
	}
}

func TestResolveExistingClimbsToTheNearestExistingParent(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	if got := resolveExisting(filepath.Join(link, "not", "yet")); got != base {
		t.Fatalf("resolveExisting = %q, want the symlink target %q", got, base)
	}
}

// A directory it may not enter is not reported on its parent's filesystem:
// that parent can be another disk altogether.
func TestResolveExistingStopsAtADirectoryItMayNotEnter(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root enters every directory")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(base, "locked")
	mkdirs(t, filepath.Join(locked, "data"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	target := filepath.Join(locked, "data")
	if got := resolveExisting(target); got != target {
		t.Fatalf("resolveExisting = %q, want %q itself", got, target)
	}
}
