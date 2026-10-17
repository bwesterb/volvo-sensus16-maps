package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const exampleDest = `E:\`

func volumeRoot(path string) (*uint16, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return windows.UTF16PtrFromString(filepath.VolumeName(abs) + `\`)
}

// diskSpace reports the free and total bytes of the volume holding path,
// and its allocation unit.
func diskSpace(path string) (free, total, unit int64, err error) {
	root, err := volumeRoot(path)
	if err != nil {
		return 0, 0, 0, err
	}
	var avail, tot, totFree uint64
	if err := windows.GetDiskFreeSpaceEx(root, &avail, &tot, &totFree); err != nil {
		return 0, 0, 0, err
	}
	// The cluster size isn't readily available; 64 KiB is the largest
	// FAT32 and exFAT use on sticks of this size, so this errs on the safe
	// side when estimating space.
	return int64(avail), int64(tot), 64 << 10, nil
}

// removableVolumes lists the drives Windows considers removable.
func removableVolumes() []string {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := windows.UTF16PtrFromString(root)
		if windows.GetDriveType(p) == windows.DRIVE_REMOVABLE {
			out = append(out, root)
		}
	}
	return out
}

// volumeInfo names the volume holding path and its file system.
func volumeInfo(path string) (name, fstype string) {
	root, err := volumeRoot(path)
	if err != nil {
		return "?", "?"
	}
	var label, fs [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeInformation(root, &label[0], uint32(len(label)), nil, nil, nil, &fs[0], uint32(len(fs))); err != nil {
		return "?", "?"
	}
	name = windows.UTF16ToString(label[:])
	if name == "" {
		name = filepath.VolumeName(path)
	}
	return name, windows.UTF16ToString(fs[:])
}

// noCache would bypass the file cache; Windows' FILE_FLAG_NO_BUFFERING
// needs aligned buffers, so writes go through the cache.
func noCache(f *os.File) {}

func dropCache(f *os.File) {}

func prepareReadBack(dest string) (string, error) {
	return "", errors.New("Windows may serve the read-back from its cache")
}

func eject(dest string) error {
	return errors.New(`eject the stick with "Safely Remove Hardware"`)
}

func systemCountry() string { return countryFromEnv() }

// setupTerminal turns on ANSI escape sequences, for the status display.
func setupTerminal() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
}
