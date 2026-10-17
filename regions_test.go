package main

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dbinfo is the DBINFO.ME for a map version like TK_914_1560, or, for ""
// one without a MapVersion line, like Japan's.
func dbinfo(version string) string {
	if version == "" {
		return "[Map]\r\nDisplay<Market>=HC2750-01\r\n"
	}
	return "DBINFO.ME_EU_Ver1.0\r\n[Map]\r\nMapVersion=" + version + "\r\nName=XX\r\n"
}

// imageManifest describes map image name in release; its DBINFO.ME is
// dbinfo(info).
func imageManifest(name, release, info string) *Manifest {
	m := &Manifest{Name: name}
	m.Version.Name = release
	sum := md5.Sum([]byte(dbinfo(info)))
	for _, p := range []string{"CHECKSUM", "DBINFO.ME", "MapImageSPA.001", "MapImageSPA.002"} {
		f := File{Path: p, Size: 4}
		if p == "DBINFO.ME" {
			f.MD5 = hex.EncodeToString(sum[:])
		}
		m.Files = append(m.Files, f)
	}
	return m
}

func regionalManifest(name, release string) *Manifest {
	m := &Manifest{Name: name}
	m.Version.Name = release
	m.Files = []File{{Path: name + "_" + release + ".DBINFO", Size: 4}, {Path: name + "_" + release + ".zip", Size: 4}}
	return m
}

// stick makes a directory with the given files; DBINFO.ME(.part) files get
// a MapVersion line for the given "name_release".
func stick(t *testing.T, files map[string]string) string {
	dir := t.TempDir()
	for name, version := range files {
		data := "1234"
		if strings.HasPrefix(name, "DBINFO.ME") {
			data = dbinfo(version)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestImageStatus(t *testing.T) {
	tk := imageManifest("TK_914", "1560", "TK_914_1560")
	for _, tc := range []struct {
		name    string
		files   map[string]string
		state   string
		stale   int
		onStick []string
	}{
		{"empty stick", nil, "missing", 0, nil},
		{"older release", map[string]string{"DBINFO.ME": "TK_914_1550", "MapImageSPA.001": ""}, "old", 2, []string{"TK_914"}},
		{"in progress", map[string]string{"DBINFO.ME.part": "TK_914_1560", "CHECKSUM": "", "MapImageSPA.001.part": ""}, "partial", 0, []string{"TK_914"}},
		{"another market", map[string]string{"DBINFO.ME": "NA_907_1560", "MapImageSPA.001": ""}, "missing", 2, []string{"NA_907"}},
		{"unidentified files", map[string]string{"MapImageSPA.001": "", "._MapImageSPA.001": ""}, "old", 2, nil},
	} {
		dir := stick(t, tc.files)
		if state, _ := localStatus(dir, tk); state != tc.state {
			t.Errorf("%s: state %s, want %s", tc.name, state, tc.state)
		}
		if stale := oldFiles(dir, tk); len(stale) != tc.stale {
			t.Errorf("%s: %d stale files %v, want %d", tc.name, len(stale), stale, tc.stale)
		}
		if got := mapsOnStick(dir); !slices.Equal(got, tc.onStick) {
			t.Errorf("%s: maps on stick %v, want %v", tc.name, got, tc.onStick)
		}
	}

	// Japan's DBINFO.ME doesn't name the map: it's recognised by checksum.
	jp := imageManifest("SPA_JP", "25w46", "")
	dir := stick(t, map[string]string{"DBINFO.ME.part": "", "MapImageSPA.001.part": ""})
	if state, _ := localStatus(dir, jp); state != "partial" || len(oldFiles(dir, jp)) != 0 {
		t.Errorf("unnamed image in progress: state %s, stale %v", state, oldFiles(dir, jp))
	}
	if got := mapsOnStick(dir); !slices.Equal(got, []string{unnamedImage}) {
		t.Errorf("unnamed image: maps on stick %v", got)
	}
	if state, _ := localStatus(dir, tk); state != "old" || len(oldFiles(dir, tk)) != 2 {
		t.Errorf("unnamed image vs another map: state %s, stale %v", state, oldFiles(dir, tk))
	}

	// Complete: every file present with the right size, DBINFO.ME final.
	dir = stick(t, map[string]string{"CHECKSUM": "", "DBINFO.ME": "TK_914_1560", "MapImageSPA.001": "", "MapImageSPA.002": ""})
	for i := range tk.Files {
		st, _ := os.Stat(filepath.Join(dir, tk.Files[i].Path))
		tk.Files[i].Size = st.Size()
	}
	if state, _ := localStatus(dir, tk); state != "current" {
		t.Errorf("complete image: state %s, want current", state)
	}
}

func TestCheckMix(t *testing.T) {
	tk := region{"turkey", imageManifest("TK_914", "1560", "TK_914_1560")}
	na := region{"north-america", imageManifest("NA_907", "1560", "NA_907_1560")}
	de := region{"germany", regionalManifest("EU_UR142_DE", "1560")}
	empty := stick(t, nil)
	withRegional := stick(t, map[string]string{"EU_UR142_DE_1560.zip": ""})
	withImage := stick(t, map[string]string{"DBINFO.ME": "NA_907_1560"})
	for _, tc := range []struct {
		name     string
		dest     string
		selected []region
		ok       bool
	}{
		{"regional on empty", empty, []region{de}, true},
		{"image on empty", empty, []region{tk}, true},
		{"two images", empty, []region{tk, na}, false},
		{"image and regional", empty, []region{tk, de}, false},
		{"image onto regional stick", withRegional, []region{tk}, false},
		{"regional onto image stick", withImage, []region{de}, false},
		{"image replacing another", withImage, []region{tk}, true},
		{"regional onto regional stick", withRegional, []region{de}, true},
	} {
		if err := checkMix(tc.dest, tc.selected); (err == nil) != tc.ok {
			t.Errorf("%s: got %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
