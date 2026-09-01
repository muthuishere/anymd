package anymd

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// pngBytes encodes a real PNG of the given size, header and all.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// jpegBytes encodes a real JPEG of the given size.
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// exifJPEG splices a hand-built APP1/Exif segment into a real JPEG, right after
// the SOI marker where a camera would put it. The TIFF block below is written
// byte by byte so the test asserts against input it fully controls.
func exifJPEG(t *testing.T, w, h int) []byte {
	t.Helper()

	le := binary.LittleEndian
	tiff := make([]byte, 134)
	copy(tiff[0:], "II")
	le.PutUint16(tiff[2:], 0x002A)
	le.PutUint32(tiff[4:], 8) // offset of IFD0

	entry := func(off int, tag uint16, typ uint16, count uint32, value func([]byte)) {
		le.PutUint16(tiff[off:], tag)
		le.PutUint16(tiff[off+2:], typ)
		le.PutUint32(tiff[off+4:], count)
		value(tiff[off+8 : off+12])
	}
	inlineU32 := func(v uint32) func([]byte) {
		return func(b []byte) { le.PutUint32(b, v) }
	}
	rational := func(off int, num, den uint32) {
		le.PutUint32(tiff[off:], num)
		le.PutUint32(tiff[off+4:], den)
	}

	// IFD0: Make, Artist, and the pointer to the Exif sub-IFD.
	le.PutUint16(tiff[8:], 3)
	entry(10, 0x010F, 2, 6, inlineU32(50))                         // Make -> "Canon\0"
	entry(22, 0x013B, 2, 4, func(b []byte) { copy(b, "Ada\x00") }) // Artist, inline
	entry(34, 0x8769, 4, 1, inlineU32(56))                         // ExifIFDPointer
	le.PutUint32(tiff[46:], 0)                                     // no IFD1
	copy(tiff[50:], "Canon\x00")

	// Exif sub-IFD, tags in ascending id order as the spec requires.
	le.PutUint16(tiff[56:], 4)
	entry(58, 0x829A, 5, 1, inlineU32(110)) // ExposureTime
	entry(70, 0x829D, 5, 1, inlineU32(118)) // FNumber
	entry(82, 0x8827, 3, 1, inlineU32(400)) // ISOSpeedRatings, inline SHORT
	entry(94, 0x920A, 5, 1, inlineU32(126)) // FocalLength
	le.PutUint32(tiff[106:], 0)
	rational(110, 1, 125)
	rational(118, 28, 10)
	rational(126, 50, 1)

	app1 := make([]byte, 0, 4+6+len(tiff))
	app1 = append(app1, 0xFF, 0xE1)
	app1 = binary.BigEndian.AppendUint16(app1, uint16(2+6+len(tiff)))
	app1 = append(app1, "Exif\x00\x00"...)
	app1 = append(app1, tiff...)

	base := jpegBytes(t, w, h)
	out := make([]byte, 0, len(base)+len(app1))
	out = append(out, base[:2]...) // SOI
	out = append(out, app1...)
	return append(out, base[2:]...)
}

