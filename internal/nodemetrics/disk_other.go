//go:build !linux

package nodemetrics

// platformStatPath reports nothing outside Linux, where the nodes run; the
// snapshot then has no disks.
func platformStatPath(string) (fsStat, error) {
	return fsStat{}, errDisksUnsupported
}
