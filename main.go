// Command volvo-sensus16-maps downloads Volvo Sensus Navigation 2016 (SPA)
// map updates onto a USB stick.
//
// It replaces Volvo's per-region "Sensus Update" downloaders with one tool.
// Those apps are thin clients for Reloaded Technologies' "Super Node
// Delivery":
//
//	manifest  https://cache.manifests.reloadedtech.com/manifests/<package>/current.json
//	          (latest release: file list, sizes, MD5 and a CRC32 per chunk)
//	nodes     https://cache.manifests.reloadedtech.com/nodes/<partner>.json
//	          (weighted list of download servers)
//	files     https://<node>/packages/<partner>/<package>/<version>/<path>
//
// The original client fetches the .xml flavour of the same manifests and the
// files over plain HTTP, one Range request per manifest chunk, with 12-32
// requests in flight and retries without delay. It also checks the download
// against a country whitelist/blacklist and posts usage statistics, which
// nothing depends on and which this tool doesn't send. We use HTTPS, fetch
// from the nearest node only (more nodes add no capacity, just stragglers),
// adapt the number of requests in flight (1-12) to what the connection and
// disk take, halve it on errors, and back off between retries.
//
// Files are written flat into the destination, which can be the USB stick
// itself: the car wants the map files in the root of the stick, as they
// come, without unpacking anything.
//
// Usage:
//
//	volvo-sensus16-maps list   [-dest DIR] [REGION ...]
//	volvo-sensus16-maps fetch  [-dest DIR] [flags] [REGION ...]   (default: the regions on the stick)
//
// Without -dest it looks for the USB stick: the one with Sensus maps on it,
// or else the only removable FAT32/exFAT/NTFS volume.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	manifestHost = "https://cache.manifests.reloadedtech.com"
	partner      = "52d4e92eef91100e689f9d22" // Volvo Sensus maps
	userAgent    = "volvo-sensus16-maps/1.0 (+https://github.com/bwesterb/volvo-sensus16-maps)"
)

type Manifest struct {
	PackageID string `json:"PackageId"`
	PartnerID string `json:"PartnerId"`
	VersionID string `json:"VersionId"`
	Name      string
	IsActive  *bool
	Version   struct {
		Name         string
		Policy       string
		CountryCodes []string
	}
	Size  int64
	Files []File
}

type File struct {
	Path     string
	Chunk    int64
	Size     int64
	Checksum string // 8 hex digits of CRC32 per chunk
	MD5      string
}

type Node struct {
	Host   string
	Path   string
	Weight float64
}

type region struct {
	name string
	m    *Manifest
}

var metaClient = &http.Client{Timeout: 60 * time.Second}

func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := metaClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func fetchManifest(ctx context.Context, pkg string) (*Manifest, error) {
	var m Manifest
	err := getJSON(ctx, manifestHost+"/manifests/"+pkg+"/current.json", &m)
	return &m, err
}

func fetchNodes(ctx context.Context) ([]Node, error) {
	var ns []Node
	err := getJSON(ctx, manifestHost+"/nodes/"+partner+".json", &ns)
	return ns, err
}

func human(n float64) string {
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if n < 1000 || unit == "GB" {
			if unit == "B" {
				return fmt.Sprintf("%.0f B", n)
			}
			return fmt.Sprintf("%.1f %s", n, unit)
		}
		n /= 1000
	}
	panic("unreachable")
}

// countryFromLocale is the region of a locale name, e.g. en_NL.UTF-8 -> NL.
func countryFromLocale(loc string) string {
	m := regexp.MustCompile(`_([A-Z]{2})\b`).FindStringSubmatch(loc)
	if m == nil {
		return ""
	}
	return m[1]
}

// countryFromEnv is the region of the locale in the environment.
func countryFromEnv() string {
	for _, v := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if c := countryFromLocale(os.Getenv(v)); c != "" {
			return c
		}
	}
	return ""
}

