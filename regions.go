package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"
)

// A region is named like the last part of its download link on Volvo's
// "Sensus Navigation 2016 map download" pages, which point to
// https://sensusupdate.volvocars.com/{osx,windows}/spa/<name>. Behind each
// is a small downloader app that knows just one package ID.
type regionInfo struct {
	Name    string `json:"name"`             // e.g. benelux
	Market  string `json:"market,omitempty"` // e.g. Europe
	Package string `json:"package"`          // package ID in Reloaded's network
	Map     string `json:"map"`              // the manifest's name, e.g. EU_UR146_BE-LU-NL
}

// builtinRegions is everything on Volvo's map download pages (read
// October 2026, release 1560): per market a full map image and, mostly,
// regions to pick from. Any other region is looked up once and remembered
// (see lookUpRegion).
var builtinRegions = []regionInfo{
	// Africa & Middle East
	{"africa-and-middle-east", "Africa & Middle East", "546a11ca9bafa20bbc69e9d4", "AFME_901"},
	{"arabia", "Africa & Middle East", "54d34ac2ef91100688842f94", "AFME_UR197_AE-BH-KW-SA"},
	{"lebanon", "Africa & Middle East", "54d34b1def91100688842f96", "AFME_UR198_LB"},
	{"southern-africa", "Africa & Middle East", "54d336d7ef91100688842f90", "AFME_UR191_ZA"},
	{"central-africa", "Africa & Middle East", "54d349619bafa20b24cf5102", "AFME_UR192_KE"},
	{"egypt", "Africa & Middle East", "54d34a78ef91100688842f92", "AFME_UR196_EG"},
	{"north-western-africa", "Africa & Middle East", "54d349989bafa20b24cf5104", "AFME_UR194_DZ-MA-TN"},

	// Europe
	{"europe", "Europe", "546a1287ef91100850d3e1c3", "EU_904"},
	{"austria-switzerland", "Europe", "54d3159cef91100688842f8a", "EU_UR170_AT-CH-LI"},
	{"eu-turkey", "Europe", "561249299bafa2026c00b852", "EU_UR264_TR"},
	{"kazakhstan", "Europe", "54d0e74b9bafa209404b32df", "EU_UR172_KZ"},
	{"scandinavia", "Europe", "54d0cc379bafa209404b32d7", "EU_UR153_DK-NO-SE"},
	{"italy", "Europe", "54d0cc169bafa209404b32d5", "EU_UR151_IT-MA"},
	{"benelux", "Europe", "54d0c8ef9bafa209404b32cd", "EU_UR146_BE-LU-NL"},
	{"spain-portugal", "Europe", "54d0cc619bafa209404b32d9", "EU_UR154_ES-PT"},
	{"south-eastern-europe", "Europe", "54d316579bafa20b24cf50f0", "EU_UR145_BG-GR-RO-RS"},
	{"finland", "Europe", "54d0ccb69bafa209404b32dd", "EU_UR200_FI"},
	{"baltic", "Europe", "54d0cc889bafa209404b32db", "EU_UR171_EE-LT-LV"},
	{"germany", "Europe", "54d0c8b49bafa209404b32cb", "EU_UR142_DE"},
	{"north-west-europe", "Europe", "54d316c49bafa20b24cf50f2", "EU_UR150_GB-IE-IS"},
	{"central-europe", "Europe", "54d0c91e9bafa209404b32cf", "EU_UR147_CZ-HU-PL-SK"},
	{"eastern-europe", "Europe", "54d0c9559bafa209404b32d1", "EU_UR148_BY-MD-UA"},
	{"france", "Europe", "54d0c9839bafa209404b32d3", "EU_UR149_AD-FR-MC"},
	// Not listed on Volvo's sites, but served like the other regions.
	{"russia", "Europe", "561249039bafa2026c00b850", "EU_UR152_RU"},

	// India
	{"india", "India", "546a12f59bafa20bbc69e9d6", "IND_905"},
	{"central-east-india", "India", "54d37c35ef91100688842f9e", "IND_UR239_OR-CT-JH-BR"},
	{"south-west-india", "India", "54d37be9ef91100688842f9a", "IND_UR237_KA-KL-LD"},
	{"uttar-pradesh", "India", "54d37afa9bafa20b205d5f50", "IND_UR233_UP"},
	{"western-india", "India", "54d37b389bafa20b205d5f52", "IND_UR234_DD-DN-GA-GJ"},
	{"northern-india", "India", "54d37add9bafa20b205d5f4e", "IND_UR232_CH-DL-HR-HP"},
	{"central-west-india", "India", "54d37b4f9bafa20b205d5f54", "IND_UR235_MP-RJ"},
	{"south-east-india", "India", "54d37bc1ef91100688842f98", "IND_UR236_AN-AP-PY-TN"},
	{"eastern-india", "India", "54d37c01ef91100688842f9c", "IND_UR238_AR-AS-MN-ML"},

	// Israel
	{"israel", "Israel", "54d0c458ef91100020d286d8", "ISR_906"},

	// North America
	{"north-america", "North America", "546a13059bafa20bbc69e9d8", "NA_907"},
	{"na-florida-carribean-fl-pr-vi", "North America", "54d37e6b9bafa20b205d5f6a", "NA_UR256_BS-PR-FL"},
	{"na-west-canada-ak", "North America", "54d37d689bafa20b205d5f58", "NA_UR247_AB-BC-MB-NT"},
	{"na-south-al-ar-la-ms", "North America", "54d37cbcef91100688842fa6", "NA_UR243_AL-AR-LA-MS"},
	{"na-east-canada", "North America", "54d37d529bafa20b205d5f56", "NA_UR246_ON-QC-NB-NL"},
	{"na-eastern-midwest-mi-in", "North America", "54d37df29bafa20b205d5f62", "NA_UR252_IN-MI"},
	{"na-texas-tx", "North America", "54d37ccfef91100688842fa8", "NA_UR244_TX"},
	{"na-midwest-ia-ks-mn-mo-nd-ne-ok-sd", "North America", "54d37e119bafa20b205d5f64", "NA_UR253_IA-KS-MN-MO"},
	{"na-central-midwest-il-wi", "North America", "54d37dda9bafa20b205d5f60", "NA_UR251_IL-WI"},
	{"na-southern-california-ca", "North America", "54d37c78ef91100688842fa0", "NA_UR240_CA"},
	{"na-new-england-ct-ma-me-nh-ri-vt", "North America", "54d37d889bafa20b205d5f5a", "NA_UR248_CT-MA-ME-NH"},
	{"na-southern-atlantic-ga-sc-nc", "North America", "54d37e459bafa20b205d5f68", "NA_UR255_GA-NC-SC"},
	{"central-america", "North America", "54d37ee89bafa20b205d5f72", "NA_UR262_CR-KY-PA"},
	{"na-pacific-ca-hi-or-wa", "North America", "54d37c8fef91100688842fa2", "NA_UR241_HI-OR-WA"},
	{"na-mountain-az-co-id-nm-nv-mt-ut-wy", "North America", "54d37ca5ef91100688842fa4", "NA_UR242_AZ-CO-ID-MT"},
	{"mexico-central", "North America", "54d37e899bafa20b205d5f6c", "NA_UR257_AG-CL-DF-GT"},
	{"na-upper-mid-atlantic-nj-pa", "North America", "54d37db69bafa20b205d5f5e", "NA_UR250_NJ-PA"},
	{"na-upper-south-and-ohio-ky-oh-tn", "North America", "54d37ce9ef91100688842faa", "NA_UR245_KY-OH-TN"},
	{"mexico-south", "North America", "54d37ecf9bafa20b205d5f70", "NA_UR259_CM-CP-GR-OA"},
	{"na-new-york", "North America", "54d37d9e9bafa20b205d5f5c", "NA_UR249_NY"},
	{"mexico-north", "North America", "54d37ea39bafa20b205d5f6e", "NA_UR258_BN-BS-CH-CA"},
	{"na-mid-atlantic-dc-de-md-va-wv", "North America", "54d37e2c9bafa20b205d5f66", "NA_UR254_DE-DC-MD-WV"},

	// Pacific
	{"pacific", "Pacific", "546a13319bafa20bbc69e9da", "PA_908"},

	// South America
	{"south-america", "South America", "54d0c5aa9bafa209404b32c3", "SA_910"},
	{"south-central-america", "South America", "54d38019ef91100688842fb0", "SA_UR176_CO-PE-VE"},
	{"brazil-northeast", "South America", "54d38087ef91100688842fb4", "SA_UR205_AL-BA-CE-MA"},
	{"argentina", "South America", "54d37f69ef91100688842fac", "SA_UR173_AR"},
	{"brazil-centralwest-north", "South America", "54d38073ef91100688842fb2", "SA_UR204_AC-AP-AM-DF"},
	{"chile-paraguay", "South America", "54d37fcfef91100688842fae", "SA_UR175_CL-PY"},
	{"brazil-south-uruguay", "South America", "54d380a3ef91100688842fb6", "SA_UR206_PR-RS-SC-UY"},
	{"brazil-southeast", "South America", "54d381249bafa20b205d5f74", "SA_UR207_ES-MG-RJ-SP"},

	// South-East Asia
	{"south-east-asia", "South-East Asia", "54d0c7889bafa209404b32c5", "SEA_911"},
	{"malaysia-singapore", "South-East Asia", "55421d829bafa20facb9bcea", "SEA_UR201_MY-SG"},
	{"indonesia-philipines", "South-East Asia", "55422a5f9bafa20facb9bcee", "SEA_UR203_ID-PH"},
	{"thailand-vietnam-brunei", "South-East Asia", "55421ec19bafa20facb9bcec", "SEA_UR202_BN-TH-VN"},

	// Taiwan
	{"taiwan", "Taiwan", "54d0c81c9bafa209404b32c9", "TW_913"},

	// Turkey
	{"turkey", "Turkey", "54d0c7d09bafa209404b32c7", "TK_914"},

	// Japan
	{"japan", "Japan", "56409fdeef91100b9c3ac7e5", "SPA_JP"},
}

