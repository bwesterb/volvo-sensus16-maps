package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// Finding the USB stick when -dest isn't given.

// carFilesystems are the file systems the car reads (FAT32, exFAT, NTFS),
// as the platforms name them.
var carFilesystems = map[string]bool{
	"msdos": true, "vfat": true, "fat32": true, "fat": true, // FAT32
	"exfat": true,
	"ntfs":  true, "ntfs3": true, "fuseblk": true, // fuseblk: ntfs-3g
}

type volume struct {
	path, name, fstype string
	total              int64
	maps               bool // holds Sensus map files already
}

func (v volume) String() string {
	s := fmt.Sprintf("%s (%s, %s, %s)", v.path, v.name, v.fstype, human(float64(v.total)))
	if v.maps {
		s += ", has Sensus maps"
	}
	return s
}

func hasMaps(dir string) bool {
	return len(mapsOnStick(dir)) > 0
}

// findSticks lists the mounted removable volumes the car could read.
func findSticks() []volume {
	var vs []volume
	for _, p := range removableVolumes() {
		name, fstype := volumeInfo(p)
		if !carFilesystems[strings.ToLower(fstype)] {
			continue
		}
		_, total, _, err := diskSpace(p)
		if err != nil {
			continue
		}
		vs = append(vs, volume{p, name, fstype, total, hasMaps(p)})
	}
	return vs
}

// chooseStick picks the stick to use: the one with Sensus maps on it, or
// else the only candidate. Writing to a volume without maps on it needs
// confirmation when confirm is set: it might be someone's camera card.
func chooseStick(confirm bool, size int64) (string, error) {
	return pickStick(findSticks(), confirm, size, askUser)
}

// askUser asks a yes/no question on the terminal; it can't ask (and
// returns an error) when stdin isn't one.
func askUser(question string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, errors.New("can't ask: not a terminal")
	}
	fmt.Print(question + " [y/N] ")
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "y" || a == "yes", nil
}

func pickStick(vs []volume, confirm bool, size int64, ask func(string) (bool, error)) (string, error) {
	var withMaps []volume
	for _, v := range vs {
		if v.maps {
			withMaps = append(withMaps, v)
		}
	}
	list := func(vs []volume) string {
		var b strings.Builder
		for _, v := range vs {
			b.WriteString("\n  " + v.String())
		}
		return b.String()
	}
	switch {
	case len(withMaps) == 1:
		fmt.Printf("Using %s\n", withMaps[0])
		return withMaps[0].path, nil
	case len(withMaps) > 1:
		return "", fmt.Errorf("several sticks have Sensus maps; pick one with -dest:%s", list(withMaps))
	case len(vs) == 0:
		return "", fmt.Errorf("no USB stick found (FAT32, exFAT or NTFS); plug one in, or pass -dest, e.g. -dest %s", exampleDest)
	case len(vs) > 1:
		return "", fmt.Errorf("several possible sticks; pick one with -dest:%s", list(vs))
	}
	v := vs[0]
	if !confirm {
		fmt.Printf("Using %s\n", v)
		return v.path, nil
	}
	ok, err := ask(fmt.Sprintf("Found %s, which has no Sensus maps yet.\nDownload up to %s of maps to it?",
		v, human(float64(size))))
	if err != nil {
		return "", fmt.Errorf("found %s, which has no Sensus maps yet; pass -dest %s to use it", v, v.path)
	}
	if !ok {
		return "", errors.New("not confirmed; nothing was changed")
	}
	return v.path, nil
}
