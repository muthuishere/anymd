package anymd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/richardlehane/mscfb"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&MsgConverter{}) }

// cfbMagic is the Compound File Binary header signature. It is shared with the
// legacy Office formats (.doc/.xls/.ppt), so it is never sufficient on its own
// to identify a .msg — see MsgConverter.Accepts.
var cfbMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// msgPropPrefix is the name prefix of every MAPI property stream inside a .msg
// container: `__substg1.0_<PROPID><TYPE>`, all hex.
const msgPropPrefix = "__substg1.0_"

// msgPropPrefixUTF16 is msgPropPrefix as it appears in a CFB directory entry,
// which stores names as UTF-16LE. Scanning for these raw bytes tells a .msg
// apart from a .doc without parsing the container.
var msgPropPrefixUTF16 = []byte("_\x00_\x00s\x00u\x00b\x00s\x00t\x00g\x001\x00.\x000\x00_\x00")

const (
	maxMsgBytes   = 128 << 20 // whole container
	maxMsgStream  = 32 << 20  // any single property stream
	maxMsgSniff   = 8 << 20   // how far Accepts will scan for the substg marker
	maxMsgEntries = 1 << 16   // directory entries we will walk
)

// MAPI property ids we render. Values are the 16-bit property ids as they
// appear in the stream name.
const (
	msgPropSubject     = "0037"
	msgPropSenderName  = "0C1A"
	msgPropSenderEmail = "0C1F"
	msgPropSenderSMTP  = "5D01"
	msgPropDisplayTo   = "0E04"
	msgPropDisplayCc   = "0E03"
	msgPropDeliverTime = "0E06"
	msgPropSubmitTime  = "0039"
	msgPropBody        = "1000"
	msgPropBodyHTML    = "1013"
)

// MsgConverter converts an Outlook .msg (a MAPI message in a Compound File
// Binary container) to Markdown.
type MsgConverter struct{}

// Name identifies the converter in errors and in `anymd --list`.
func (c *MsgConverter) Name() string { return "msg" }

// Accepts recognizes a .msg.
//
// The CFB magic alone is NOT enough: it is byte-for-byte the magic of legacy
// .doc, .xls and .ppt, so accepting on it would hijack every one of those files
// and — because the engine treats an accepted-then-failed conversion as a hard
// error — permanently break them rather than letting their own converter run.
// So unless the filename says .msg, we additionally require the UTF-16LE
// `__substg1.0_` directory-entry marker, which only a MAPI message carries.
func (c *MsgConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	var head [8]byte
	n, _ := io.ReadFull(r, head[:])
	if n != len(head) || !bytes.Equal(head[:], cfbMagic) {
		return false
	}
	if info.HasExt(".msg") {
		return true
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false
	}
	// Bounded scan: the directory sectors of a real .msg sit well inside this
	// window, and the cap keeps Accepts cheap on a huge legacy Office file.
	buf, err := io.ReadAll(io.LimitReader(r, maxMsgSniff))
	if err != nil {
		return false
	}
	return bytes.Contains(buf, msgPropPrefixUTF16)
}