// unsupportedRegions are served by Volvo's own servers for that country,
// under another partner, which refuse other countries.
var unsupportedRegions = map[string]string{
	"korea": "Korea's maps come from Volvo's Korean servers, which serve only Korea",
	"china": "China's maps come from Volvo's Chinese servers and may only be downloaded in China and Hong Kong",
}

const downloaderURL = "https://sensusupdate.volvocars.com/windows/spa/"

const regionHint = "use the last part of a download link on Volvo's \"Sensus Navigation 2016 map download\" pages, " +
	"e.g. benelux for .../spa/benelux"

// regionBook is the built-in regions plus those looked up before.
type regionBook struct {
	regions []regionInfo
	path    string // where looked-up regions are remembered
}

func loadRegions() *regionBook {
	b := &regionBook{regions: slices.Clone(builtinRegions)}
	if dir, err := os.UserConfigDir(); err == nil {
		b.path = filepath.Join(dir, "volvo-sensus16-maps", "regions.json")
		var saved []regionInfo
		if data, err := os.ReadFile(b.path); err == nil && json.Unmarshal(data, &saved) == nil {
			for _, r := range saved {
				if _, ok := b.byName(r.Name); !ok {
					b.regions = append(b.regions, r)
				}
			}
		}
	}
	return b
}

