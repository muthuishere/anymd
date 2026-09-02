// Package skills carries the anymd agent skill as data embedded in the binary.
//
// It exists for one reason: //go:embed cannot name a path outside the
// directory of the package that declares it. The CLI lives in cmd/anymd, the
// skill lives in skills/anymd, and no directive written in cmd/anymd can reach
// up and across to it.
//
// The alternatives were worse. Copying SKILL.md into cmd/anymd at build time
// leaves two files to keep in step, and the stale one ships silently. Encoding
// the skill as a Go string literal means a generate step nobody remembers to
// run. A three-line package in the directory that already holds the skill keeps
// exactly ONE canonical SKILL.md, needs no build step, and works from a plain
// `go install` with no repository checked out.
package skills

import "embed"

// FS holds the skill directory. Every path in it is prefixed with Root.
//
//go:embed anymd
var FS embed.FS

// Root is the directory prefix every path in FS carries, and the name of the
// directory the skill is installed as.
const Root = "anymd"