// allowedIn applies the package's country policy the way the original
// client does.
func allowedIn(m *Manifest, country string) (bool, error) {
	codes := m.Version.CountryCodes
	switch m.Version.Policy {
	case "", "None":
		return true, nil
	case "Whitelist":
		return slices.Contains(codes, country), nil
	case "Blacklist":
		return !slices.Contains(codes, country), nil
	}
	return false, fmt.Errorf("%s: unknown country policy %q; refusing to guess", m.Name, m.Version.Policy)
}

// parseInterleaved parses flags that may appear before, between or after
// the positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			return pos
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	usage := func() {
		fmt.Fprintf(os.Stderr, "usage: volvo-sensus16-maps list|fetch [flags] [REGION ...]\n")
		os.Exit(2)
	}
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch cmd := os.Args[1]; cmd {
	case "list":
		fs := flag.NewFlagSet("list", flag.ExitOnError)
		dest := fs.String("dest", "", "download directory or USB stick root to compare with (default: find the stick)")
		names := parseInterleaved(fs, os.Args[2:])
		err = listCmd(ctx, *dest, names)
	case "fetch":
		fs := flag.NewFlagSet("fetch", flag.ExitOnError)
		var o fetchOpts
		fs.StringVar(&o.dest, "dest", "", "download directory or USB stick root (default: find the stick)")
		fs.IntVar(&o.workers, "workers", 0,
			fmt.Sprintf("fixed number of parallel chunk downloads, 1-%d (default: adapt to the connection and disk)", maxWorkers))
		fs.StringVar(&o.country, "country", "",
			"two-letter country code for the regions' availability rules (default: from the system locale)")
		fs.BoolVar(&o.verify, "verify", false, "re-read all files of the selected regions from disk and check them")
		fs.BoolVar(&o.noVerify, "no-verify", false, "skip re-reading freshly downloaded files from disk")
		fs.BoolVar(&o.eject, "eject", false, "eject the stick when done")
		fs.Usage = func() {
			var names []string
			for _, r := range loadRegions().regions {
				names = append(names, r.Name)
			}
			fmt.Fprintf(os.Stderr, "usage: volvo-sensus16-maps fetch [flags] [REGION ...]\n\n"+
				"Regions default to those on the stick. Known: %s.\nFor others, %s.\n\n",
				strings.Join(names, ", "), regionHint)
			fs.PrintDefaults()
		}
		o.regions = parseInterleaved(fs, os.Args[2:])
		if o.workers != 0 && (o.workers < 1 || o.workers > maxWorkers) {
			fmt.Fprintf(os.Stderr, "-workers must be between 1 and %d\n", maxWorkers)
			os.Exit(2)
		}
		if o.verify && o.noVerify {
			fmt.Fprintln(os.Stderr, "-verify and -no-verify are mutually exclusive")
			os.Exit(2)
		}
		err = fetchCmd(ctx, o)
	default:
		usage()
	}
	if errors.Is(err, context.Canceled) {
		fmt.Println("\nInterrupted; rerun to resume.")
		os.Exit(130)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "\n"+err.Error())
		os.Exit(1)
	}
}

type fetchOpts struct {
	dest     string
	regions  []string
	workers  int
	country  string
	verify   bool
	noVerify bool
	eject    bool
}

