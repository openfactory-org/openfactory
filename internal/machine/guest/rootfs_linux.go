//go:build linux

package guest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/openfactory-org/openfactory/internal/machine/mmds"
)

// setupRoot mounts the image, the scratch overlay, the tools and the cache
// disks under /mnt/root and writes the files of /etc.
func setupRoot(p mmds.Payload) error {
	d := p.Disks
	if d == nil {
		return errors.New("the document names no disks")
	}
	drives := []string{d.Image, d.Scratch}
	for _, c := range d.Cache {
		drives = append(drives, c.Drive)
	}
	devs := MapDisks("/sys/block", drives)
	img, scr := devs[d.Image], devs[d.Scratch]
	if img == "" || scr == "" {
		return fmt.Errorf("image or scratch disk not found (%v)", devs)
	}
	steps := []struct {
		src, dst, fs string
		flags        uintptr
		data         string
	}{
		{img, "/mnt/lower", "ext4", unix.MS_RDONLY | unix.MS_NODEV, "noload"},
		{scr, "/mnt/scratch", "ext4", unix.MS_NODEV | unix.MS_NOSUID, "discard"},
	}
	for _, s := range steps {
		if err := os.MkdirAll(s.dst, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(s.src, s.dst, s.fs, s.flags, s.data); err != nil {
			return fmt.Errorf("mount %s: %w", s.dst, err)
		}
	}
	for _, dir := range []string{"/mnt/scratch/upper", "/mnt/scratch/work", "/mnt/root"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := unix.Mount("overlay", "/mnt/root", "overlay", unix.MS_NODEV|unix.MS_NOSUID,
		"lowerdir=/mnt/lower,upperdir=/mnt/scratch/upper,workdir=/mnt/scratch/work"); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	r := "/mnt/root"
	if err := os.MkdirAll(r+ToolsMount, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("/", r+ToolsMount, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind tools: %w", err)
	}
	_ = unix.Mount("", r+ToolsMount, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
	for _, c := range d.Cache {
		dev := devs[c.Drive]
		if dev == "" {
			continue
		}
		dst := filepath.Join(r, GitCacheMount, c.Key+".git")
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := unix.Mount(dev, dst, "ext4", unix.MS_RDONLY|unix.MS_NODEV|unix.MS_NOSUID|unix.MS_NOEXEC, "noload"); err != nil {
			return fmt.Errorf("mount git cache %s: %w", c.Key, err)
		}
	}
	if err := mountAll([][4]string{
		{"proc", r + "/proc", "proc", ""},
		{"sysfs", r + "/sys", "sysfs", ""},
		{"devtmpfs", r + "/dev", "devtmpfs", ""},
		{"devpts", r + "/dev/pts", "devpts", "newinstance,ptmxmode=0666"},
		{"tmpfs", r + "/dev/shm", "tmpfs", "nosuid,nodev"},
		{"tmpfs", r + "/run", "tmpfs", "nosuid,nodev"},
		{"tmpfs", r + "/tmp", "tmpfs", "nosuid,nodev,size=25%"},
	}); err != nil {
		return err
	}
	dns := []string{}
	if p.Network != nil {
		dns = p.Network.DNS
	}
	files := map[string]string{
		"/etc/hostname":    p.Hostname + "\n",
		"/etc/hosts":       Hosts(p.Hostname),
		"/etc/resolv.conf": ResolvConf(dns),
		"/etc/machine-id":  p.MachineID + "\n",
	}
	_ = os.MkdirAll(r+"/etc", 0o755)
	for f, data := range files {
		_ = os.Remove(r + f)
		if err := os.WriteFile(r+f, []byte(data), 0o644); err != nil { //nolint:gosec // G306: world-readable system files
			return err
		}
	}
	if len(d.Cache) > 0 {
		gc, _ := os.OpenFile(r+"/etc/gitconfig", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G302: world-readable git config
		if gc != nil {
			_, _ = gc.WriteString("[safe]\n\tdirectory = *\n")
			_ = gc.Close()
		}
	}
	return nil
}

func pivot() error {
	if err := os.Chdir("/mnt/root"); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach the old root: %w", err)
	}
	return os.Chdir("/")
}
