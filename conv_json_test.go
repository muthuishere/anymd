package anymd

import (
	"strings"
	"testing"
)

// TestJSONArrayOfObjectsBecomesTable covers the "exported records" case: the
// header is the UNION of keys in first-seen order, so a record missing a key
// still lines up and a record introducing one still widens the table.
func TestJSONArrayOfObjectsBecomesTable(t *testing.T) {
	in := `[{"id":1,"name":"ada","active":true},
	        {"name":"grace","id":2,"note":null,"score":9.50}]`
	res, err := New().ConvertBytes([]byte(in), StreamInfo{Extension: ".json"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := "| id | name | active | note | score |\n" +
		"| --- | --- | --- | --- | --- |\n" +
		"| 1 | ada | true |  |  |\n" +
		"| 2 | grace |  |  | 9.50 |\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

// TestJSONObjectStaysCodeBlock: only the flat-array shape is promoted.
func TestJSONObjectStaysCodeBlock(t *testing.T) {
	in := `{"b":1,"a":[1,2]}`
	res, err := New().ConvertBytes([]byte(in), StreamInfo{Extension: ".json"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := "```json\n{\n  \"b\": 1,\n  \"a\": [\n    1,\n    2\n  ]\n}\n```\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

// TestJSONNestedArrayStaysCodeBlock: an array whose objects hold non-scalars is
// not a record set, so it keeps the code block.
func TestJSONNestedArrayStaysCodeBlock(t *testing.T) {
	in := `[{"id":1,"tags":["x"]}]`
	res, err := New().ConvertBytes([]byte(in), StreamInfo{Extension: ".json"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	want := "```json\n[\n  {\n    \"id\": 1,\n    \"tags\": [\n      \"x\"\n    ]\n  }\n]\n```\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
}

// TestJSONAcceptsRequiresRealJSON: the .json extension is often a lie, and a
// converter that accepts and then fails is a hard error for the engine.
func TestJSONAcceptsRequiresRealJSON(t *testing.T) {
	c := &JSONConverter{}
	for _, tc := range []struct {
		name string
		body string
		info StreamInfo
		want bool
	}{
		{"valid with ext", `{"a":1}`, StreamInfo{Extension: ".json"}, true},
		{"valid bare", `[1,2]`, StreamInfo{}, true},
		{"lying ext", "not json at all", StreamInfo{Extension: ".json"}, false},
		{"broken json with ext", `{"a":`, StreamInfo{Extension: ".json"}, false},
		{"mime only", `{"a":1}`, StreamInfo{MimeType: "application/json"}, true},
		{"notebook declined", `{"nbformat":4}`, StreamInfo{Extension: ".ipynb"}, false},
		{"plain text", "hello", StreamInfo{}, false},
		{"empty", "", StreamInfo{Extension: ".json"}, false},
	} {
		if got := c.Accepts(strings.NewReader(tc.body), tc.info, nil); got != tc.want {
			t.Errorf("%s: Accepts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestJSONHugeHeadFallsBackToHint proves the bounded sniff: past the 1 MiB
// window the head cannot be validated, so only the hint decides — and the
// converter never parses the whole file inside Accepts.
func TestJSONHugeHeadFallsBackToHint(t *testing.T) {
	big := "[" + strings.Repeat(`"x",`, (jsonSniffBytes/4)+16) + `"y"]`
	c := &JSONConverter{}
	if !c.Accepts(strings.NewReader(big), StreamInfo{Extension: ".json"}, nil) {
		t.Error("Accepts(big, hinted) = false, want true")
	}
	if c.Accepts(strings.NewReader(big), StreamInfo{}, nil) {
		t.Error("Accepts(big, unhinted) = true, want false")
	}
}

func TestJSONInvalidBodyIsAnError(t *testing.T) {
	if _, err := (&JSONConverter{}).Convert(strings.NewReader(`{"a":`), StreamInfo{Extension: ".json"}, nil); err == nil {
		t.Error("Convert(invalid) = nil error, want an error")
	}
}
