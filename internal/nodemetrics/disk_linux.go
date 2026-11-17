//go:build linux

package nodemetrics

import "golang.org/x/sys/unix"

// platformStatPath reads the filesystem holding path: stat for the device
// that names it, statfs for its size.
func platformStatPath(path string) (fsStat, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return fsStat{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return fsStat{}, err
	}
	// f_frsize is the unit of the block counts; f_bsize is only the
	// preferred I/O size, though the two agree on common filesystems.
	blockSize := uint64(fs.Frsize)
	if fs.Frsize <= 0 {
		blockSize = uint64(fs.Bsize)
	}
	dev := uint64(st.Dev)
	return fsStat{
		major:     unix.Major(dev),
		minor:     unix.Minor(dev),
		blockSize: blockSize,
		blocks:    fs.Blocks,
		free:      fs.Bfree,
		avail:     fs.Bavail,
		files:     fs.Files,
		filesFree: fs.Ffree,
		readOnly:  fs.Flags&unix.ST_RDONLY != 0,
	}, nil
}
