package collector

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/upsignal/agent/internal/telemetry"
)

// diskFSTypes is an allowlist of local, persistent filesystems. An allowlist avoids reporting the long tail of
// pseudo filesystems (tmpfs, overlay, cgroup, …) and skips network filesystems, whose statfs can hang.
var diskFSTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "jfs": true, "reiserfs": true, "bcachefs": true,
	"vfat": true, "exfat": true, "ntfs3": true,
}

type mountEntry struct {
	device     string // major:minor, used to collapse duplicate and bind mounts
	root       string // path within the filesystem that is mounted; "/" unless it is a bind mount of a subdirectory
	mountPoint string
	fsType     string
	source     string
}

// parseMountInfo reads /proc/self/mountinfo. See proc(5): optional fields end at a lone "-".
func parseMountInfo(data []byte) ([]mountEntry, error) {
	var mounts []mountEntry
	for line := range strings.Lines(string(data)) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		sep := slices.Index(f, "-")
		if sep < 6 || len(f) < sep+3 {
			return nil, fmt.Errorf("mountinfo: malformed line %q", strings.TrimSpace(line))
		}
		mounts = append(mounts, mountEntry{
			device:     f[2],
			root:       unescapeMount(f[3]),
			mountPoint: unescapeMount(f[4]),
			fsType:     f[sep+1],
			source:     unescapeMount(f[sep+2]),
		})
	}
	return mounts, nil
}

// unescapeMount decodes the octal escapes (\040 for space, etc.) the kernel uses in mount paths.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// selectMounts keeps allowlisted filesystems and reports each device once: preferring a mount of the
// filesystem root over a bind mount, then the shortest mount point.
func selectMounts(mounts []mountEntry) []mountEntry {
	best := map[string]mountEntry{}
	for _, m := range mounts {
		if !diskFSTypes[m.fsType] {
			continue
		}
		cur, seen := best[m.device]
		if !seen || better(m, cur) {
			best[m.device] = m
		}
	}
	out := make([]mountEntry, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b mountEntry) int { return strings.Compare(a.mountPoint, b.mountPoint) })
	return out
}

func better(a, b mountEntry) bool {
	if (a.root == "/") != (b.root == "/") {
		return a.root == "/"
	}
	return cmp.Or(cmp.Compare(len(a.mountPoint), len(b.mountPoint)), strings.Compare(a.mountPoint, b.mountPoint)) < 0
}

type fsStats struct {
	blocks, bfree, bavail uint64 // in units of bsize
	files, ffree          uint64
	bsize                 uint64
}

func filesystemFromStats(m mountEntry, st fsStats) telemetry.Filesystem {
	total, free, avail := st.blocks*st.bsize, st.bfree*st.bsize, st.bavail*st.bsize
	fs := telemetry.Filesystem{
		MountPoint:     m.mountPoint,
		Type:           m.fsType,
		TotalBytes:     total,
		AvailableBytes: avail,
		UsedBytes:      total - min(free, total),
	}
	if strings.HasPrefix(m.source, "/dev/") {
		fs.Device = &m.source
	}
	// Same as df: blocks reserved for root count as neither used nor available to users.
	if denom := fs.UsedBytes + avail; denom > 0 {
		p := percent(fs.UsedBytes, denom)
		fs.UsedPercent = &p
	}
	if st.files > 0 {
		total, avail := st.files, min(st.ffree, st.files)
		p := percent(total-avail, total)
		fs.InodesTotal, fs.InodesAvailable, fs.InodesUsedPercent = &total, &avail, &p
	}
	return fs
}

func (c *Collector) collectFilesystems() ([]telemetry.Filesystem, error) {
	data, err := c.read("proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	mounts, err := parseMountInfo(data)
	if err != nil {
		return nil, err
	}
	// Containers and sandboxed services bind-mount single files (e.g. /etc/hosts); those are not disks.
	// Only allowlisted types are stat'ed, since stat on a hung network mount can block.
	mounts = slices.DeleteFunc(mounts, func(m mountEntry) bool {
		if !diskFSTypes[m.fsType] {
			return true
		}
		fi, err := os.Stat(filepath.Join(c.root, m.mountPoint))
		return err == nil && !fi.IsDir()
	})
	selected := selectMounts(mounts)
	var errs []error
	if len(selected) > MaxFilesystems {
		errs = append(errs, fmt.Errorf("reported first %d of %d filesystems", MaxFilesystems, len(selected)))
		selected = selected[:MaxFilesystems]
	}
	out := []telemetry.Filesystem{}
	for _, m := range selected {
		st, err := c.statfs(m.mountPoint)
		if err != nil {
			errs = append(errs, fmt.Errorf("statfs %s: %w", m.mountPoint, err))
			continue
		}
		out = append(out, filesystemFromStats(m, st))
	}
	return out, errors.Join(errs...)
}
