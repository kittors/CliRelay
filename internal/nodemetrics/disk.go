package nodemetrics

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Disk use names: what a directory holds, and so what a full filesystem
// would break. The console shows them as labels.
const (
	// DiskUseRoot is the root filesystem, always reported.
	DiskUseRoot = "root"
	// DiskUseWorkdir is the process's working directory: on a node the
	// deploy root with the slot binaries and config.yaml.
	DiskUseWorkdir = "workdir"
	// DiskUseAuth holds the credential files (auth-dir).
	DiskUseAuth = "auth"
	// DiskUseLogs holds the application and request error logs.
	DiskUseLogs = "logs"
	// DiskUseBodyCache holds request bodies too large to keep in memory.
	// It defaults to the temp directory, a small tmpfs on some hosts.
	DiskUseBodyCache = "body_cache"
	// DiskUseUsageSpool holds usage records queued while the database is
	// unreachable.
	DiskUseUsageSpool = "usage_spool"
	// DiskUseExtra is a directory an operator asked to watch.
	DiskUseExtra = "extra"
	// DiskUseDatabase is a database's data directory, as the host agent on
	// the database host reports it.
	DiskUseDatabase = "database"
	// DiskUseBackups holds base backups and archived WAL.
	DiskUseBackups = "backups"
)

const (
	// diskGrowthSpan is how far back the growth of used space looks.
	diskGrowthSpan = time.Hour
	// diskGrowthStep keeps one point a minute in the growth history.
	diskGrowthStep = time.Minute
	// diskGrowthMinWindow is the shortest window the growth is reported for.
	// The console extrapolates it days ahead, so ten minutes after a restart
	// (every deploy) was too little: 10 MB more turned an empty 2 GB tmpfs
	// into "full in 30 hours" on 2026-11-17.
	diskGrowthMinWindow = 30 * time.Minute
	// diskGrowthEdge is how much of each end of the window the low-water
	// marks are taken from; see diskGrowth.
	diskGrowthEdge = 10 * time.Minute
)

// errDisksUnsupported is what statPath returns where filesystems cannot be
// read (outside Linux); the snapshot then has no disks.
var errDisksUnsupported = errors.New("nodemetrics: disk usage is only read on Linux")

// DiskPath is a directory whose filesystem the snapshot reports, and what it
// holds.
type DiskPath struct {
	Use  string `json:"use"`
	Path string `json:"path"`
}

// Disk is one filesystem holding one or more of the reported directories.
// Directories on the same filesystem share one entry.
type Disk struct {
	// Mount is where the filesystem is mounted, e.g. "/" or "/tmp".
	Mount string `json:"mount"`
	// FSType is the filesystem type from /proc/self/mountinfo, e.g. "xfs";
	// "tmpfs" lives in memory. Left out when the mount is not listed.
	FSType string `json:"fs_type,omitempty"`
	// ReadOnly means the filesystem itself is read-only, as when the kernel
	// remounted it after errors (errors=remount-ro). It is read from the
	// super options in mountinfo, not from statfs: statfs reports the mount
	// the process sees, and a sandboxed service (ProtectSystem=strict, as
	// clirelay-hostagent runs) sees / as a read-only bind mount of a
	// filesystem that is still written. Without the mount in mountinfo it
	// falls back to statfs.
	ReadOnly bool       `json:"read_only,omitempty"`
	Paths    []DiskPath `json:"paths"`

	TotalBytes uint64 `json:"total_bytes"`
	// UsedBytes is total minus free space, as df counts it.
	UsedBytes uint64 `json:"used_bytes"`
	// AvailableBytes is what an unprivileged process can still write: free
	// space minus the blocks reserved for root, df's "Avail". df's Use% is
	// used / (used + available), which reaches 100% while root could still
	// write.
	AvailableBytes uint64 `json:"available_bytes"`
	// InodesTotal and InodesFree are left out for filesystems without a
	// fixed inode table, which report 0 (btrfs).
	InodesTotal *uint64 `json:"inodes_total,omitempty"`
	InodesFree  *uint64 `json:"inodes_free,omitempty"`
	// Growth is left out until the process has watched the filesystem for
	// ten minutes.
	Growth *DiskGrowth `json:"growth,omitempty"`
}

// DiskGrowth is how the used space of a filesystem changed over about the
// last hour (since the process started, while it is younger), measured
// between the least space used near the start of that window and the least
// used near its end, so that files which come and go do not count. The
// window is the time between those two points. The console divides the
// available space by it to estimate when the filesystem fills.
type DiskGrowth struct {
	WindowSeconds float64 `json:"window_seconds"`
	// UsedDeltaBytes is negative when space was freed.
	UsedDeltaBytes int64 `json:"used_delta_bytes"`
}

// fsStat is what stat and statfs report for one path.
type fsStat struct {
	// major and minor identify the filesystem (st_dev); they match the
	// device column of /proc/self/mountinfo.
	major, minor uint32
	blockSize    uint64
	blocks       uint64
	free         uint64
	avail        uint64
	files        uint64
	filesFree    uint64
	readOnly     bool
}

