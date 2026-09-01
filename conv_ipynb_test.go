package anymd

import (
	"strings"
	"testing"
)

// arraySourceNotebook exercises the two nbformat shapes that trip people up:
// `source` as an array of fragments (each already newline-terminated) and
// `source` as a plain string, in the same file. It also carries a stream
// output, an execute_result, and a display_data image that must vanish.
const arraySourceNotebook = `{
 "nbformat": 4,
 "nbformat_minor": 5,
 "metadata": {
  "title": "Sales Notebook",
  "kernelspec": {"name": "ir", "language": "R"}
 },
 "cells": [
  {"cell_type": "markdown", "source": ["# Intro\n", "\n", "Some prose.\n"]},
  {"cell_type": "code",
   "source": "print(1 + 1)\n",
   "outputs": [
     {"output_type": "stream", "name": "stdout", "text": ["hello\n", "world\n"]},
     {"output_type": "display_data",
      "data": {"image/png": "iVBORw0KGgoAAAANSU", "text/plain": "<Figure>"}},
     {"output_type": "execute_result", "data": {"text/plain": "2", "image/png": "iVBOR"}}
   ]},
  {"cell_type": "code", "source": ["x <- 1\n", "x + 1"], "outputs": []}
 ]
}`

func TestIpynbArraySourceAndOutputs(t *testing.T) {
	res, err := New().ConvertBytes([]byte(arraySourceNotebook), StreamInfo{Extension: ".ipynb"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.Title != "Sales Notebook" {
		t.Errorf("Title = %q, want %q", res.Title, "Sales Notebook")
	}
	want := "# Intro\n" +
		"\n" +
		"Some prose.\n" +
		"\n" +
		"```R\n" +
		"print(1 + 1)\n" +
		"```\n" +
		"\n" +
		"```\n" +
		"hello\n" +
		"world\n" +
		"```\n" +
		"\n" +
		"```\n" +
		"2\n" +
		"```\n" +
		"\n" +
		"```R\n" +
		"x <- 1\n" +
		"x + 1\n" +
		"```\n"
	if res.Markdown != want {
		t.Errorf("markdown mismatch\n got: %q\nwant: %q", res.Markdown, want)
	}
	if strings.Contains(res.Markdown, "iVBOR") {
		t.Error("base64 image data leaked into the markdown")
	}
}

// TestIpynbDefaultLanguage: no kernelspec at all still fences code as python.
func TestIpynbDefaultLanguage(t *testing.T) {
	nb := `{"nbformat": 4, "cells": [{"cell_type": "code", "source": "pass", "outputs": []}]}`
	res, err := New().ConvertBytes([]byte(nb), StreamInfo{Extension: ".ipynb"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if want := "```python\npass\n```\n"; res.Markdown != want {
		t.Errorf("markdown = %q, want %q", res.Markdown, want)
	}
	if res.Title != "" {
		t.Errorf("Title = %q, want empty", res.Title)
	}
}

// TestIpynbEmptyCellsProduceNothing: blank cells and image-only outputs must not
// leave empty fenced blocks behind.
func TestIpynbEmptyCellsProduceNothing(t *testing.T) {
	nb := `{"nbformat": 4, "cells": [
		{"cell_type": "code", "source": "   ", "outputs": [
			{"output_type": "display_data", "data": {"image/png": "AAAA"}}]},
		{"cell_type": "markdown", "source": []}
	]}`
	res, err := New().ConvertBytes([]byte(nb), StreamInfo{Extension: ".ipynb"}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.Markdown != "" {
		t.Errorf("markdown = %q, want empty", res.Markdown)
	}
}

func TestIpynbAccepts(t *testing.T) {
	c := &IpynbConverter{}
	for _, tc := range []struct {
		name string
		body string
		info StreamInfo
		want bool
	}{
		{"ext and marker", arraySourceNotebook, StreamInfo{Extension: ".ipynb"}, true},
		{"bare with marker", arraySourceNotebook, StreamInfo{}, true},
		{"plain json declined", `{"a": 1}`, StreamInfo{Extension: ".json"}, false},
		{"ext but not json", "nbformat", StreamInfo{Extension: ".ipynb"}, false},
		{"array declined", `[{"nbformat": 4}]`, StreamInfo{}, false},
		{"empty", "", StreamInfo{Extension: ".ipynb"}, false},
	} {
		if got := c.Accepts(strings.NewReader(tc.body), tc.info, nil); got != tc.want {
			t.Errorf("%s: Accepts = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestIpynbBeatsJSON: both converters see notebook bytes, and the notebook one
// must win via its lower priority.
func TestIpynbBeatsJSON(t *testing.T) {
	e := New()
	res, err := e.ConvertBytes([]byte(arraySourceNotebook), StreamInfo{}, nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if res.Title != "Sales Notebook" {
		t.Errorf("Title = %q — the JSON converter appears to have won dispatch", res.Title)
	}
}

func TestIpynbMalformedIsAnError(t *testing.T) {
	c := &IpynbConverter{}
	if _, err := c.Convert(strings.NewReader(`{"nbformat": 4, "cells": `), StreamInfo{Extension: ".ipynb"}, nil); err == nil {
		t.Error("Convert(truncated) = nil error, want an error")
	}
	if _, err := c.Convert(strings.NewReader(`{"cells": []}`), StreamInfo{Extension: ".ipynb"}, nil); err == nil {
		t.Error("Convert(no nbformat) = nil error, want an error")
	}
}
