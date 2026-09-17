package discovery

import (
	"fmt"
	"sort"
	"strings"
)

// BlockSources are the four candidate origins of a blocks field's real
// blockType slugs, in §7.10's resolution order.
//
// The API does not have the answer when interfaceName is set: verified that
// Page_Layout.possibleTypes is CallToActionBlock, ContentBlock, MediaBlock,
// ArchiveBlock, FormBlock while the real slugs are cta, content, mediaBlock,
// archive, formBlock. The answer exists on disk, in the project's own source.
type BlockSources struct {
	// Configured is the profile's blocks map, keyed by field path.
	Configured map[string][]string
	// ProjectSource is §7.10's local filesystem scan result — every slug:
	// literal found in an object that also has fields:. It is not keyed by
	// field because the scan cannot know which collection a block belongs to.
	ProjectSource []string
	// ProjectSourceFiles are the absolute paths each slug came from, recorded
	// in diagnostics.block_slug_files[].
	ProjectSourceFiles []string
	// Observed are blockType values seen in sampled documents, keyed by field
	// path. On a fresh project this yields nothing — verified that all 12 live
	// pages have layout: [].
	Observed map[string][]string
}

// BlockResolution is the answer for one blocks field.
type BlockResolution struct {
	Slugs  []string
	Source string
	// Reason is the plain-words explanation written into
	// publishable_reason when nothing resolved.
	Reason string
}

// ResolveBlocks walks §7.10's order: profile map -> project source -> observed
// document values -> unresolved.
//
// interfaces are the UNION possibleTypes (interfaceNames). They are never
// returned as slugs: feeding an interfaceName back to the API is exactly the
// silent-wrong-answer this function exists to prevent.
func ResolveBlocks(fieldPath string, interfaces []string, src BlockSources) BlockResolution {
	if v := src.Configured[fieldPath]; len(v) > 0 {
		return BlockResolution{Slugs: dedupeSorted(v), Source: SourceConfigured}
	}
	if len(src.ProjectSource) > 0 {
		return BlockResolution{Slugs: dedupeSorted(src.ProjectSource), Source: SourceProjectSource}
	}
	if v := src.Observed[fieldPath]; len(v) > 0 {
		return BlockResolution{Slugs: dedupeSorted(v), Source: SourceObserved}
	}
	return BlockResolution{
		Slugs:  nil,
		Source: SourceUnknown,
		Reason: UnresolvedBlocksReason(fieldPath),
	}
}

// UnresolvedBlocksReason is §7.10's publishable_reason, verbatim.
func UnresolvedBlocksReason(fieldPath string) string {
	return fmt.Sprintf(
		"required field %q is a blocks field whose allowed blockType values cannot be determined from the Payload API for this project",
		fieldPath)
}

// DescribeBlocksHelp is the text `pay describe <c> --field <f>` prints for an
// unresolved blocks field, verbatim from §7.10. It names only sources that can
// actually answer: no hint in PayCLI may name a `pay` command that cannot.
func DescribeBlocksHelp(interfaces []string, profile, fieldPath string) string {
	names := strings.Join(firstFew(interfaces, 2), ", ")
	if names == "" {
		names = "CallToActionBlock, ContentBlock"
	}
	if profile == "" {
		profile = "local"
	}
	return "allowed blockType values cannot be determined from the Payload API for this project.\n" +
		"GraphQL exposes interfaceNames (" + names + ", ...), not the real slugs,\n" +
		"and no document in this collection has a non-empty " + fieldPath + " to observe.\n" +
		"Read them from payload.config.ts / src/blocks/*/config.ts (look for `slug:`),\n" +
		"then pin them: pay config set profiles." + profile + ".blocks." + fieldPath + " cta,content,mediaBlock"
}

func firstFew(ss []string, n int) []string {
	if len(ss) <= n {
		return ss
	}
	return ss[:n]
}

func dedupeSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
