// Command inproc measures anymd's in-process conversion time on the same files,
// in the same way, as the markitdown loop in bench/run.sh: one warm-up convert,
// then the mean of ten.
//
// It exists so the "Speed — in-process" table in bench/README.md is reproduced
// by the same script that reproduces every other table. Without it that column
// had no generator, and a claim nothing regenerates is a claim that silently
// goes stale.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/muthuishere/anymd"
)

// files is the markitdown loop's list, in its order, so the two columns of the
// README table line up file for file.
var files = []string{
	"test.docx",
	"test.pdf",
	"test.xlsx",
	"test_wikipedia.html",
	"test.pptx",
	"test.epub",
}

const runs = 10

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: inproc <corpus-dir>")
		os.Exit(2)
	}
	corpus := os.Args[1]
	e := anymd.New()

	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(corpus, name))
		if err != nil {
			continue
		}
		info := anymd.StreamInfo{FileName: name, Extension: filepath.Ext(name)}
		opts := &anymd.Options{}
		// Warm up, and refuse to time a conversion that does not work: a
		// benchmark of an early error measures the error path.
		if _, err := e.ConvertBytes(data, info, opts); err != nil {
			fmt.Printf("    anymd %-24s%8s\n", name, "error")
			continue
		}
		start := time.Now()
		for i := 0; i < runs; i++ {
			if _, err := e.ConvertBytes(data, info, opts); err != nil {
				break
			}
		}
		ms := float64(time.Since(start).Microseconds()) / 1000 / runs
		fmt.Printf("    anymd %-24s%8.2f ms\n", name, ms)
	}
}
