package anymd

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"math"
	"strconv"
	"strings"

	// Registered so image.DecodeConfig can read the headers of the three
	// formats the standard library ships. Anything else (webp, tiff, bmp)
	// degrades to "no dimensions" rather than pulling in a new dependency.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	exif "github.com/dsoprea/go-exif/v3"
	exifcommon "github.com/dsoprea/go-exif/v3/common"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&ImageConverter{}) }

// maxImageBytes bounds how much of an image stream we will buffer. Only the
// header is decoded, but EXIF may sit anywhere in the file, so the search needs
// the bytes; the cap keeps a hostile file from exhausting memory.
const maxImageBytes = 64 << 20

// ImageConverter renders what can be recovered from an image *losslessly*:
// its dimensions and its EXIF metadata.
//
// There is deliberately no OCR and no captioning. anymd is a pure-Go library
// with no model and no native dependency, so inventing a description of the
// pixels is out of scope. What is in scope is the metadata — capture time,
// camera, lens, exposure, GPS, description, rights — which is genuine,
// verifiable text and is often the only part of an image a retrieval index can
// meaningfully match on.
type ImageConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *ImageConverter) Name() string { return "image" }

// Accepts recognizes an image by magic bytes first, then by mime, then by
// extension.
func (c *ImageConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	var head [16]byte
	n, _ := io.ReadFull(r, head[:])
	if imageMagic(head[:n]) != "" {
		return true
	}
	if info.HasMimePrefix("image/") {
		return true
	}
	return info.HasExt(".jpg", ".jpeg", ".png", ".gif", ".webp", ".tiff", ".tif", ".bmp")
}

// imageMagic returns a short format name for the magic bytes at the head of an
// image, or "" if none matched.
func imageMagic(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpeg"
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp"
	}
	return ""
}

