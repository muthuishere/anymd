package anymd

import (
	"strings"
	"testing"

	"github.com/muthuishere/anymd/internal/mdutil"
)

func TestCSVRaggedRows(t *testing.T) {
	// Row 2 is short and row 3 is long: neither may be dropped, and neither may
	// abort the conversion.
	in := "name,role,city\nada,engineer\ngrace,admiral,arlington,extra\n"
	res, err := New().ConvertBytes([]byte(in), StreamInfo{Extension: ".csv"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	// NOTE: the blank line between the two data rows is mdutil.Table's current
	// behaviour, not this converter's — see TestKnownMdutilTableBlankLineDefect.
	want := "| name | role | city |  |\n" +
		"| --- | --- | --- | --- |\n" +
		"| ada | engineer |  |  |\n" +
		"\n" +
		"| grace | admiral | arlington | extra |\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestCSVBOMAndCRLF(t *testing.T) {
	in := "\xef\xbb\xbfa,b\r\n1,2\r\n"
	res, err := New().ConvertBytes([]byte(in), StreamInfo{Extension: ".csv"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := "| a | b |\n| --- | --- |\n| 1 | 2 |\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestTSVDelimiterFromExtension(t *testing.T) {
	in := "a\tb,c\n1\t2,3\n"
	res, err := New().ConvertBytes([]byte(in), StreamInfo{Extension: ".tsv"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	// The comma inside "b,c" must stay inside one cell.
	want := "| a | b,c |\n| --- | --- |\n| 1 | 2,3 |\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestCSVSniffsDelimiterWithoutExtension(t *testing.T) {
	in := "a;b;c\n1;2;3\n4;5;6\n"
	res, err := New().ConvertBytes([]byte(in), StreamInfo{MimeType: "text/csv"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := "| a | b | c |\n| --- | --- | --- |\n| 1 | 2 | 3 |\n\n| 4 | 5 | 6 |\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestCSVQuotedFieldsAndEmbeddedNewline(t *testing.T) {
	in := "a,b\n\"x, y\",\"line1\nline2\"\n"
	res, err := New().ConvertBytes([]byte(in), StreamInfo{Extension: ".csv"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := "| a | b |\n| --- | --- |\n| x, y | line1 line2 |\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

func TestCSVAccepts(t *testing.T) {
	c := &CSVConverter{}
	for _, tc := range []struct {
		info StreamInfo
		want bool
	}{
		{StreamInfo{Extension: ".csv"}, true},
		{StreamInfo{Extension: ".tsv"}, true},
		{StreamInfo{MimeType: "text/csv; charset=utf-8"}, true},
		{StreamInfo{Extension: ".txt"}, false},
		{StreamInfo{}, false},
	} {
		if got := c.Accepts(strings.NewReader("a,b\n1,2\n"), tc.info, nil); got != tc.want {
			t.Errorf("Accepts(%+v) = %v, want %v", tc.info, got, tc.want)
		}
	}
}

func TestCSVEmptyInput(t *testing.T) {
	res, err := (&CSVConverter{}).Convert(strings.NewReader(""), StreamInfo{Extension: ".csv"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.Markdown != "" {
		t.Errorf("markdown = %q, want empty", res.Markdown)
	}
}

// TestKnownMdutilTableBlankLineDefect pins a defect in the SHARED emitter, not
// in this converter: mdutil.Table writes a newline before every row after the
// first, while writeRow already terminates its own line, so two or more data
// rows come out separated by a blank line — which ends the table in GFM. Every
// multi-row expectation in these converter tests encodes that behaviour, so
// when mdutil.Table is fixed this test fails first and names the cause.
func TestKnownMdutilTableBlankLineDefect(t *testing.T) {
	got := mdutil.Table([]string{"a"}, [][]string{{"1"}, {"2"}})
	if want := "| a |\n| --- |\n| 1 |\n\n| 2 |"; got != want {
		t.Fatalf("mdutil.Table behaviour changed (likely fixed): got %q, want %q\n"+
			"If the blank line is gone, drop the blank lines from the expectations in\n"+
			"conv_csv_test.go, conv_xlsx_test.go, conv_json_test.go and conv_ipynb_test.go.", got, want)
	}
}
