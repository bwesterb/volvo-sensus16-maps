package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const exampleDest = "/Volumes/STICK"

// diskSpace reports the free and total bytes of the volume holding path,
// and its allocation unit.
func diskSpace(path string) (free, total, unit int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	b := int64(st.Bsize)
	return int64(st.Bavail) * b, int64(st.Blocks) * b, b, nil
}

// removableVolumes lists the writable volumes mounted under /Volumes,
// other than the startup disk (a symlink to /).
func removableVolumes() []string {
	entries, _ := os.ReadDir("/Volumes")
	var out []string
	for _, e := range entries {
		p := filepath.Join("/Volumes", e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		var st unix.Statfs_t
		if unix.Statfs(p, &st) != nil || unix.ByteSliceToString(st.Mntonname[:]) != p ||
			st.Flags&unix.MNT_RDONLY != 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

// volumeInfo names the volume holding path and its file system.
func volumeInfo(path string) (name, fstype string) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "?", "?"
	}
	return filepath.Base(unix.ByteSliceToString(st.Mntonname[:])), unix.ByteSliceToString(st.Fstypename[:])
}

// noCache makes reads from and writes to f bypass the buffer cache.
func noCache(f *os.File) {
	unix.FcntlInt(f.Fd(), unix.F_NOCACHE, 1)
}

// dropCache makes the next reads of f come from the device. With noCache
// on the file that's already so.
func dropCache(f *os.File) {}

// prepareReadBack makes sure reads from dest come from the device: files
// written earlier through the cache may still be in memory, so unmount and
// remount the volume. It returns the (possibly changed) mount point.
func prepareReadBack(dest string) (string, error) {
	mp, dev, err := diskutilInfo(dest)
	if err != nil {
		return "", err
	}
	a, _ := filepath.EvalSymlinks(mp)
	b, _ := filepath.EvalSymlinks(dest)
	if mp == "" || a != b {
		return "", errors.New("not the root of a separate volume")
	}
	if out, err := exec.Command("diskutil", "unmount", dest).CombinedOutput(); err != nil {
		return "", fmt.Errorf("can't unmount: %s", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("diskutil", "mount", dev).CombinedOutput(); err != nil {
		return "", fmt.Errorf("can't remount %s: %s", dev, strings.TrimSpace(string(out)))
	}
	mp, _, err = diskutilInfo(dev)
	return mp, err
}

func diskutilInfo(target string) (mountPoint, device string, err error) {
	out, err := exec.Command("diskutil", "info", target).Output()
	if err != nil {
		return "", "", fmt.Errorf("diskutil info %s: %w", target, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Mount Point":
			mountPoint = strings.TrimSpace(v)
		case "Device Identifier":
			device = strings.TrimSpace(v)
		}
	}
	return mountPoint, device, nil
}

func eject(dest string) error {
	cmd := exec.Command("diskutil", "eject", dest)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// systemCountry is the region of the macOS locale, e.g. en_NL -> NL.
func systemCountry() string {
	if out, err := exec.Command("defaults", "read", "-g", "AppleLocale").Output(); err == nil {
		if c := countryFromLocale(string(out)); c != "" {
			return c
		}
	}
	return countryFromEnv()
}

func setupTerminal() {}
