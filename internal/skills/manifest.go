package skills

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// ManifestName is the install record written next to the skill files (§14).
const ManifestName = ".pay-skill.json"

// Manifest records what was installed, by which binary, against which project.
// Its Files map is what makes "the user edited this file" detectable: a hash
// that differs from the recorded one was not written by PayCLI.
type Manifest struct {
	CLIVersion        string            `json:"cli_version"`
	InstalledAt       string            `json:"installed_at"`
	Scope             string            `json:"scope"`
	Agents            []string          `json:"agents"`
	Profile           string            `json:"profile"`
	BaseURL           string            `json:"base_url"`
	DiscoveryRevision string            `json:"discovery_revision,omitempty"`
	Files             map[string]string `json:"files"`
}

// ManifestPath is the manifest's location inside an installed skill directory.
func ManifestPath(dir string) string { return filepath.Join(dir, ManifestName) }

// LoadManifest reads the manifest from an installed skill directory. A missing
// manifest is (nil, nil): the directory may predate PayCLI or have been created
// by hand, and that is not an error. An unparseable one is cache_corrupt
// (exit 1) so the user is told to delete it rather than silently losing the
// user-edit protection it provides.
func LoadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(ManifestPath(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, apierr.Wrap(err, apierr.CodeInternal, "cannot read %s: %v", ManifestPath(dir), err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, apierr.Wrap(err, apierr.CodeCacheCorrupt,
			"%s is not valid JSON: %v", ManifestPath(dir), err).
			WithHint("delete %s and re-run `pay skills install`", ManifestPath(dir))
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return &m, nil
}

// Save writes the manifest atomically. base_url passes through redact.URL
// because a project skill directory is typically committed (§14 Privacy).
func (m *Manifest) Save(dir string) error {
	out := *m
	out.BaseURL = redact.URL(out.BaseURL)
	out.Agents = append([]string(nil), out.Agents...)
	sort.Strings(out.Agents)
	if out.Files == nil {
		out.Files = map[string]string{}
	}
	// Key-sorted, indented JSON: this file is committed and must diff cleanly.
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "cannot encode %s: %v", ManifestName, err)
	}
	b = append(b, '\n')
	if err := fsatomic.Write(ManifestPath(dir), b, FilePerm); err != nil {
		return apierr.Wrap(err, apierr.CodeInternal, "cannot write %s: %v", ManifestPath(dir), err)
	}
	return nil
}

// Recorded returns the hash this manifest has for a skill-relative path, and
// whether it has one at all.
func (m *Manifest) Recorded(rel string) (string, bool) {
	if m == nil || m.Files == nil {
		return "", false
	}
	sum, ok := m.Files[rel]
	return sum, ok
}
