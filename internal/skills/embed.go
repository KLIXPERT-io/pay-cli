// Package skills owns the agent-facing documentation that ships inside the
// binary (§14) and installs it into an agent's skill directory.
//
// The canonical source lives only at internal/skills/assets/pay/. There is no
// top-level skills/ directory: go:embed cannot reach a parent directory, so a
// second copy would inevitably drift from the binary it documents, and
// `pay skills print` makes it unnecessary.
package skills

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

//go:embed all:assets
var assets embed.FS

// assetRoot is where the skill tree lives inside the embedded FS.
const assetRoot = "assets/pay"

const (
	// Name is the skill's directory name and the name agents refer to it by.
	Name = "pay"
	// SkillFile is the entry point every agent reads first.
	SkillFile = "SKILL.md"
	// ReferenceDir holds the deep-dive documents.
	ReferenceDir = "references"
	// ProjectFile is the optional, discovery-generated project snapshot.
	ProjectFile = "references/PROJECT.md"
	// DirPerm is the mode for created skill directories (§14).
	DirPerm fs.FileMode = 0o755
	// FilePerm is the mode for installed skill files (§14).
	FilePerm fs.FileMode = 0o644
)

// File is one embedded skill document.
type File struct {
	// Path is relative to the skill root, slash-separated ("SKILL.md",
	// "references/errors.md").
	Path string `json:"path"`
	// Data is the file's content.
	Data []byte `json:"-"`
	// SHA256 is the hex digest recorded in the install manifest.
	SHA256 string `json:"sha256"`
}

// FS returns the embedded skill tree rooted at the skill directory, so
// fs.ReadFile(skills.FS(), "SKILL.md") works.
func FS() fs.FS {
	sub, err := fs.Sub(assets, assetRoot)
	if err != nil {
		// Unreachable: assetRoot is embedded at build time.
		panic("skills: embedded assets are missing: " + err.Error())
	}
	return sub
}

// Files returns every embedded document, sorted by path, with its digest.
func Files() []File {
	root := FS()
	var out []File
	_ = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := fs.ReadFile(root, p)
		if readErr != nil {
			return readErr
		}
		out = append(out, File{Path: p, Data: data, SHA256: Sum(data)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Names lists the embedded document paths, sorted. `pay skills list` prints it.
func Names() []string {
	files := Files()
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// Read returns one embedded document by its skill-relative path.
// `pay skills print [NAME]` uses it; NAME defaults to SKILL.md and may be given
// without the .md suffix or the references/ prefix.
func Read(name string) ([]byte, error) {
	return fs.ReadFile(FS(), Resolve(name))
}

// Resolve maps a user-supplied document name onto an embedded path. It accepts
// "", "SKILL.md", "skill", "errors", "references/errors.md" and "errors.md".
func Resolve(name string) string {
	n := strings.TrimSpace(name)
	n = strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(n, "\\", "/")), "/")
	switch strings.ToLower(n) {
	case "", ".", "skill", "skill.md":
		return SkillFile
	}
	if _, err := fs.Stat(FS(), n); err == nil {
		return n
	}
	if !strings.HasSuffix(n, ".md") {
		n += ".md"
	}
	if _, err := fs.Stat(FS(), n); err == nil {
		return n
	}
	return path.Join(ReferenceDir, path.Base(n))
}

// Sum is the digest recorded in .pay-skill.json and compared on reinstall.
func Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// MandatoryStatements are the four sentences §14 requires verbatim in both
// SKILL.md and references/gotchas.md. Each one is a place where the obvious
// agent behaviour produces a confidently wrong answer, so they are asserted by
// a unit test rather than left to review.
var MandatoryStatements = []string{
	// §9.6.2 — the draft-read rule.
	"A read without `--draft` does **not** filter out unpublished documents — documents that were never published are returned with `_status:\"draft\"`. To get only published content, always pass `--published-only` (`_status = published`). `--draft` additionally swaps in the newest draft for documents that **do** have a published version.",
	// §9.10.3 — whole-document validation and what error.fields[].sent means.
	"Payload validates the **whole document** on update: a `PATCH` of one field re-validates every field of the stored document, so the error can name fields you never sent. `error.fields[].sent` is `true` only for paths that were leaves of the body PayCLI actually sent; when every entry is `sent: false`, the stored document was already invalid and your change was rejected by pre-existing state, not by your input.",
	// §7.9 — the localisation rule.
	"`--locale de` on an untranslated field returns the **default locale's** text unless `fallback-locale=none`, which PayCLI sends by default and reports in `meta.locale`.",
	// §11.2 — the closing rule.
	"**Never branch on `error.message` — it is translated.** Payload runs its error strings through i18n, so the same failure reads differently on a German project, and a proxy can change the language by injecting `Accept-Language`. `error.code`, `error.exit` and `ok` are the stable signals.",
}

var (
	blockquotePrefix = regexp.MustCompile(`(?m)^[ \t]*>[ \t]?`)
	whitespaceRun    = regexp.MustCompile(`\s+`)
)

// NormalizeProse strips Markdown blockquote markers and collapses whitespace so
// a required statement can be compared regardless of how it was wrapped or
// indented. It is how the mandatory-statement test compares text.
func NormalizeProse(s string) string {
	return whitespaceRun.ReplaceAllString(blockquotePrefix.ReplaceAllString(s, ""), " ")
}