// mountEntry is one line of /proc/self/mountinfo.
type mountEntry struct {
	major, minor uint32
	point        string
	fsType       string
	// readOnly is set when the super options, the filesystem's own state
	// shared by every mount of it, include "ro". See Disk.ReadOnly.
	readOnly bool
}

type diskSample struct {
	at   time.Time
	used uint64
}

// sampleDisks reports the filesystems holding the root and paths, one entry
// per filesystem. A path that does not exist yet is reported on the
// filesystem of its nearest existing parent, where it would be created.
func (c *Collector) sampleDisks(now time.Time, paths []DiskPath) []Disk {
	paths = append([]DiskPath{{Use: DiskUseRoot, Path: "/"}}, paths...)
	mounts, _ := readMountInfo(c.root)

	type found struct {
		disk *Disk
		stat fsStat
	}
	byDevice := map[[2]uint32]*found{}
	for _, p := range paths {
		if strings.TrimSpace(p.Path) == "" {
			continue
		}
		resolved := resolveExisting(p.Path)
		stat, err := c.statPath(resolved)
		if err != nil {
			// Unreadable, or not Linux: leave this directory out.
			continue
		}
		key := [2]uint32{stat.major, stat.minor}
		entry, ok := byDevice[key]
		if !ok {
			mount := mountFor(mounts, resolved, stat.major, stat.minor)
			disk := diskFromStat(stat)
			disk.Mount, disk.FSType = mount.point, mount.fsType
			if mount.point != "" && mount.major == stat.major && mount.minor == stat.minor {
				// The filesystem's state, not that of the view this process
				// has of it; see Disk.ReadOnly.
				disk.ReadOnly = mount.readOnly
			}
			if disk.Mount == "" {
				disk.Mount = resolved
			}
			entry = &found{disk: disk, stat: stat}
			byDevice[key] = entry
		}
		entry.disk.Paths = appendDiskPath(entry.disk.Paths, DiskPath{Use: p.Use, Path: filepath.Clean(p.Path)})
	}
	if len(byDevice) == 0 {
		c.diskHistory = nil
		return nil
	}

	disks := make([]Disk, 0, len(byDevice))
	seen := make(map[string]bool, len(byDevice))
	for _, entry := range byDevice {
		key := entry.disk.Mount + "\x00" + strconv.FormatUint(uint64(entry.stat.major), 10) + ":" + strconv.FormatUint(uint64(entry.stat.minor), 10)
		seen[key] = true
		entry.disk.Growth = c.diskGrowth(key, now, entry.disk.UsedBytes)
		disks = append(disks, *entry.disk)
	}
	for key := range c.diskHistory {
		if !seen[key] {
			delete(c.diskHistory, key)
		}
	}
	// The root filesystem first, the rest by mount point: a stable order for
	// the console and for tests.
	sort.SliceStable(disks, func(i, j int) bool {
		ri, rj := hasDiskUse(disks[i], DiskUseRoot), hasDiskUse(disks[j], DiskUseRoot)
		if ri != rj {
			return ri
		}
		return disks[i].Mount < disks[j].Mount
	})
	return disks
}

// diskGrowth records used space once a minute for the filesystem under key
// and returns its change over the window, nil while the window is shorter
// than diskGrowthMinWindow.
//
// The change is between low-water marks: the least used in the first
// diskGrowthEdge of the window and the least used in the last one (the
// current sample included). A file that is there for a few minutes and gone
// again, such as a large request body in the tmpfs body cache, lifts single
// samples but not those minimums, so only space that stays counts as growth.
// Comparing the newest sample with the oldest read a passing 10 MB body on an
// empty 2 GB tmpfs as a trend.
func (c *Collector) diskGrowth(key string, now time.Time, used uint64) *DiskGrowth {
	if c.diskHistory == nil {
		c.diskHistory = map[string][]diskSample{}
	}
	history := c.diskHistory[key]
	if n := len(history); n == 0 || now.Sub(history[n-1].at) >= diskGrowthStep {
		history = append(history, diskSample{at: now, used: used})
	}
	cut := now.Add(-diskGrowthSpan)
	for len(history) > 1 && !history[1].at.After(cut) {
		history = history[1:]
	}
	c.diskHistory[key] = history
	start := history[0]
	elapsed := now.Sub(start.at)
	if elapsed < diskGrowthMinWindow {
		return nil
	}
	// Ties keep the earliest point at the start and the current sample at the
	// end, so a flat series spans the whole window.
	early := start
	for _, s := range history {
		if s.at.After(start.at.Add(diskGrowthEdge)) {
			break
		}
		if s.used < early.used {
			early = s
		}
	}
	late := diskSample{at: now, used: used}
	for _, s := range history {
		if !s.at.Before(now.Add(-diskGrowthEdge)) && s.used < late.used {
			late = s
		}
	}
	span := late.at.Sub(early.at)
	if span <= 0 {
		return nil
	}
	return &DiskGrowth{
		WindowSeconds:  round3(span.Seconds()),
		UsedDeltaBytes: int64(late.used) - int64(early.used),
	}
}

