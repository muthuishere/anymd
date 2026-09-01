package anymd

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

const (
	cfbSectorSize  = 512
	cfbEndOfChain  = 0xFFFFFFFE
	cfbFreeSect    = 0xFFFFFFFF
	cfbFatSect     = 0xFFFFFFFD
	cfbNoStream    = 0xFFFFFFFF
	cfbStreamAlloc = 4096 // one sector-aligned allocation per stream, 8 sectors
)

type cfbStream struct {
	name string
	data []byte
}

// buildCFB writes a real Compound File Binary container: header, one FAT
// sector, a directory chain and one 4096-byte allocation per stream.
//
// Every stream is declared as exactly 4096 bytes — the mini-stream cutoff — so
// the fixture needs no mini-FAT and no mini-stream while staying a legal CFB.
// The trailing NUL padding is harmless: both MAPI string types are NUL
// terminated, which is precisely what the decoder must already handle.
func buildCFB(streams []cfbStream) []byte {
	nStreams := len(streams)
	dirSectors := (1 + nStreams + 3) / 4
	dataStart := uint32(1 + dirSectors)
	totalSectors := 1 + dirSectors + nStreams*8

	out := make([]byte, (1+totalSectors)*cfbSectorSize)
	le := binary.LittleEndian

	// --- header (the first 512 bytes, which is not itself a numbered sector)
	copy(out[0:], []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
	le.PutUint16(out[24:], 0x003E) // minor version
	le.PutUint16(out[26:], 0x0003) // major version 3
	le.PutUint16(out[28:], 0xFFFE) // little endian
	le.PutUint16(out[30:], 0x0009) // 512-byte sectors
	le.PutUint16(out[32:], 0x0006) // 64-byte mini sectors
	le.PutUint32(out[40:], 0)      // num directory sectors: 0 for v3
	le.PutUint32(out[44:], 1)      // one FAT sector
	le.PutUint32(out[48:], 1)      // directory starts at sector 1
	le.PutUint32(out[56:], 4096)   // mini stream cutoff
	le.PutUint32(out[60:], cfbEndOfChain)
	le.PutUint32(out[64:], 0)
	le.PutUint32(out[68:], cfbEndOfChain)
	le.PutUint32(out[72:], 0)
	for i := 76; i+4 <= 512; i += 4 {
		le.PutUint32(out[i:], cfbFreeSect)
	}
	le.PutUint32(out[76:], 0) // DIFAT[0] -> FAT lives in sector 0

	sector := func(n uint32) []byte {
		off := int(n+1) * cfbSectorSize
		return out[off : off+cfbSectorSize]
	}

	// --- FAT
	fat := sector(0)
	for i := 0; i+4 <= cfbSectorSize; i += 4 {
		le.PutUint32(fat[i:], cfbFreeSect)
	}
	le.PutUint32(fat[0:], cfbFatSect)
	for i := 0; i < dirSectors; i++ {
		next := uint32(cfbEndOfChain)
		if i < dirSectors-1 {
			next = uint32(2 + i)
		}
		le.PutUint32(fat[(1+i)*4:], next)
	}
	for s := 0; s < nStreams; s++ {
		base := dataStart + uint32(s*8)
		for k := uint32(0); k < 8; k++ {
			next := uint32(cfbEndOfChain)
			if k < 7 {
				next = base + k + 1
			}
			le.PutUint32(fat[(base+k)*4:], next)
		}
	}

	// --- directory
	// Sector 1 begins one sector past the header.
	dir := out[2*cfbSectorSize : (2+dirSectors)*cfbSectorSize]
	writeEntry := func(idx int, name string, objType byte, left, right, child, start, size uint32) {
		e := dir[idx*128 : (idx+1)*128]
		u := utf16.Encode([]rune(name))
		for i, c := range u {
			le.PutUint16(e[i*2:], c)
		}
		le.PutUint16(e[64:], uint16(2*(len(u)+1)))
		e[66] = objType
		e[67] = 1 // black
		le.PutUint32(e[68:], left)
		le.PutUint32(e[72:], right)
		le.PutUint32(e[76:], child)
		le.PutUint32(e[116:], start)
		le.PutUint32(e[120:], size)
	}
	writeEntry(0, "Root Entry", 5, cfbNoStream, cfbNoStream, 1, cfbEndOfChain, 0)
	for i, s := range streams {
		right := uint32(cfbNoStream)
		if i+2 <= nStreams {
			right = uint32(i + 2)
		}
		writeEntry(i+1, s.name, 2, cfbNoStream, right, cfbNoStream,
			dataStart+uint32(i*8), cfbStreamAlloc)
	}

	// --- stream data
	for i, s := range streams {
		off := int(dataStart+uint32(i*8)+1) * cfbSectorSize
		copy(out[off:off+cfbStreamAlloc], s.data)
	}
	return out
}

// utf16LE encodes a Go string the way a PT_UNICODE (001F) property stream
// stores it.
func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	return b
}

