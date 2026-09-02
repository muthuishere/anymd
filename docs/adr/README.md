# Architecture decision records

A record here exists for one reason: to stop a decision from being re-litigated
by someone — including us, in a year — who can see the code but not the
constraint that shaped it. The rule of thumb for writing one is whether a
competent contributor could look at the result and reasonably ask *"why on
earth is it done like that?"*. Everything else belongs in a doc comment.

Records are immutable once accepted. A decision that stops being true is
superseded by a new record that says so and links back; the old one stays,
because the reasoning that was correct at the time is the evidence for whether
the new reasoning is.

| # | Decision | Status |
|---|---|---|
| [0001](0001-pure-go-no-cgo-no-network.md) | Pure Go, and no network the caller did not ask for | Accepted (recorded retrospectively) |
| [0002](0002-vendor-the-pdf-parser.md) | Vendor the PDF parser as `internal/pdf` and cache resolved objects | Accepted |
| [0003](0003-quality-benchmark-against-docling.md) | Measure output quality against docling's ground truth | Accepted |

## Format

Context (the forces, including the ones that lost), Decision (one sentence in
bold, then the detail), Consequences (what this costs, not only what it buys).
Numbers get four digits and never get reused.