func fetchCmd(ctx context.Context, o fetchOpts) error {
	// An unknown country passes blacklists and fails whitelists, which is
	// what the original client does when its geolocation lookup fails.
	country := o.country
	if country == "" {
		country = systemCountry()
	}

	// Named regions, or else the ones already on the stick.
	book := loadRegions()
	dest := o.dest
	var wanted []regionInfo
	if len(o.regions) > 0 {
		for _, name := range o.regions {
			r, err := book.resolve(ctx, name)
			if err != nil {
				return err
			}
			wanted = append(wanted, r)
		}
	} else {
		if dest == "" {
			var err error
			if dest, err = chooseStick(false, 0); err != nil {
				return err
			}
		}
		var err error
		if wanted, err = regionsOn(ctx, dest, book); err != nil {
			return err
		}
		var names []string
		for _, r := range wanted {
			names = append(names, r.Name)
		}
		fmt.Printf("Updating the regions on the stick: %s\n", strings.Join(names, ", "))
	}

	fmt.Println("Checking latest releases…")
	var selected []region
	for _, r := range wanted {
		m, err := fetchManifest(ctx, r.Package)
		if err != nil {
			return err
		}
		// Only the package-level flag counts; a missing flag means active.
		if m.IsActive != nil && !*m.IsActive {
			return fmt.Errorf("%s: package is disabled on the server", r.Name)
		}
		ok, err := allowedIn(m, country)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s: not available in %s", r.Name, country)
		}
		selected = append(selected, region{r.Name, m})
	}

	if dest == "" {
		var size int64
		for _, r := range selected {
			size += r.m.Size
		}
		var err error
		if dest, err = chooseStick(true, size); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := checkMix(dest, selected); err != nil {
		return err
	}

	var todo []region
	for _, r := range selected {
		if state, _ := localStatus(dest, r.m); state == "current" {
			fmt.Printf("  %-20s %s  already present\n", r.name, r.m.Version.Name)
		} else {
			todo = append(todo, r)
		}
	}

	var downloaded []File
	if len(todo) > 0 {
		// Old releases on the destination are useless (the car already has
		// its maps installed), so they go first to make room -- but only
		// once we know everything will fit.
		var stale []string
		for _, r := range todo {
			stale = append(stale, oldFiles(dest, r.m)...)
		}
		if err := checkSpace(dest, todo, stale, true); err != nil {
			return err
		}
		for _, p := range stale {
			if name := filepath.Base(p); !strings.HasPrefix(name, "._") {
				fmt.Printf("  removing old %s\n", name)
			}
			if err := os.Remove(p); err != nil {
				return err
			}
		}

		nodes, err := fetchNodes(ctx)
		if err != nil {
			return err
		}
		dl := newDownloader(ctx, nodes, newConcurrency(o.workers))
		var total int64
		for _, r := range todo {
			for _, f := range r.m.Files {
				total += f.Size
			}
		}
		dl.total, dl.dest = total, dest
		t0 := time.Now()
		fmt.Println()
		for _, r := range todo {
			dl.st.region, dl.st.fileCount = r.name, len(r.m.Files)
			dl.st.file, dl.st.fileIdx, dl.st.fileDone, dl.st.fileSize = "", 0, 0, 0
			dl.printf("%s: %s release %s (%s)\n", r.name, r.m.Name, r.m.Version.Name, human(float64(r.m.Size)))
			// A map image's DBINFO.ME goes first, as a .part file, and
			// becomes final last (see imageInfo).
			files := slices.Clone(r.m.Files)
			image := isImage(r.m)
			if image {
				slices.SortStableFunc(files, func(a, b File) int {
					return cmp.Compare(boolInt(b.Path == imageInfo), boolInt(a.Path == imageInfo))
				})
			}
			for k, f := range files {
				dl.st.fileIdx = k + 1
				if st, err := os.Stat(filepath.Join(dest, f.Path)); err == nil && st.Size() == f.Size {
					dl.done.Add(f.Size)
					continue
				}
				if err := dl.fetchFile(ctx, r.m, f, dest, image && f.Path == imageInfo); err != nil {
					return err
				}
				downloaded = append(downloaded, f)
			}
			if p := filepath.Join(dest, imageInfo); image {
				if _, err := os.Stat(p + ".part"); err == nil {
					if err := os.Rename(p+".part", p); err != nil {
						return err
					}
				}
			}
		}
		dl.finish()
		fmt.Printf("Downloaded in %s, from %s.\n", fmtDuration(time.Since(t0)), dl.nodeShares())
	} else if !o.verify {
		fmt.Println("Everything is up to date.")
	}

	// Check what actually landed on the stick, not the copy in memory.
	var check []File
	switch {
	case o.verify:
		for _, r := range selected {
			check = append(check, r.m.Files...)
		}
	case !o.noVerify:
		check = downloaded
	}
	if len(check) > 0 {
		var size int64
		for _, f := range check {
			size += f.Size
		}
		fmt.Printf("\nVerifying %s on disk…\n", human(float64(size)))
		if mounted, err := prepareReadBack(dest); err == nil {
			dest = mounted
		} else {
			fmt.Printf("  (%v; the read may be served from cache)\n", err)
		}
		bad, err := verifyOnDisk(ctx, dest, check)
		if err != nil {
			return err
		}
		if bad > 0 {
			return errors.New("some files are damaged on disk; rerun to repair them")
		}
		fmt.Println("All files verified.")
	}

	var ms []*Manifest
	for _, r := range selected {
		ms = append(ms, r.m)
	}
	if err := removeAppleDouble(dest, ms); err != nil {
		return err
	}
	if o.eject {
		if err := eject(dest); err != nil {
			fmt.Printf("Not ejected: %v\n", err)
		}
	}
	return nil
}

