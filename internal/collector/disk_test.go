package collector

import (
	"slices"
	"testing"
)

const mountinfo = `22 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw,errors=remount-ro
23 22 0:21 / /proc rw,nosuid shared:12 - proc proc rw
24 22 0:22 / /sys rw,nosuid shared:7 - sysfs sysfs rw
25 22 0:5 / /dev rw,nosuid shared:2 - devtmpfs udev rw,size=4000000k
26 22 0:24 / /run rw,nosuid shared:5 - tmpfs tmpfs rw,size=800000k
27 24 0:26 / /sys/fs/cgroup rw,nosuid shared:9 - cgroup2 cgroup2 rw
28 22 7:0 / /snap/core22/1 ro,nodev shared:30 - squashfs /dev/loop0 ro
29 22 252:16 / /home rw,relatime shared:31 - xfs /dev/vdb rw
30 22 252:1 /srv/data /mnt/bind rw,relatime shared:1 - ext4 /dev/vda1 rw
31 22 0:40 / /var/lib/docker/overlay2/abc/merged rw,relatime - overlay overlay rw,lowerdir=/x
32 22 0:41 / /mnt/nfs rw,relatime - nfs4 10.0.0.2:/export rw
33 22 252:32 / /mnt/My\040Disk rw,relatime shared:40 - ext4 /dev/vdc rw
34 22 252:16 / /var/lib/backups rw,relatime shared:31 - xfs /dev/vdb rw
`

func TestParseAndSelectMounts(t *testing.T) {
	mounts, err := parseMountInfo([]byte(mountinfo))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 13 {
		t.Fatalf("parsed %d mounts, want 13", len(mounts))
	}
	var points []string
	for _, m := range selectMounts(mounts) {
		points = append(points, m.mountPoint)
	}
	// Root is kept, the bind mount of vda1 and the second vdb mount are collapsed, pseudo/overlay/network
	// filesystems are excluded, and the escaped space is decoded.
	want := []string{"/", "/home", "/mnt/My Disk"}
	if !slices.Equal(points, want) {
		t.Fatalf("selected %q, want %q", points, want)
	}
	if _, err := parseMountInfo([]byte("22 1 252:1 / / rw\n")); err == nil {
		t.Error("line without separator: want error")
	}
}

func TestSelectMountsPrefersFilesystemRoot(t *testing.T) {
	got := selectMounts([]mountEntry{
		{device: "8:1", root: "/data", mountPoint: "/a", fsType: "ext4"},
		{device: "8:1", root: "/", mountPoint: "/mnt/longer/path", fsType: "ext4"},
	})
	if len(got) != 1 || got[0].mountPoint != "/mnt/longer/path" {
		t.Fatalf("got %+v; want the mount of the filesystem root, not the bind mount", got)
	}
}

func TestFilesystemFromStats(t *testing.T) {
	m := mountEntry{mountPoint: "/", fsType: "ext4", source: "/dev/vda1"}
	// 100 blocks of 4 KiB: 40 free, of which 30 available to users (10 reserved for root).
	fs := filesystemFromStats(m, fsStats{blocks: 100, bfree: 40, bavail: 30, files: 1000, ffree: 750, bsize: 4096})
	if fs.TotalBytes != 409600 || fs.UsedBytes != 245760 || fs.AvailableBytes != 122880 {
		t.Fatalf("bytes = %+v", fs)
	}
	// df semantics: used / (used + available) = 60 / 90.
	if fs.UsedPercent == nil || *fs.UsedPercent != 66.67 {
		t.Errorf("usedPercent = %v, want 66.67", fs.UsedPercent)
	}
	if fs.InodesTotal == nil || *fs.InodesTotal != 1000 || *fs.InodesAvailable != 750 || *fs.InodesUsedPercent != 25 {
		t.Errorf("inodes = %v %v %v", fs.InodesTotal, fs.InodesAvailable, fs.InodesUsedPercent)
	}
	if fs.Device == nil || *fs.Device != "/dev/vda1" {
		t.Errorf("device = %v", fs.Device)
	}

	// btrfs reports no inode counts; those fields must be null, not zero.
	btrfs := filesystemFromStats(mountEntry{mountPoint: "/", fsType: "btrfs", source: "/dev/sda2"},
		fsStats{blocks: 10, bfree: 5, bavail: 5, bsize: 4096})
	if btrfs.InodesTotal != nil || btrfs.InodesUsedPercent != nil {
		t.Errorf("btrfs inodes should be nil: %+v", btrfs)
	}
	// ZFS datasets have no /dev source.
	zfs := filesystemFromStats(mountEntry{mountPoint: "/tank", fsType: "zfs", source: "tank/data"}, fsStats{})
	if zfs.Device != nil || zfs.UsedPercent != nil {
		t.Errorf("zfs empty dataset = %+v", zfs)
	}
}

func TestUnescapeMount(t *testing.T) {
	tests := map[string]string{`/a\040b`: "/a b", `/tab\011x`: "/tab\tx", `/plain`: "/plain", `/trail\04`: `/trail\04`}
	for in, want := range tests {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}