func TestImageConverterAccepts(t *testing.T) {
	c := &ImageConverter{}
	cases := []struct {
		name string
		body []byte
		info StreamInfo
		want bool
	}{
		{"png magic", pngBytes(t, 2, 2), StreamInfo{}, true},
		{"jpeg magic", jpegBytes(t, 2, 2), StreamInfo{}, true},
		{"gif magic", []byte("GIF89a\x01\x00\x01\x00"), StreamInfo{}, true},
		{"webp magic", []byte("RIFF\x10\x00\x00\x00WEBPVP8 "), StreamInfo{}, true},
		{"mime only", []byte("\x00\x01"), StreamInfo{MimeType: "image/tiff"}, true},
		{"extension only", []byte("\x00\x01"), StreamInfo{Extension: ".bmp"}, true},
		{"text", []byte("hello"), StreamInfo{Extension: ".txt"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Accepts(bytes.NewReader(tc.body), tc.info, &Options{}); got != tc.want {
				t.Fatalf("Accepts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestImageConvertPNGNoExif(t *testing.T) {
	res, err := (&ImageConverter{}).Convert(
		bytes.NewReader(pngBytes(t, 3, 2)),
		StreamInfo{FileName: "chart.png", Extension: ".png"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "![chart.png]()\n\n- Dimensions: 3 × 2\n- Format: png\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestImageConvertJPEGWithExif(t *testing.T) {
	res, err := (&ImageConverter{}).Convert(
		bytes.NewReader(exifJPEG(t, 8, 4)),
		StreamInfo{FileName: "shot.jpg", Extension: ".jpg"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "![shot.jpg]()\n\n" +
		"- Dimensions: 8 × 4\n- Format: jpeg\n\n" +
		"| Tag | Value |\n" +
		"| --- | --- |\n" +
		"| Make | Canon |\n" +
		"| FNumber | f/2.8 |\n" +
		"| ExposureTime | 1/125 s |\n" +
		"| ISO | 400 |\n" +
		"| FocalLength | 50 mm |\n" +
		"| Artist | Ada |\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestImageCorruptExifIsNotAnError(t *testing.T) {
	// An "Exif\0\0" header followed by nonsense is exactly what a truncated
	// upload looks like. It must degrade to no table, never to an error.
	good := exifJPEG(t, 4, 4)
	broken := bytes.Replace(good, []byte("II\x2a\x00"), []byte("II\x2a\x00\xff\xff\xff\xff"), 1)

	res, err := (&ImageConverter{}).Convert(
		bytes.NewReader(broken), StreamInfo{FileName: "broken.jpg"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if bytes.Contains([]byte(res.Markdown), []byte("| Tag |")) {
		t.Fatalf("expected no EXIF table, got:\n%s", res.Markdown)
	}
	if !bytes.HasPrefix([]byte(res.Markdown), []byte("![broken.jpg]()")) {
		t.Fatalf("placeholder missing:\n%s", res.Markdown)
	}
}

func TestImageUnknownFormatDegrades(t *testing.T) {
	// WEBP is not decodable by the standard library, so we keep the placeholder
	// and the format name and drop the dimensions rather than failing.
	res, err := (&ImageConverter{}).Convert(
		bytes.NewReader([]byte("RIFF\x10\x00\x00\x00WEBPVP8 \x00\x00\x00\x00")),
		StreamInfo{FileName: "logo.webp", Extension: ".webp"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "![logo.webp]()\n\n- Format: webp\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestImageNoFileNameStillPlaceholds(t *testing.T) {
	res, err := (&ImageConverter{}).Convert(bytes.NewReader(pngBytes(t, 1, 1)), StreamInfo{}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "![image]()\n\n- Dimensions: 1 × 1\n- Format: png\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

// gpsJPEG builds a JPEG carrying only a GPS sub-IFD, to pin down the
// degrees/minutes/seconds -> signed decimal degrees conversion.
func gpsJPEG(t *testing.T) []byte {
	t.Helper()
	le := binary.LittleEndian
	tiff := make([]byte, 128)
	copy(tiff[0:], "II")
	le.PutUint16(tiff[2:], 0x002A)
	le.PutUint32(tiff[4:], 8)

	entry := func(off int, tag, typ uint16, count uint32, value func([]byte)) {
		le.PutUint16(tiff[off:], tag)
		le.PutUint16(tiff[off+2:], typ)
		le.PutUint32(tiff[off+4:], count)
		value(tiff[off+8 : off+12])
	}
	dms := func(off int, vals ...[2]uint32) {
		for i, v := range vals {
			le.PutUint32(tiff[off+i*8:], v[0])
			le.PutUint32(tiff[off+i*8+4:], v[1])
		}
	}

	le.PutUint16(tiff[8:], 1)
	entry(10, 0x8825, 4, 1, func(b []byte) { le.PutUint32(b, 26) }) // GPSInfoIFDPointer
	le.PutUint32(tiff[22:], 0)

	le.PutUint16(tiff[26:], 4)
	entry(28, 0x0001, 2, 2, func(b []byte) { copy(b, "N\x00") })    // GPSLatitudeRef
	entry(40, 0x0002, 5, 3, func(b []byte) { le.PutUint32(b, 80) }) // GPSLatitude
	entry(52, 0x0003, 2, 2, func(b []byte) { copy(b, "W\x00") })    // GPSLongitudeRef
	entry(64, 0x0004, 5, 3, func(b []byte) { le.PutUint32(b, 104) })
	le.PutUint32(tiff[76:], 0)
	dms(80, [2]uint32{51, 1}, [2]uint32{30, 1}, [2]uint32{0, 1})     // 51°30'00" N
	dms(104, [2]uint32{0, 1}, [2]uint32{7, 1}, [2]uint32{3600, 100}) // 0°07'36" W

	app1 := []byte{0xFF, 0xE1}
	app1 = binary.BigEndian.AppendUint16(app1, uint16(2+6+len(tiff)))
	app1 = append(app1, "Exif\x00\x00"...)
	app1 = append(app1, tiff...)

	base := jpegBytes(t, 2, 2)
	out := append([]byte{}, base[:2]...)
	out = append(out, app1...)
	return append(out, base[2:]...)
}

func TestImageExifGPSAsDecimalDegrees(t *testing.T) {
	res, err := (&ImageConverter{}).Convert(
		bytes.NewReader(gpsJPEG(t)), StreamInfo{FileName: "geo.jpg"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "![geo.jpg]()\n\n" +
		"- Dimensions: 2 × 2\n- Format: jpeg\n\n" +
		"| Tag | Value |\n" +
		"| --- | --- |\n" +
		"| GPS Latitude | 51.500000 |\n" +
		"| GPS Longitude | -0.126667 |\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}
