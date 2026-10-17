package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// regionFilesPattern matches the files of one region in any release, e.g.
// EU_UR142_DE_1550_of_02.zip, plus .part files and macOS ._ companions.
func regionFilesPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`^(\._)?` + regexp.QuoteMeta(name) + `_(\d+)(_of_\d+)?\.(zip|DBINFO)(\.part)?$`)
}

func fileSize(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return st.Size(), true
}

// localStatus reports a package's state in dest: current, partial, old or
// missing.
func localStatus(dest string, m *Manifest) (state, detail string) {
	if isImage(m) && !isLatestImage(dest, m) {
		// Not this map in this release (yet): which, then?
		info := readImageInfo(dest)
		switch {
		case !info.present && len(imageFiles(dest)) > 0:
			return "old", "unidentified map files"
		case info.name != "" && info.name != m.Name:
			return "missing", "(the stick has " + info.name + ")"
		case info.name == m.Name:
			return "old", "release " + info.release
		case info.present:
			return "old", "another map image or release"
		}
	}
	// Otherwise go by the files. A map image's DBINFO.ME is only there
	// (rather than as .part) once the rest is complete.
	present, partial := 0, false
	for _, f := range m.Files {
		if n, ok := fileSize(filepath.Join(dest, f.Path)); ok && n == f.Size {
			present++
		} else if ok {
			partial = true
		} else if _, ok := fileSize(filepath.Join(dest, f.Path+".part")); ok {
			partial = true
		}
	}
	if present == len(m.Files) {
		return "current", ""
	}
	if present > 0 || partial {
		return "partial", fmt.Sprintf("%d/%d files", present, len(m.Files))
	}
	pat := regionFilesPattern(m.Name)
	entries, _ := os.ReadDir(dest)
	var old []string
	for _, e := range entries {
		if mt := pat.FindStringSubmatch(e.Name()); mt != nil && mt[1] == "" && !slices.Contains(old, mt[2]) {
			old = append(old, mt[2])
		}
	}
	if len(old) > 0 {
		slices.Sort(old)
		return "old", "release " + strings.Join(old, ", ")
	}
	return "missing", ""
}

// imageFiles lists the files of map images in dest.
func imageFiles(dest string) []string {
	entries, _ := os.ReadDir(dest)
	var out []string
	for _, e := range entries {
		if imageFile.MatchString(e.Name()) {
			out = append(out, filepath.Join(dest, e.Name()))
		}
	}
	return out
}

// oldFiles lists other releases of this region (and macOS ._ litter) in dest.
func oldFiles(dest string, m *Manifest) []string {
	if isImage(m) {
		// Image files have the same names in every release, so they're
		// all stale unless DBINFO.ME(.part) says they're this release.
		if isLatestImage(dest, m) {
			return nil
		}
		return imageFiles(dest)
	}
	keep := map[string]bool{}
	for _, f := range m.Files {
		keep[f.Path], keep[f.Path+".part"] = true, true
	}
	pat := regionFilesPattern(m.Name)
	entries, _ := os.ReadDir(dest)
	var out []string
	for _, e := range entries {
		if pat.MatchString(e.Name()) && !keep[e.Name()] {
			out = append(out, filepath.Join(dest, e.Name()))
		}
	}
	return out
}

// removeAppleDouble deletes the ._ companions of the regions' files. macOS
// tags files it creates (com.apple.provenance) and FAT can't store that, so
// every file gets a hidden ._ file. The car doesn't need them.
func removeAppleDouble(dest string, ms []*Manifest) error {
	entries, err := os.ReadDir(dest)
	if err != nil {
		return err
	}
	for _, m := range ms {
		pat := regionFilesPattern(m.Name)
		if isImage(m) {
			pat = imageFile
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "._") && pat.MatchString(e.Name()) {
				if err := os.Remove(filepath.Join(dest, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
	}
	return nil
}

// checkSpace reports whether dest can hold todo once the stale files are
// removed. With fatal it returns an error if it can't; otherwise it just
// prints the verdict.
func checkSpace(dest string, todo []region, stale []string, fatal bool) error {
	free, capacity, cluster, err := diskSpace(dest)
	if err != nil {
		return err
	}
	onDisk := func(n int64) int64 { // FAT/exFAT allocate whole clusters
		return (n + cluster - 1) / cluster * cluster
	}
	present := func(path string) int64 {
		for _, p := range []string{path, path + ".part"} {
			if n, ok := fileSize(p); ok {
				return onDisk(n)
			}
		}
		return 0
	}
	var need, total, freed int64
	for _, r := range todo {
		for _, f := range r.m.Files {
			need += onDisk(f.Size) - present(filepath.Join(dest, f.Path))
			total += onDisk(f.Size)
		}
	}
	for _, p := range stale {
		if n, ok := fileSize(p); ok {
			freed += onDisk(n)
		}
	}
	const margin = 64 * 1000 * 1000 // directory entries, FAT metadata
	msg := fmt.Sprintf("Space: %s to download, %s free", human(float64(need)), human(float64(free)))
	if freed > 0 {
		msg += fmt.Sprintf(" + %s from old releases", human(float64(freed)))
	}
	fmt.Println(msg)
	if need+margin <= free+freed {
		if !fatal {
			fmt.Println(paintIf(useColor(), ansiGreen, "Everything fits."))
		}
		return nil
	}
	hint := " Free up space or use an emptier stick."
	if total+margin > capacity {
		hint = fmt.Sprintf(" The selected regions take %s in total, more than the %s the stick holds; select fewer regions.",
			human(float64(total)), human(float64(capacity)))
	}
	msg = fmt.Sprintf("Not enough space on %s: short by %s.%s", dest, human(float64(need+margin-free-freed)), hint)
	if fatal {
		return errors.New(msg + " Nothing was changed.")
	}
	fmt.Println(paintIf(useColor(), ansiRed, msg))
	return nil
}

// verifyOnDisk re-reads files from dest, bypassing the cache, and checks
// their MD5. Bad files are turned back into .part files so a rerun repairs
// only the chunks that are wrong. It returns the number of bad files.
func verifyOnDisk(ctx context.Context, dest string, files []File) (int, error) {
	var total, done int64
	for _, f := range files {
		total += f.Size
	}
	bad := 0
	t0 := time.Now()
	last := time.Time{}
	buf := make([]byte, 8<<20)
	for _, f := range files {
		path := filepath.Join(dest, f.Path)
		fh, err := os.Open(path)
		if err != nil {
			return bad, err
		}
		noCache(fh)
		dropCache(fh)
		h := md5.New()
		for {
			if ctx.Err() != nil {
				fh.Close()
				return bad, ctx.Err()
			}
			n, err := fh.Read(buf)
			h.Write(buf[:n])
			done += int64(n)
			if now := time.Now(); now.Sub(last) > 500*time.Millisecond || err == io.EOF {
				last = now
				rate := float64(done) / now.Sub(t0).Seconds()
				eta := "-"
				if rate > 0 {
					eta = fmtDuration(time.Duration(float64(total-done) / rate * float64(time.Second)))
				}
				fmt.Printf("\r  %s / %s  (%5.1f%%)  %s/s  ETA %s   ", human(float64(done)), human(float64(total)),
					100*float64(done)/float64(total), human(rate), eta)
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				fh.Close()
				return bad, err
			}
		}
		fh.Close()
		if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), f.MD5) {
			fmt.Printf("\n  %s: MD5 mismatch on disk\n", f.Path)
			if err := os.Rename(path, path+".part"); err != nil {
				return bad, err
			}
			bad++
		}
	}
	fmt.Println()
	return bad, nil
}
