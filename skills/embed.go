// Package skills is the repository-root home of PayCLI's agent skill and the
// single owner of its //go:embed directive (§14).
//
// The skill tree lives at skills/pay/, which is the layout the `skills`
// convention expects (github.com/anthropics/skills), so
//
//	npx skills add https://github.com/KLIXPERT-io/pay-cli/skills --skill pay
//
// finds SKILL.md exactly where it looks for it — the same way KLIXPERT-io/gsc-cli
// ships skills/gsc-cli/SKILL.md.
//
// Making that directory a Go package is what keeps the old embedded-only
// decision's guarantee intact (§1 conflict 18): //go:embed cannot reach a parent
// directory, but it reaches a child, so the package that embeds the tree lives
// *in* the tree's directory. The bytes compiled into `pay` are read from the
// files a reader of the repository sees. There is exactly one copy, no sync
// step, and nothing to drift.
//
// This package is deliberately tiny — embed plus accessors, no filesystem
// writes and no dependencies on the rest of the program. internal/skills
// consumes FS() and owns installation, manifests and project context.
package skills

import (
	"embed"
	"io/fs"
)

// Dir is the skill's directory name: the child of this package on disk, the
// `--skill` argument to `npx skills add`, and the directory created inside an
// agent's skills directory by `pay skills install`.
const Dir = "pay"

//go:embed all:pay
var assets embed.FS

// FS returns the skill tree rooted at the skill directory, so
// fs.ReadFile(skills.FS(), "SKILL.md") reads skills/pay/SKILL.md.
func FS() fs.FS {
	sub, err := fs.Sub(assets, Dir)
	if err != nil {
		// Unreachable: Dir is embedded at build time, and a missing embed is a
		// compile error rather than a runtime one.
		panic("skills: embedded skill tree is missing: " + err.Error())
	}
	return sub
}