func (b *regionBook) byName(name string) (regionInfo, bool) {
	for _, r := range b.regions {
		if r.Name == name {
			return r, true
		}
	}
	return regionInfo{}, false
}

func (b *regionBook) byMap(m string) (regionInfo, bool) {
	for _, r := range b.regions {
		if r.Map == m {
			return r, true
		}
	}
	return regionInfo{}, false
}

// resolve finds a region by name, looking it up on Volvo's download server
// if it's new, and remembering it.
func (b *regionBook) resolve(ctx context.Context, name string) (regionInfo, error) {
	if r, ok := b.byName(name); ok {
		return r, nil
	}
	if why, ok := unsupportedRegions[name]; ok {
		return regionInfo{}, fmt.Errorf("%s: not supported; %s", name, why)
	}
	fmt.Printf("Looking up region %s…\n", name)
	r, err := lookUpRegion(ctx, name)
	if err != nil {
		return r, err
	}
	b.regions = append(b.regions, r)
	if err := b.save(); err != nil {
		fmt.Printf("  (can't remember it: %v)\n", err)
	}
	return r, nil
}

func (b *regionBook) save() error {
	if b.path == "" {
		return errors.New("no configuration directory")
	}
	var looked []regionInfo
	for _, r := range b.regions {
		if !slices.Contains(builtinRegions, r) {
			looked = append(looked, r)
		}
	}
	data, err := json.MarshalIndent(looked, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(b.path, append(data, '\n'), 0o644)
}

// lookUpRegion finds the package ID of a region from its Windows
// downloader, which has it as UTF-16 text (the macOS one only inside a
// disk image). Besides the region's package, the downloader names Volvo's
// partner ID and its own app packages; the one whose manifest lists a map
// database (DBINFO) is the region.
func lookUpRegion(ctx context.Context, name string) (regionInfo, error) {
	if !regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(name) {
		return regionInfo{}, fmt.Errorf("%q isn't a region name; %s", name, regionHint)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", downloaderURL+name, nil)
	if err != nil {
		return regionInfo{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := metaClient.Do(req)
	if err != nil {
		return regionInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 || resp.StatusCode == 404 {
		return regionInfo{}, fmt.Errorf("no region %q at Volvo; %s", name, regionHint)
	}
	if resp.StatusCode != 200 {
		return regionInfo{}, fmt.Errorf("%s: HTTP %d", req.URL, resp.StatusCode)
	}
	exe, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return regionInfo{}, err
	}
	var found []regionInfo
	for _, id := range packageIDs(exe) {
		if id == partner {
			continue
		}
		m, err := fetchManifest(ctx, id)
		if err != nil {
			continue // not a package (or not ours)
		}
		for _, f := range m.Files {
			if strings.Contains(f.Path, "DBINFO") {
				if m.PartnerID != partner {
					return regionInfo{}, fmt.Errorf("%s: not supported; its maps come from other servers than Volvo's usual ones", name)
				}
				found = append(found, regionInfo{Name: name, Package: id, Map: m.Name})
				break
			}
		}
	}
	if len(found) != 1 {
		return regionInfo{}, fmt.Errorf("can't tell which package the %s downloader fetches (found %d map packages)", name, len(found))
	}
	return found[0], nil
}

// packageIDs finds the 24-hex-digit IDs written as UTF-16 text in b.
func packageIDs(b []byte) []string {
	re := regexp.MustCompile(`(?:[0-9a-f]\x00){24}`)
	var ids []string
	for _, m := range re.FindAll(b, -1) {
		u := make([]uint16, len(m)/2)
		for i := range u {
			u[i] = uint16(m[2*i]) | uint16(m[2*i+1])<<8
		}
		if id := string(utf16.Decode(u)); !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// mapsOnStick lists the names of the Sensus maps in dir, e.g.
// EU_UR146_BE-LU-NL, in any release and also partly downloaded.
func mapsOnStick(dir string) []string {
	regional, image := stickContents(dir)
	if image != "" {
		return append(regional, image)
	}
	return regional
}

// stickContents lists the regional maps in dir, and names the full map
// image there, if any.
func stickContents(dir string) (regional []string, image string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if m := mapFileName.FindStringSubmatch(e.Name()); m != nil && !slices.Contains(regional, m[1]) {
			regional = append(regional, m[1])
		}
	}
	slices.Sort(regional)
	if info := readImageInfo(dir); info.present {
		image = info.name
		if image == "" {
			image = unnamedImage
		}
	}
	return regional, image
}

// Whole markets (north-america, turkey, all of europe, ...) come as one
// map image with fixed file names instead of regions with the release in
// theirs. Only one fits on a stick, and DBINFO.ME says which it is:
//
//	MapVersion=TK_914_1560
//
// It's downloaded first but kept as DBINFO.ME.part until all other files
// are in, so a half-done image can be recognised but not taken for done.
const imageInfo = "DBINFO.ME"

var (
	imageFile      = regexp.MustCompile(`^(\._)?(CHECKSUM|DBINFO\.ME|MapImageSPA\.\d+|MelcoMapUpdateMain_SPA\.dat)(\.part)?$`)
	mapVersionLine = regexp.MustCompile(`(?m)^MapVersion=(.+)_(\d+)\s*$`)
)

func isImage(m *Manifest) bool {
	for _, f := range m.Files {
		if f.Path == imageInfo {
			return true
		}
	}
	return false
}

// unnamedImage stands for a map image whose DBINFO.ME doesn't say which
// map it is: Japan's has no MapVersion line.
const unnamedImage = "(unnamed map image)"

// imageInfoFile is the DBINFO.ME (or, while it's being downloaded,
// DBINFO.ME.part) in a directory.
type imageInfoFile struct {
	present       bool
	name, release string // from its MapVersion line, if it has one
	md5           string
}

func readImageInfo(dir string) imageInfoFile {
	for _, p := range []string{imageInfo, imageInfo + ".part"} {
		data, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil || len(data) > 1<<20 {
			continue
		}
		sum := md5.Sum(data)
		info := imageInfoFile{present: true, md5: hex.EncodeToString(sum[:])}
		if m := mapVersionLine.FindSubmatch(data); m != nil {
			info.name, info.release = string(m[1]), string(m[2])
		}
		return info
	}
	return imageInfoFile{}
}

// isLatestImage says whether dir holds (possibly still downloading) the
// release of map image m: its DBINFO.ME matches the manifest's.
func isLatestImage(dir string, m *Manifest) bool {
	info := readImageInfo(dir)
	for _, f := range m.Files {
		if f.Path == imageInfo {
			return info.present && strings.EqualFold(info.md5, f.MD5)
		}
	}
	return false
}

// regionalMap matches the names of regional maps (rather than images).
var regionalMap = regexp.MustCompile(`^[A-Z]+_UR\d+_`)

// mapFileName matches the files of a map and captures the map's name: e.g.
// EU_UR142_DE from EU_UR142_DE_1560_of_02.zip.
var mapFileName = regexp.MustCompile(`^([A-Z]+_UR\d+_.+?)_\d+(?:_of_\d+)?\.(?:zip|DBINFO)(?:\.part)?$`)
