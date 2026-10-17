package main

import (
	"errors"
	"strings"
	"testing"
)

func TestPickStick(t *testing.T) {
	maps := volume{path: "/m", name: "MAPS", fstype: "msdos", total: 64e9, maps: true}
	maps2 := volume{path: "/m2", name: "MAPS2", fstype: "exfat", total: 64e9, maps: true}
	empty := volume{path: "/e", name: "EMPTY", fstype: "msdos", total: 64e9}
	empty2 := volume{path: "/e2", name: "CAMERA", fstype: "exfat", total: 32e9}
	yes := func(string) (bool, error) { return true, nil }
	no := func(string) (bool, error) { return false, nil }
	cannot := func(string) (bool, error) { return false, errors.New("not a terminal") }
	mustNotAsk := func(string) (bool, error) { t.Fatal("asked"); return false, nil }

	for _, tc := range []struct {
		name    string
		vs      []volume
		confirm bool
		ask     func(string) (bool, error)
		want    string // path, or error substring prefixed with "!"
	}{
		{"the stick with maps wins", []volume{empty, maps, empty2}, true, mustNotAsk, "/m"},
		{"two with maps", []volume{maps, maps2}, true, mustNotAsk, "!several sticks have Sensus maps"},
		{"none", nil, true, mustNotAsk, "!no USB stick found"},
		{"two without maps", []volume{empty, empty2}, true, mustNotAsk, "!several possible sticks"},
		{"one without maps, confirmed", []volume{empty}, true, yes, "/e"},
		{"one without maps, declined", []volume{empty}, true, no, "!not confirmed"},
		{"one without maps, can't ask", []volume{empty}, true, cannot, "!pass -dest /e"},
		{"one without maps, list doesn't ask", []volume{empty}, false, mustNotAsk, "/e"},
	} {
		got, err := pickStick(tc.vs, tc.confirm, 33e9, tc.ask)
		if strings.HasPrefix(tc.want, "!") {
			if err == nil || !strings.Contains(err.Error(), tc.want[1:]) {
				t.Errorf("%s: got %q, %v; want error containing %q", tc.name, got, err, tc.want[1:])
			}
		} else if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestMapFileName(t *testing.T) {
	for name, want := range map[string]string{
		"EU_UR142_DE_1560.zip":            "EU_UR142_DE",
		"EU_UR142_DE_1560_of_02.zip":      "EU_UR142_DE",
		"EU_UR146_BE-LU-NL_1560.DBINFO":   "EU_UR146_BE-LU-NL",
		"EU_UR170_AT-CH-LI_1560.zip.part": "EU_UR170_AT-CH-LI",
		"NA_UR301_TX_1560_of_03.zip":      "NA_UR301_TX",
		"._EU_UR142_DE_1560.zip":          "",
		"holiday.jpg":                     "",
		"EU_904_DBINFO.ME":                "",
	} {
		got := ""
		if m := mapFileName.FindStringSubmatch(name); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
}

func TestPackageIDs(t *testing.T) {
	utf16le := func(s string) []byte {
		var b []byte
		for _, c := range s {
			b = append(b, byte(c), 0)
		}
		return b
	}
	blob := append([]byte("MZ\x90junk 54d0cc889bafa209404b32db (ascii, ignored) "), utf16le("x54d0cc889bafa209404b32dby")...)
	blob = append(blob, utf16le(partner)...)
	blob = append(blob, utf16le("54d0cc889bafa209404b32db")...) // duplicate
	got := packageIDs(blob)
	want := []string{"54d0cc889bafa209404b32db", partner}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}
