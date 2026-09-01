package anymd

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/muthuishere/anymd/internal/mdutil"
)

func init() { addBuiltin(&RSSConverter{}) }

// rssSniffBytes is how much of the head we look at to decide "this is a feed".
// The root element of a feed is within the first few hundred bytes even after
// an XML declaration, a stylesheet PI and a comment or two.
const rssSniffBytes = 4096

// rssMaxBytes bounds the buffered feed. Feeds are parsed whole.
const rssMaxBytes = 64 << 20 // 64 MiB

// RSSConverter renders an RSS 2.0, RDF/RSS 1.0, or Atom feed as Markdown.
//
// It stays at PrioritySpecific so it wins over the generic HTML converter, and
// it pairs a hint check with a content sniff: a plain .xml file that is not a
// feed must fall through to whoever really owns it, not be claimed here and
// then hard-fail the engine.
type RSSConverter struct{}

// Name implements Named.
func (c *RSSConverter) Name() string { return "rss" }

// feedRoots are the root elements that mean "feed": RSS 2.0, Atom, RSS 1.0.
var feedRoots = []string{"<rss", "<feed", "<rdf:rdf"}

// Accepts requires BOTH a plausible hint (extension or mime) AND a sniff that
// the head really contains a feed root element.
func (c *RSSConverter) Accepts(r io.ReadSeeker, info StreamInfo, opts *Options) bool {
	hinted := info.HasExt(".rss", ".atom", ".xml", ".rdf") ||
		info.HasMimePrefix(
			"application/rss+xml", "application/atom+xml", "application/rdf+xml",
			"application/xml", "text/xml",
		)
	if !hinted {
		return false
	}
	var head [rssSniffBytes]byte
	n, _ := io.ReadFull(r, head[:])
	if n <= 0 {
		return false
	}
	s := strings.ToLower(string(head[:n]))
	for _, root := range feedRoots {
		if strings.Contains(s, root) {
			return true
		}
	}
	return false
}

// Convert parses the feed and renders it, preserving item order as given.
func (c *RSSConverter) Convert(r io.ReadSeeker, info StreamInfo, opts *Options) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, err = Result{}, fmt.Errorf("rss: parse panicked: %v", p)
		}
	}()

	raw, err := io.ReadAll(io.LimitReader(r, rssMaxBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(raw) > rssMaxBytes {
		return Result{}, fmt.Errorf("rss: input exceeds %d bytes", rssMaxBytes)
	}

	feed, err := gofeed.NewParser().Parse(bytes.NewReader(raw))
	if err != nil {
		return Result{}, fmt.Errorf("rss: %w", err)
	}
	if feed == nil {
		return Result{}, fmt.Errorf("rss: empty feed")
	}

	base := feed.Link
	if base == "" {
		base = info.URL
	}

	blocks := []string{
		mdutil.Heading(1, feed.Title),
		mdutil.Collapse(feed.Description),
	}
	for _, item := range feed.Items {
		if item == nil {
			continue
		}
		blocks = append(blocks, c.itemBlocks(item, base)...)
	}
	return Result{Markdown: mdutil.Join(blocks...), Title: mdutil.Collapse(feed.Title)}, nil
}

// itemBlocks renders one entry: heading, metadata line, link, body.
func (c *RSSConverter) itemBlocks(item *gofeed.Item, base string) []string {
	out := []string{mdutil.Heading(2, item.Title)}

	if meta := feedItemMeta(item); meta != "" {
		out = append(out, "*"+meta+"*")
	}
	if link := strings.TrimSpace(item.Link); link != "" {
		text := mdutil.Collapse(item.Title)
		if text == "" {
			text = link
		}
		out = append(out, "["+feedEscapeLinkText(text)+"]("+link+")")
	}

	body := item.Content // content:encoded
	if strings.TrimSpace(body) == "" {
		body = item.Description
	}
	if strings.TrimSpace(body) != "" {
		// Feed bodies are HTML (usually escaped HTML), so they go through the
		// shared HTML path. A body we cannot parse is dropped rather than
		// failing the whole feed — one bad entry should not lose the other 49.
		itemBase := strings.TrimSpace(item.Link)
		if itemBase == "" {
			itemBase = base
		}
		if md, err := HTMLToMarkdown(body, itemBase); err == nil {
			out = append(out, md)
		}
	}
	return out
}

// feedItemMeta builds the metadata line: an RFC3339 published date and the
// author, each included only when the feed actually carried it.
func feedItemMeta(item *gofeed.Item) string {
	var parts []string
	if d := feedDate(item); d != "" {
		parts = append(parts, d)
	}
	if a := feedAuthor(item); a != "" {
		parts = append(parts, "by "+a)
	}
	return strings.Join(parts, " — ")
}

// feedDate normalizes the publication date to RFC3339 in UTC, so two feeds
// that disagree about format (RFC822 in RSS, RFC3339 in Atom) render the same.
// An unparseable date falls back to the raw string rather than vanishing.
func feedDate(item *gofeed.Item) string {
	switch {
	case item.PublishedParsed != nil:
		return item.PublishedParsed.UTC().Format(time.RFC3339)
	case item.UpdatedParsed != nil:
		return item.UpdatedParsed.UTC().Format(time.RFC3339)
	case strings.TrimSpace(item.Published) != "":
		return mdutil.Collapse(item.Published)
	case strings.TrimSpace(item.Updated) != "":
		return mdutil.Collapse(item.Updated)
	}
	return ""
}

func feedAuthor(item *gofeed.Item) string {
	for _, p := range item.Authors {
		if p == nil {
			continue
		}
		if n := mdutil.Collapse(p.Name); n != "" {
			return n
		}
		if e := mdutil.Collapse(p.Email); e != "" {
			return e
		}
	}
	if item.Author != nil {
		if n := mdutil.Collapse(item.Author.Name); n != "" {
			return n
		}
		if e := mdutil.Collapse(item.Author.Email); e != "" {
			return e
		}
	}
	return ""
}

// feedEscapeLinkText keeps a title containing brackets from breaking the link.
func feedEscapeLinkText(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "[", `\[`)
	s = strings.ReplaceAll(s, "]", `\]`)
	return s
}
