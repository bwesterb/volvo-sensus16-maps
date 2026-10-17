package main

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const exampleDest = "/media/you/STICK"

// diskSpace reports the free and total bytes of the volume holding path,
// and its allocation unit.
func diskSpace(path string) (free, total, unit int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, 0, err
	}
	b := int64(st.Frsize) // field widths differ per architecture
	if b == 0 {
		b = int64(st.Bsize)
	}
	return int64(st.Bavail) * b, int64(st.Blocks) * b, b, nil
}

type mount struct {
	point, fstype, source string
	readOnly              bool
}

// mounts reads /proc/self/mountinfo.
func mounts() ([]mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ms []mount
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// ID parent major:minor root mountpoint options [optional...] - fstype source superoptions
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		fields, tail := strings.Fields(pre), strings.Fields(post)
		if !ok || len(fields) < 6 || len(tail) < 2 {
			continue
		}
		ro := false
		for _, o := range strings.Split(fields[5], ",") {
			ro = ro || o == "ro"
		}
		ms = append(ms, mount{unescapeMount(fields[4]), tail[0], tail[1], ro})
	}
	return ms, sc.Err()
}

// mountOf finds the mount holding path: its mount point, file system type
// and source device.
func mountOf(path string) (point, fstype, source string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", "", err
	}
	if p, err := filepath.EvalSymlinks(abs); err == nil {
		abs = p
	}
	ms, err := mounts()
	if err != nil {
		return "", "", "", err
	}
	for _, m := range ms {
		if (abs == m.point || strings.HasPrefix(abs, strings.TrimSuffix(m.point, "/")+"/")) && len(m.point) >= len(point) {
			point, fstype, source = m.point, m.fstype, m.source
		}
	}
	if point == "" {
		return "", "", "", errors.New("mount not found")
	}
	return point, fstype, source, nil
}

// removableVolumes lists writable mounts where desktops put removable
// media.
func removableVolumes() []string {
	ms, _ := mounts()
	var out []string
	for _, m := range ms {
		for _, dir := range []string{"/media/", "/run/media/", "/mnt/"} {
			if strings.HasPrefix(m.point, dir) && !m.readOnly {
				out = append(out, m.point)
				break
			}
		}
	}
	return out
}

// unescapeMount decodes the octal escapes (\040 for a space) in mountinfo.
func unescapeMount(s string) string {
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

// volumeInfo names the volume holding path and its file system.
func volumeInfo(path string) (name, fstype string) {
	point, fstype, _, err := mountOf(path)
	if err != nil {
		return "?", "?"
	}
	return filepath.Base(point), fstype
}

// noCache would bypass the page cache for f. Linux has no simple
// equivalent of macOS's F_NOCACHE (O_DIRECT needs aligned buffers), but it
// also doesn't make closing a file or exiting wait for the dirty pages, so
// writes go through the cache; the stick's write speed in the status then
// reflects the cache more than the stick.
func noCache(f *os.File) {}

// dropCache makes the next reads of f come from the device: flush it, then
// evict its now clean pages.
func dropCache(f *os.File) {
	f.Sync()
	unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}

// prepareReadBack: dropCache per file is enough on Linux.
func prepareReadBack(dest string) (string, error) {
	return dest, nil
}

func eject(dest string) error {
	_, _, source, err := mountOf(dest)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("udisksctl"); err != nil {
		return errors.New("udisksctl not found; unmount the stick yourself")
	}
	for _, args := range [][]string{{"unmount", "-b", source}, {"power-off", "-b", source}} {
		cmd := exec.Command("udisksctl", args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
	}
	return nil
}

func systemCountry() string { return countryFromEnv() }

func setupTerminal() {}