// Convert renders subject, envelope and body.
func (c *MsgConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (res Result, err error) {
	defer func() {
		// mscfb walks sector chains built from attacker-controlled indices.
		if p := recover(); p != nil {
			res = Result{}
			err = fmt.Errorf("malformed msg: %v", p)
		}
	}()

	raw, err := io.ReadAll(io.LimitReader(r, maxMsgBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(raw) > maxMsgBytes {
		return Result{}, fmt.Errorf("msg too large: over %d bytes", maxMsgBytes)
	}

	props, fixed, err := msgReadProps(bytes.NewReader(raw))
	if err != nil {
		return Result{}, err
	}
	if len(props) == 0 && len(fixed) == 0 {
		return Result{}, errors.New("no MAPI properties found (not an Outlook .msg?)")
	}

	subject := props[msgPropSubject]

	var rows [][]string
	if from := msgFrom(props); from != "" {
		rows = append(rows, []string{"From", from})
	}
	if to := props[msgPropDisplayTo]; to != "" {
		rows = append(rows, []string{"To", to})
	}
	if cc := props[msgPropDisplayCc]; cc != "" {
		rows = append(rows, []string{"Cc", cc})
	}
	if date := msgDate(props, fixed); date != "" {
		rows = append(rows, []string{"Date", date})
	}

	blocks := []string{mdutil.Heading(1, subject)}
	if len(rows) > 0 {
		blocks = append(blocks, mdutil.Table([]string{"Field", "Value"}, rows))
	}
	blocks = append(blocks, msgBody(props))

	return Result{Markdown: mdutil.Join(blocks...), Title: strings.TrimSpace(subject)}, nil
}

// msgReadProps walks the CFB and returns the decoded top-level string
// properties keyed by property id, plus the raw fixed-length property block.
//
// Only top-level streams are read: nested storages hold attachments and
// recipients, whose bodies are not part of this message's text.
func msgReadProps(ra io.ReaderAt) (map[string]string, []byte, error) {
	doc, err := mscfb.New(ra)
	if err != nil {
		return nil, nil, err
	}
	props := make(map[string]string)
	var fixed []byte
	seen := 0
	for entry, err := doc.Next(); err == nil; entry, err = doc.Next() {
		if seen++; seen > maxMsgEntries {
			break
		}
		if len(entry.Path) != 0 || entry.Size <= 0 {
			continue
		}
		switch {
		case entry.Name == "__properties_version1.0":
			fixed = msgReadStream(entry)
		case strings.HasPrefix(entry.Name, msgPropPrefix):
			id, typ, ok := msgParseName(entry.Name)
			if !ok {
				continue
			}
			if _, dup := props[id]; dup {
				continue
			}
			if v, ok := msgDecode(msgReadStream(entry), typ); ok {
				props[id] = v
			}
		}
	}
	return props, fixed, nil
}

// msgParseName splits `__substg1.0_0037001F` into its property id and type.
func msgParseName(name string) (id, typ string, ok bool) {
	rest := name[len(msgPropPrefix):]
	if len(rest) < 8 {
		return "", "", false
	}
	rest = strings.ToUpper(rest[:8])
	for i := 0; i < 8; i++ {
		if !isHexDigit(rest[i]) {
			return "", "", false
		}
	}
	return rest[:4], rest[4:], true
}

func isHexDigit(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'A' && b <= 'F'
}

// msgReadStream reads one stream, capped. The declared Size is attacker
// controlled, so it is clamped before it is used to size an allocation.
func msgReadStream(f *mscfb.File) []byte {
	size := f.Size
	if size <= 0 {
		return nil
	}
	if size > maxMsgStream {
		size = maxMsgStream
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(f, buf)
	if err != nil && n == 0 {
		return nil
	}
	return buf[:n]
}

// msgDecode turns a property stream's bytes into text according to its MAPI
// type suffix.
//
// 001F is PT_UNICODE — UTF-16LE. Getting this wrong is the classic .msg bug:
// reading UTF-16LE as bytes yields text with a NUL between every letter, which
// then silently survives into the index. 001E is PT_STRING8, and 0102 is
// PT_BINARY, which is how Outlook usually stores the HTML body.
func msgDecode(b []byte, typ string) (string, bool) {
	switch typ {
	case "001F":
		return decodeUTF16LE(b), true
	case "001E", "0102":
		return decode8Bit(b), true
	}
	return "", false
}

// decodeUTF16LE decodes UTF-16LE, tolerating a trailing odd byte and unpaired
// surrogates (utf16.Decode maps those to U+FFFD rather than failing).
func decodeUTF16LE(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	n := len(b) / 2
	u := make([]uint16, n)
	for i := 0; i < n; i++ {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return strings.TrimRight(string(utf16.Decode(u)), "\x00")
}

// decode8Bit handles PT_STRING8. The codepage is a separate property that is
// frequently absent or wrong, so we take valid UTF-8 as-is and otherwise fall
// back to Latin-1, which never fails and never invents replacement characters.
func decode8Bit(b []byte) string {
	b = bytes.TrimRight(b, "\x00")
	if utf8.Valid(b) {
		return string(b)
	}
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

// msgFrom renders the sender as "Name <email>", using whichever halves exist.
func msgFrom(props map[string]string) string {
	name := strings.TrimSpace(props[msgPropSenderName])
	email := strings.TrimSpace(props[msgPropSenderEmail])
	if email == "" {
		email = strings.TrimSpace(props[msgPropSenderSMTP])
	}
	switch {
	case name != "" && email != "":
		return name + " <" + email + ">"
	case name != "":
		return name
	default:
		return email
	}
}

// msgDate returns the send/delivery time. Timestamps are FILETIME values, which
// live in the fixed-length property block rather than in a substg stream, so a
// string property is only a fallback for producers that write one.
func msgDate(props map[string]string, fixed []byte) string {
	if s := strings.TrimSpace(props[msgPropDeliverTime]); s != "" {
		return s
	}
	for _, id := range []string{msgPropDeliverTime, msgPropSubmitTime} {
		if t, ok := msgFixedTime(fixed, id); ok {
			return t.UTC().Format("2006-01-02 15:04:05 MST")
		}
	}
	return ""
}

// msgFixedTime scans the `__properties_version1.0` block for a PT_SYSTIME
// (type 0x0040) entry with the given property id.
//
// Layout: a header (32 bytes for a top-level message) followed by 16-byte
// entries of {tag uint32, flags uint32, value [8]byte}, where the tag's high
// 16 bits are the property id and the low 16 bits the type.
func msgFixedTime(fixed []byte, id string) (time.Time, bool) {
	want, err := strconv.ParseUint(id, 16, 16)
	if err != nil || len(fixed) < 32 {
		return time.Time{}, false
	}
	body := fixed[32:]
	for off := 0; off+16 <= len(body); off += 16 {
		tag := binary.LittleEndian.Uint32(body[off:])
		if uint32(tag>>16) != uint32(want) || tag&0xFFFF != 0x0040 {
			continue
		}
		ft := binary.LittleEndian.Uint64(body[off+8:])
		if ft == 0 {
			continue
		}
		return filetimeToTime(ft)
	}
	return time.Time{}, false
}

// filetimeToTime converts a Windows FILETIME (100ns ticks since 1601-01-01 UTC)
// to a time.Time, rejecting values that would overflow into nonsense dates.
func filetimeToTime(ft uint64) (time.Time, bool) {
	const ticksPerSecond = 10000000
	const epochDelta = 11644473600 // seconds between 1601-01-01 and 1970-01-01
	secs := int64(ft/ticksPerSecond) - epochDelta
	if secs < -62135596800 || secs > 253402300799 { // year 1 .. year 9999
		return time.Time{}, false
	}
	nsec := int64(ft%ticksPerSecond) * 100
	return time.Unix(secs, nsec).UTC(), true
}

// msgBody renders the message body, preferring the HTML part.
//
// The HTML body carries the structure — headings, lists, links, tables — that
// the plain-text part has already thrown away, so converting it gives strictly
// more usable Markdown. The plain body is the fallback when there is no HTML
// part or when the HTML fails to convert.
func msgBody(props map[string]string) string {
	plain := strings.TrimSpace(strings.ReplaceAll(props[msgPropBody], "\r\n", "\n"))
	if html := strings.TrimSpace(props[msgPropBodyHTML]); html != "" {
		if md, err := HTMLToMarkdown(html, ""); err == nil {
			if md = strings.TrimSpace(md); md != "" {
				return md
			}
		}
	}
	return plain
}