// propsBlockWithTime builds a `__properties_version1.0` block holding one
// PT_SYSTIME entry.
func propsBlockWithTime(propID uint16, t time.Time) []byte {
	b := make([]byte, 32+16)
	binary.LittleEndian.PutUint32(b[32:], uint32(propID)<<16|0x0040)
	const epochDelta = 11644473600
	ft := uint64(t.UTC().Unix()+epochDelta) * 10000000
	binary.LittleEndian.PutUint64(b[40:], ft)
	return b
}

func sampleMsg() []byte {
	return buildCFB([]cfbStream{
		{"__substg1.0_0037001F", utf16LE("Quarterly report")},
		{"__substg1.0_0C1A001F", utf16LE("Ada Lovelace")},
		{"__substg1.0_0C1F001F", utf16LE("ada@example.com")},
		{"__substg1.0_0E04001F", utf16LE("bob@example.com")},
		{"__substg1.0_0E03001F", utf16LE("carol@example.com")},
		{"__substg1.0_1000001F", utf16LE("Numbers are up.\r\nDetails attached.")},
		{"__properties_version1.0", propsBlockWithTime(0x0E06,
			time.Date(2024, 3, 5, 9, 41, 0, 0, time.UTC))},
	})
}

func TestMsgConvertPlainBody(t *testing.T) {
	res, err := (&MsgConverter{}).Convert(bytes.NewReader(sampleMsg()),
		StreamInfo{FileName: "mail.msg", Extension: ".msg"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "# Quarterly report\n\n" +
		"| Field | Value |\n" +
		"| --- | --- |\n" +
		"| From | Ada Lovelace <ada@example.com> |\n" +
		"| To | bob@example.com |\n" +
		"| Cc | carol@example.com |\n" +
		"| Date | 2024-03-05 09:41:00 UTC |\n\n" +
		"Numbers are up.\nDetails attached.\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
	if res.Title != "Quarterly report" {
		t.Fatalf("Title = %q", res.Title)
	}
}

func TestMsgPrefersHTMLBody(t *testing.T) {
	msg := buildCFB([]cfbStream{
		{"__substg1.0_0037001F", utf16LE("Structured")},
		{"__substg1.0_1000001F", utf16LE("plain fallback")},
		{"__substg1.0_1013001E", []byte("<html><body><h2>Agenda</h2><ul><li>One</li></ul></body></html>")},
	})
	res, err := (&MsgConverter{}).Convert(bytes.NewReader(msg),
		StreamInfo{Extension: ".msg"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	// The exact HTML->Markdown rendering belongs to conv_html.go; what this
	// converter owns is the choice of the HTML part over the plain one.
	if strings.Contains(res.Markdown, "plain fallback") {
		t.Fatalf("plain body used despite an HTML part:\n%s", res.Markdown)
	}
	for _, want := range []string{"# Structured", "Agenda", "One"} {
		if !strings.Contains(res.Markdown, want) {
			t.Fatalf("missing %q in:\n%s", want, res.Markdown)
		}
	}
}

func TestMsgFallsBackToPlainWhenNoHTML(t *testing.T) {
	msg := buildCFB([]cfbStream{
		{"__substg1.0_0037001F", utf16LE("Subject only")},
		{"__substg1.0_1000001F", utf16LE("body text")},
	})
	res, err := (&MsgConverter{}).Convert(bytes.NewReader(msg), StreamInfo{Extension: ".msg"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "# Subject only\n\nbody text\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestMsgEightBitPropertiesDecode(t *testing.T) {
	msg := buildCFB([]cfbStream{
		{"__substg1.0_0037001E", []byte("ASCII subject")},
		{"__substg1.0_0C1A001E", []byte("Bob")},
		{"__substg1.0_1000001E", []byte("plain 8-bit body")},
	})
	res, err := (&MsgConverter{}).Convert(bytes.NewReader(msg), StreamInfo{Extension: ".msg"}, &Options{})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	want := "# ASCII subject\n\n" +
		"| Field | Value |\n| --- | --- |\n| From | Bob |\n\n" +
		"plain 8-bit body\n"
	if res.Markdown != want {
		t.Fatalf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

// TestMsgDecodeTypes is the unit-level guard on the single most common .msg
// bug: reading a PT_UNICODE stream as bytes, which leaves a NUL between every
// letter and silently poisons everything downstream.
func TestMsgDecodeTypes(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		typ  string
		want string
		ok   bool
	}{
		{"utf16le", utf16LE("Héllo"), "001F", "Héllo", true},
		{"utf16le trailing nul", append(utf16LE("Hi"), 0, 0), "001F", "Hi", true},
		{"utf16le odd trailing byte", append(utf16LE("Hi"), 0x41), "001F", "Hi", true},
		{"utf16le astral", utf16LE("a\U0001F600"), "001F", "a\U0001F600", true},
		{"utf16le empty", nil, "001F", "", true},
		{"8bit utf8", []byte("café"), "001E", "café", true},
		{"8bit latin1", []byte{0x63, 0x61, 0x66, 0xE9}, "001E", "café", true},
		{"binary html", []byte("<b>x</b>"), "0102", "<b>x</b>", true},
		{"unknown type", []byte("x"), "0003", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := msgDecode(tc.in, tc.typ)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("msgDecode = %q,%v; want %q,%v", got, ok, tc.want, tc.ok)
			}
			if strings.ContainsRune(got, 0) {
				t.Fatalf("decoded text still contains NUL: %q", got)
			}
		})
	}
}

func TestMsgConverterAccepts(t *testing.T) {
	c := &MsgConverter{}
	msg := sampleMsg()

	// A legacy .doc: same CFB magic, but no __substg1.0_ stream anywhere.
	doc := buildCFB([]cfbStream{{"WordDocument", []byte("legacy office")}})

	cases := []struct {
		name string
		body []byte
		info StreamInfo
		want bool
	}{
		{"magic and substg", msg, StreamInfo{}, true},
		{"magic and .msg extension", msg, StreamInfo{Extension: ".msg"}, true},
		{"legacy doc must not be hijacked", doc, StreamInfo{Extension: ".doc"}, false},
		{"legacy doc with no hints", doc, StreamInfo{}, false},
		{"not cfb", []byte("PK\x03\x04 zip"), StreamInfo{Extension: ".msg"}, false},
		{"empty", nil, StreamInfo{Extension: ".msg"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Accepts(bytes.NewReader(tc.body), tc.info, &Options{}); got != tc.want {
				t.Fatalf("Accepts = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMsgMalformedIsAnErrorNotAPanic(t *testing.T) {
	good := sampleMsg()
	cases := map[string][]byte{
		"truncated":   good[:600],
		"header only": good[:512],
		"bad sector size": func() []byte {
			b := append([]byte(nil), good...)
			binary.LittleEndian.PutUint16(b[30:], 0x00FF)
			return b
		}(),
		"wild directory pointer": func() []byte {
			b := append([]byte(nil), good...)
			binary.LittleEndian.PutUint32(b[48:], 0xFFFFFFF0)
			return b
		}(),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := (&MsgConverter{}).Convert(bytes.NewReader(body), StreamInfo{Extension: ".msg"}, &Options{}); err == nil {
				t.Fatal("want an error, got success")
			}
		})
	}
}

func TestMsgThroughEngine(t *testing.T) {
	res, err := New().ConvertBytes(sampleMsg(), StreamInfo{Extension: ".msg", FileName: "mail.msg"}, nil)
	if err != nil {
		t.Fatalf("ConvertBytes: %v", err)
	}
	if !strings.HasPrefix(res.Markdown, "# Quarterly report\n") {
		t.Fatalf("engine routed elsewhere:\n%s", res.Markdown)
	}
}

// TestMsgHeaderGuardRejectsLyingSectorCounts is a regression test for an
// out-of-memory found by CI on Linux (and invisible on macOS, whose allocator
// is lazy enough to hide it). github.com/richardlehane/mscfb sizes allocations
// from the compound-file header, so a 574-byte file declaring four billion FAT
// sectors made it ask the runtime for tens of gigabytes.
func TestMsgHeaderGuardRejectsLyingSectorCounts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int
		field  string
	}{
		{"directory sectors", 40, "directory"},
		{"FAT sectors", 44, "FAT"},
		{"mini FAT sectors", 64, "mini FAT"},
		{"DIFAT sectors", 72, "DIFAT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := make([]byte, 574)
			copy(raw, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
			binary.LittleEndian.PutUint16(raw[30:32], 9)
			binary.LittleEndian.PutUint32(raw[tc.offset:tc.offset+4], 0xFFFFFFF0)
			if err := msgCheckHeader(bytes.NewReader(raw), int64(len(raw))); err == nil {
				t.Fatalf("accepted a header claiming 4 billion %s sectors in 574 bytes", tc.field)
			}
		})
	}
}

// A sector shift outside MS-CFB's two legal values is rejected before mscfb
// can compute a sector size from it.
func TestMsgHeaderGuardRejectsBadSectorShift(t *testing.T) {
	raw := make([]byte, 574)
	copy(raw, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1})
	binary.LittleEndian.PutUint16(raw[30:32], 31) // 2^31-byte sectors
	if err := msgCheckHeader(bytes.NewReader(raw), int64(len(raw))); err == nil {
		t.Fatal("accepted an invalid sector shift")
	}
}
