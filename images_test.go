package anymd

import (
	"regexp"
	"strings"
	"testing"
)

// dataURIPattern matches an inlined image payload in rendered Markdown.
var dataURIPattern = regexp.MustCompile(`\(data:[^)]*\)`)

// stripDataURIs collapses every inlined image to `(data:…)`.
//
// It exists so a test can assert on a whole document by equality — which is
// the strictest and most readable form — without writing a megabyte of base64
// into its expectation, and without weakening to a substring check that would
// stop noticing everything around the image.
func stripDataURIs(md string) string {
	return dataURIPattern.ReplaceAllString(md, "(data:…)")
}

func TestImageBudgetInlinesAndDedups(t *testing.T) {
	png := ooxmlPixels(4096, 7)
	b := newImageBudget(&Options{})

	first := b.dataURI([]byte(png), "image/png")
	if !strings.HasPrefix(first, "data:image/png;base64,") {
		t.Fatalf("not inlined: %.40q", first)
	}
	// The same bytes a second time must cost nothing more: a letterhead on
	// every page is one image, however many times it is drawn.
	spentAfterFirst := b.remaining
	if again := b.dataURI([]byte(png), "image/png"); again != first {
		t.Errorf("identical image encoded twice")
	}
	if b.remaining != spentAfterFirst {
		t.Errorf("repeat image charged again: %d -> %d", spentAfterFirst, b.remaining)
	}
}

func TestImageBudgetRefusesOversizeAndNonImages(t *testing.T) {
	b := newImageBudget(&Options{MaxImageBytes: 1024})
	if uri := b.dataURI([]byte(ooxmlPixels(4096, 3)), "image/png"); uri != "" {
		t.Errorf("image over the per-image cap was inlined")
	}
	// Refusal must not spend the document budget: one huge image cannot
	// starve the ordinary ones after it.
	if b.remaining != defaultMaxImageTotalBytes {
		t.Errorf("refused image still charged the budget")
	}
	if uri := b.dataURI([]byte("this is not an image at all, it is prose"), ""); uri != "" {
		t.Errorf("non-image bytes were inlined")
	}
}

func TestImageBudgetTotalIsBounded(t *testing.T) {
	b := newImageBudget(&Options{MaxImageTotalBytes: 5000})
	var inlined int
	for i := 0; i < 5; i++ {
		// Distinct bytes each time, so the dedup cache cannot absorb them.
		if b.dataURI([]byte(ooxmlPixels(2000, byte(i+1))), "image/png") != "" {
			inlined++
		}
	}
	if inlined != 2 {
		t.Errorf("inlined %d images against a 5000-byte budget, want 2", inlined)
	}
}

func TestImageBudgetDropImages(t *testing.T) {
	b := newImageBudget(&Options{DropImages: true})
	if uri := b.dataURI([]byte(ooxmlPixels(4096, 3)), "image/png"); uri != "" {
		t.Errorf("DropImages still inlined an image")
	}
}

func TestMarkdownImageEscapesAlt(t *testing.T) {
	got := markdownImage("a [bracketed] name\nwith a break", "data:image/png;base64,AAAA")
	want := `![a \[bracketed\] name with a break](data:image/png;base64,AAAA)`
	if got != want {
		t.Errorf("alt not escaped\n got: %q\nwant: %q", got, want)
	}
}
