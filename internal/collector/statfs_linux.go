//go:build linux

package collector

import "syscall"

func statfs(path string) (fsStats, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return fsStats{}, err
	}
	// Block counts are in fragment-size units; Bsize is only the preferred I/O size.
	bsize := uint64(s.Frsize)
	if bsize == 0 {
		bsize = uint64(s.Bsize)
	}
	return fsStats{
		blocks: s.Blocks, bfree: s.Bfree, bavail: s.Bavail,
		files: s.Files, ffree: s.Ffree,
		bsize: bsize,
	}, nil
}