func diskFromStat(stat fsStat) *Disk {
	disk := &Disk{
		ReadOnly:       stat.readOnly,
		TotalBytes:     stat.blocks * stat.blockSize,
		AvailableBytes: stat.avail * stat.blockSize,
	}
	if stat.blocks >= stat.free {
		disk.UsedBytes = (stat.blocks - stat.free) * stat.blockSize
	}
	if stat.files > 0 {
		total, free := stat.files, min(stat.filesFree, stat.files)
		disk.InodesTotal, disk.InodesFree = &total, &free
	}
	return disk
}

func hasDiskUse(disk Disk, use string) bool {
	for _, p := range disk.Paths {
		if p.Use == use {
			return true
		}
	}
	return false
}

// appendDiskPath adds p unless the same use and path are already listed:
// the working directory and the auth directory are often configured to the
// same place.
func appendDiskPath(paths []DiskPath, p DiskPath) []DiskPath {
	for _, existing := range paths {
		if existing == p {
			return paths
		}
	}
	return append(paths, p)
}

// resolveExisting makes path absolute, follows symlinks, and climbs to the
// nearest directory that exists: a spool or cache directory may not have
// been created yet, and its filesystem is still the one it will land on. It
// climbs only past directories that do not exist. One it may not enter
// (permission denied) is returned as is, so statfs fails and the directory
// is left out rather than silently reported on its parent's filesystem.
func resolveExisting(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	for current := abs; ; {
		_, err := os.Stat(current)
		if err == nil {
			if real, err := filepath.EvalSymlinks(current); err == nil {
				return real
			}
			return current
		}
		if !errors.Is(err, os.ErrNotExist) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs
		}
		current = parent
	}
}

// mountFor picks the mount of the filesystem major:minor that holds path:
// the longest mount point the path lies under, the later one when the same
// point is mounted twice (the later mount hides the earlier). A filesystem
// mounted at several points (a bind mount) matches each, and the longest
// one under the path wins. Without a match on the device it falls back to
// the path alone, as in a container whose mountinfo is partial.
func mountFor(mounts []mountEntry, path string, major, minor uint32) mountEntry {
	best, bestLen := -1, -1
	fallback, fallbackLen := -1, -1
	for i, m := range mounts {
		if !pathWithin(path, m.point) {
			continue
		}
		if len(m.point) >= fallbackLen {
			fallback, fallbackLen = i, len(m.point)
		}
		if m.major == major && m.minor == minor && len(m.point) >= bestLen {
			best, bestLen = i, len(m.point)
		}
	}
	switch {
	case best >= 0:
		return mounts[best]
	case fallback >= 0:
		return mounts[fallback]
	default:
		return mountEntry{}
	}
}

// pathWithin reports whether path is dir or lies under it.
func pathWithin(path, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}

func readMountInfo(root string) ([]mountEntry, bool) {
	data, err := os.ReadFile(filepath.Join(root, "proc", "self", "mountinfo"))
	if err != nil {
		return nil, false
	}
	mounts := parseMountInfo(data)
	return mounts, len(mounts) > 0
}

// parseMountInfo reads /proc/self/mountinfo lines such as
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//
// keeping the device, the mount point (field 5), the filesystem type (the
// first field after the "-" separator) and whether the super options (the
// third field after it) include "ro". Lines it cannot read are skipped.
func parseMountInfo(data []byte) []mountEntry {
	var mounts []mountEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 7 {
			continue
		}
		majorText, minorText, ok := strings.Cut(fields[2], ":")
		if !ok {
			continue
		}
		major, errMajor := strconv.ParseUint(majorText, 10, 32)
		minor, errMinor := strconv.ParseUint(minorText, 10, 32)
		if errMajor != nil || errMinor != nil {
			continue
		}
		fsType, readOnly := "", false
		for i := 6; i < len(fields)-1; i++ {
			if fields[i] == "-" {
				fsType = fields[i+1]
				if i+3 < len(fields) {
					readOnly = hasMountOption(fields[i+3], "ro")
				}
				break
			}
		}
		mounts = append(mounts, mountEntry{
			major:    uint32(major),
			minor:    uint32(minor),
			point:    unescapeMountField(fields[4]),
			fsType:   fsType,
			readOnly: readOnly,
		})
	}
	return mounts
}

// hasMountOption reports whether the comma-separated options include name
// on its own ("errors=remount-ro" is not "ro").
func hasMountOption(options, name string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == name {
			return true
		}
	}
	return false
}

// unescapeMountField undoes the octal escapes mountinfo uses for space, tab,
// newline and backslash (\040, \011, \012, \134).
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