// Convert emits the image placeholder, its dimensions, and its EXIF table.
func (c *ImageConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (res Result, err error) {
	defer func() {
		// The EXIF parser walks attacker-controlled offsets; a panic there is
		// an error about this file, not a reason to take the process down.
		if p := recover(); p != nil {
			res = Result{}
			err = fmt.Errorf("malformed image metadata: %v", p)
		}
	}()

	b, err := io.ReadAll(io.LimitReader(r, maxImageBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(b) > maxImageBytes {
		return Result{}, fmt.Errorf("image too large: over %d bytes", maxImageBytes)
	}

	name := info.FileName
	if name == "" {
		name = "image"
	}

	blocks := []string{imagePlaceholder(name)}

	if facts := imageFacts(b); facts != "" {
		blocks = append(blocks, facts)
	}
	if table := exifTable(b); table != "" {
		blocks = append(blocks, table)
	}
	return Result{Markdown: mdutil.Join(blocks...)}, nil
}

// imagePlaceholder emits `![name]()` so the image keeps its position when this
// converter runs inside a container (a zip, an epub, a .msg attachment): the
// surrounding text still reads in the right order with a visible hole where the
// picture was.
func imagePlaceholder(name string) string {
	alt := strings.NewReplacer("[", `\[`, "]", `\]`, "\n", " ", "\r", " ").Replace(name)
	return "![" + alt + "]()"
}

// imageFacts renders the header-only facts: pixel dimensions and the decoded
// format. DecodeConfig reads the header alone, never the pixel data, so this
// stays cheap and cannot be turned into a decompression bomb.
func imageFacts(b []byte) string {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		// webp/tiff/bmp are not registered in the standard library, and a
		// truncated header is normal enough. Degrade to no dimensions.
		if f := imageMagic(b); f != "" {
			return "- Format: " + f
		}
		return ""
	}
	return fmt.Sprintf("- Dimensions: %d × %d\n- Format: %s", cfg.Width, cfg.Height, format)
}

// exifField is one row of the rendered EXIF table: the label we print and the
// EXIF tag names that can supply it.
type exifField struct {
	label string
	tags  []string
}

// exifFields is the fixed set of human-meaningful tags, in a fixed order so the
// same photo always renders byte-identically. The full EXIF dictionary is
// hundreds of machine-only tags; dumping it would bury the useful ones.
var exifFields = []exifField{
	{"DateTimeOriginal", []string{"DateTimeOriginal", "DateTime"}},
	{"Make", []string{"Make"}},
	{"Model", []string{"Model"}},
	{"LensModel", []string{"LensModel"}},
	{"FNumber", []string{"FNumber"}},
	{"ExposureTime", []string{"ExposureTime"}},
	{"ISO", []string{"ISOSpeedRatings", "PhotographicSensitivity", "ISOSpeed"}},
	{"FocalLength", []string{"FocalLength"}},
	{"GPS Latitude", nil},
	{"GPS Longitude", nil},
	{"ImageDescription", []string{"ImageDescription"}},
	{"Artist", []string{"Artist"}},
	{"Copyright", []string{"Copyright"}},
}

// exifTable renders the EXIF block, or "" when the image carries no EXIF or
// none of the tags we care about. Missing or corrupt EXIF is the normal case
// for most images and is never an error.
func exifTable(b []byte) string {
	raw, err := exif.SearchAndExtractExif(b)
	if err != nil || len(raw) == 0 {
		return ""
	}
	tags, _, err := exif.GetFlatExifData(raw, nil)
	if err != nil && len(tags) == 0 {
		return ""
	}

	byName := make(map[string]exif.ExifTag, len(tags))
	for _, t := range tags {
		if _, seen := byName[t.TagName]; !seen {
			byName[t.TagName] = t
		}
	}

	var rows [][]string
	for _, f := range exifFields {
		var val string
		switch f.label {
		case "GPS Latitude":
			val = gpsDegrees(byName, "GPSLatitude", "GPSLatitudeRef", "S")
		case "GPS Longitude":
			val = gpsDegrees(byName, "GPSLongitude", "GPSLongitudeRef", "W")
		default:
			for _, name := range f.tags {
				if t, ok := byName[name]; ok {
					if val = exifValue(f.label, t); val != "" {
						break
					}
				}
			}
		}
		if val != "" {
			rows = append(rows, []string{f.label, val})
		}
	}
	if len(rows) == 0 {
		return ""
	}
	return mdutil.Table([]string{"Tag", "Value"}, rows)
}

// exifValue formats one tag for display, giving the photographic tags the
// notation a photographer would recognize (f/2.8, 1/125 s, 50 mm) and falling
// back to the library's own formatting for everything else.
func exifValue(label string, t exif.ExifTag) string {
	switch label {
	case "FNumber":
		if v, ok := exifRational(t); ok {
			return "f/" + trimFloat(v)
		}
	case "ExposureTime":
		// Shutter speeds are stored as an exact fraction (1/125). Divide the
		// stored numerator and denominator, never the evaluated float, or
		// 1/0.008 prints as 125.00000000000001.
		if num, den, ok := exifRationalND(t); ok {
			if num > 0 && num < den {
				return "1/" + trimFloat(den/num) + " s"
			}
			return trimFloat(num/den) + " s"
		}
	case "FocalLength":
		if v, ok := exifRational(t); ok {
			return trimFloat(v) + " mm"
		}
	}
	return strings.TrimSpace(mdutil.Collapse(strings.Trim(t.Formatted, "\x00")))
}

// exifRationalND pulls the first rational out of a tag value without
// evaluating it, so exact fractions stay exact.
func exifRationalND(t exif.ExifTag) (num, den float64, ok bool) {
	switch v := t.Value.(type) {
	case []exifcommon.Rational:
		if len(v) > 0 && v[0].Denominator != 0 {
			return float64(v[0].Numerator), float64(v[0].Denominator), true
		}
	case []exifcommon.SignedRational:
		if len(v) > 0 && v[0].Denominator != 0 {
			return float64(v[0].Numerator), float64(v[0].Denominator), true
		}
	}
	return 0, 0, false
}

// exifRational pulls the first rational out of a tag value. EXIF stores every
// value as an array, so a scalar is an array of one.
func exifRational(t exif.ExifTag) (float64, bool) {
	switch v := t.Value.(type) {
	case []exifcommon.Rational:
		if len(v) > 0 && v[0].Denominator != 0 {
			return float64(v[0].Numerator) / float64(v[0].Denominator), true
		}
	case []exifcommon.SignedRational:
		if len(v) > 0 && v[0].Denominator != 0 {
			return float64(v[0].Numerator) / float64(v[0].Denominator), true
		}
	}
	return 0, false
}

// gpsDegrees converts EXIF's degrees/minutes/seconds triple plus its N/S/E/W
// reference into signed decimal degrees, which is what every mapping and
// retrieval system actually wants.
func gpsDegrees(byName map[string]exif.ExifTag, tag, refTag, negRef string) string {
	t, ok := byName[tag]
	if !ok {
		return ""
	}
	dms, ok := t.Value.([]exifcommon.Rational)
	if !ok || len(dms) < 3 {
		return ""
	}
	var deg float64
	for i, scale := range []float64{1, 60, 3600} {
		if dms[i].Denominator == 0 {
			return ""
		}
		deg += float64(dms[i].Numerator) / float64(dms[i].Denominator) / scale
	}
	if math.IsNaN(deg) || math.IsInf(deg, 0) {
		return ""
	}
	ref := strings.ToUpper(strings.TrimSpace(strings.Trim(fmt.Sprint(valueOf(byName[refTag])), "\x00")))
	if strings.HasPrefix(ref, negRef) {
		deg = -deg
	}
	return strconv.FormatFloat(deg, 'f', 6, 64)
}

// valueOf returns a tag's formatted text, tolerating a missing tag.
func valueOf(t exif.ExifTag) string { return t.Formatted }

// trimFloat prints a float without trailing zeros, so 2.8 stays "2.8" and 50
// stays "50" rather than "50.000000".
func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