// checkMix refuses combinations of maps that can't share a stick: a full
// map image uses fixed file names, so there's room for one, and it's not
// known what the car makes of one next to regional maps.
func checkMix(dest string, selected []region) error {
	var images, regional []string
	for _, r := range selected {
		if isImage(r.m) {
			images = append(images, r.name)
		} else {
			regional = append(regional, r.name)
		}
	}
	stickRegional, stickImage := stickContents(dest)
	switch {
	case len(images) > 1:
		return fmt.Errorf("%s are each a full map image, and a stick holds one; use a stick per map",
			strings.Join(images, " and "))
	case len(images) == 1 && len(regional) > 0:
		return fmt.Errorf("%s is a full map image and needs a stick without regional maps (%s)",
			images[0], strings.Join(regional, ", "))
	case len(images) == 1 && len(stickRegional) > 0:
		return fmt.Errorf("%s is a full map image and needs a stick without regional maps; %s has %s",
			images[0], dest, strings.Join(stickRegional, ", "))
	case len(regional) > 0 && stickImage != "":
		return fmt.Errorf("%s has the full map image %s; regional maps need a stick of their own", dest, stickImage)
	}
	for _, r := range selected {
		if isImage(r.m) && stickImage != "" && !isLatestImage(dest, r.m) {
			fmt.Printf("Replacing the map image %s on %s with %s release %s.\n",
				stickImage, dest, r.m.Name, r.m.Version.Name)
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// identifyImage finds the map image on dest whose DBINFO.ME doesn't name it
// by comparing it with the latest release of each known map image.
func identifyImage(ctx context.Context, dest string, book *regionBook) (regionInfo, bool) {
	for _, r := range book.regions {
		if regionalMap.MatchString(r.Map) {
			continue // a region, not an image
		}
		if m, err := fetchManifest(ctx, r.Package); err == nil && isImage(m) && isLatestImage(dest, m) {
			return r, true
		}
	}
	return regionInfo{}, false
}

// regionsOn returns the regions whose maps are on dest.
func regionsOn(ctx context.Context, dest string, book *regionBook) ([]regionInfo, error) {
	maps := mapsOnStick(dest)
	if len(maps) == 0 {
		return nil, fmt.Errorf("no Sensus maps on %s yet; name the regions to fetch, e.g. "+
			"\"fetch benelux germany\" (%s)", dest, regionHint)
	}
	var found []regionInfo
	var unknown []string
	for _, m := range maps {
		if r, ok := book.byMap(m); ok {
			found = append(found, r)
		} else if r, ok := identifyImage(ctx, dest, book); m == unnamedImage && ok {
			found = append(found, r)
		} else {
			unknown = append(unknown, m)
		}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("don't know which region has the maps %s; name the regions to fetch once (%s)",
			strings.Join(unknown, ", "), regionHint)
	}
	return found, nil
}

func listCmd(ctx context.Context, dest string, names []string) error {
	if dest == "" {
		// Without a stick, just list the releases.
		if d, err := chooseStick(false, 0); err == nil {
			dest = d
		} else {
			fmt.Printf("(%v)\n\n", err)
		}
	}
	haveDest := dest != ""
	if haveDest {
		if st, err := os.Stat(dest); err != nil || !st.IsDir() {
			return fmt.Errorf("%s: not a directory", dest)
		}
	}

	// The named regions, or else all known ones; maps on the stick that
	// belong to no known region are listed too.
	book := loadRegions()
	shown := book.regions
	if len(names) > 0 {
		shown = nil
		for _, name := range names {
			r, err := book.resolve(ctx, name)
			if err != nil {
				return err
			}
			shown = append(shown, r)
		}
	}
	var onStick []string
	if haveDest {
		onStick = mapsOnStick(dest)
	}

	// There are many regions: fetch their manifests in parallel.
	ms := make([]*Manifest, len(shown))
	errs := make([]error, len(shown))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, r := range shown {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ms[i], errs[i] = fetchManifest(ctx, r.Package)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	var todo []region // what a plain fetch would update
	unnamedKnown := false
	color := useColor()
	nameWidth := len("region")
	for _, r := range shown {
		nameWidth = max(nameWidth, len(r.Name))
	}
	header := fmt.Sprintf("  %-*s %-24s %7s %9s", nameWidth, "region", "maps", "release", "size")
	if haveDest {
		header += "  on " + dest
	}
	fmt.Println(header)
	market := "-"
	for i, r := range shown {
		m := ms[i]
		if len(names) == 0 && r.Market != market {
			market = r.Market
			label := market
			if label == "" {
				label = "Looked up"
			}
			fmt.Printf("\n%s\n", paintIf(color, ansiBold, label))
		}
		where, onIt := "", false
		if haveDest {
			state, detail := localStatus(dest, m)
			latest := isImage(m) && isLatestImage(dest, m)
			unnamedKnown = unnamedKnown || latest
			onIt = state != "missing"
			if state != "current" && (slices.Contains(onStick, m.Name) || latest) {
				todo = append(todo, region{r.Name, m})
			}
			// Colour the state; the maps on the stick stand out, the rest
			// is dimmed.
			stateColor := map[string]string{"current": ansiGreen, "partial": ansiYellow, "old": ansiYellow}[state]
			where = strings.TrimSpace(paintIf(color && onIt, stateColor, state) + " " + detail)
		}
		row := fmt.Sprintf("  %-*s %-24s %7s %9s", nameWidth, r.Name, m.Name, m.Version.Name, human(float64(m.Size)))
		if haveDest && !onIt {
			fmt.Println(paintIf(color, ansiDim, strings.TrimRight(row+"  "+where, " ")))
		} else {
			fmt.Println(strings.TrimRight(row+"  "+where, " "))
		}
	}
	for _, m := range onStick {
		if _, ok := book.byMap(m); !ok && !(m == unnamedImage && unnamedKnown) {
			fmt.Println(paintIf(color, ansiRed, fmt.Sprintf("  %-*s %-24s %7s %9s  on the stick; region name unknown", nameWidth, "?", m, "", "")))
		}
	}
	if len(names) == 0 {
		fmt.Printf("\n%s\n", paintIf(color, ansiDim, "Not supported: "+strings.Join(slices.Sorted(maps.Keys(unsupportedRegions)), ", ")+"."))
	}
	if !haveDest {
		return nil
	}
	fmt.Println()
	if len(todo) == 0 {
		fmt.Println(paintIf(color, ansiGreen, "The regions on the stick are up to date."))
		return nil
	}
	var stale []string
	for _, r := range todo {
		stale = append(stale, oldFiles(dest, r.m)...)
	}
	return checkSpace(dest, todo, stale, false)
}
