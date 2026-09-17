package discovery

import (
	"fmt"
	"sort"
	"strings"
)

// BlockSources are the candidate origins of a blocks field's real blockType
// slugs, in §7.10's resolution order.
//
// GraphQL is authoritative about WHICH blocks a field accepts and useless
// about what they are CALLED on the wire: verified live that
// __type(name:"Page_Layout").possibleTypes is exactly CallToActionBlock,
// ContentBlock, MediaBlock, ArchiveBlock, FormBlock while the slugs the REST
// API accepts are cta, content, mediaBlock, archive, formBlock. The project's
// own source has the other half — src/blocks/CallToAction/config.ts declares
// slug: 'cta' beside interfaceName: 'CallToActionBlock' — so the two are only
// useful together.
type BlockSources struct {
	// Configured is the profile's blocks map, keyed by field path. A pin is an
	// explicit statement by the operator and outranks everything.
	Configured map[string][]string
	// ProjectSource is §7.10's local filesystem scan result — every slug:
	// literal found in an object that also has fields:. It is a PROJECT-WIDE
	// bag with no field attribution, so it is never the answer for a field on
	// its own; it only supplies the vocabulary SlugByInterface indexes.
	ProjectSource []string
	// ProjectSourceFiles are the absolute paths each slug came from, recorded
	// in diagnostics.block_slug_files[].
	ProjectSourceFiles []string
	// SlugByInterface maps an interfaceName declared in project source to the
	// slug declared beside it in the same object literal. This is what turns a
	// per-field union member into a writable blockType.
	SlugByInterface map[string]string
	// Observed are blockType values seen in sampled documents, keyed by field
	// path. On a fresh project this yields nothing — verified that all 12 live
	// pages have layout: [].
	Observed map[string][]string
}

// BlockResolution is the answer for ONE blocks field. Every answer is
// per-field: there is no such thing as a project-wide blockType list, and
// pretending otherwise is what made `pay describe forms` advertise page layout
// blocks for a form-builder field.
type BlockResolution struct {
	// Slugs are the blockType values the REST API accepts for this field, or
	// nil when nothing could resolve them. nil and empty mean different
	// things: nil is "PayCLI does not know".
	Slugs []string
	// Source is how THIS field was resolved: the weakest provenance among its
	// slugs, so a single inferred member downgrades the whole field.
	Source string
	// SlugSources gives each returned slug its own provenance, so an agent can
	// tell a slug confirmed from project source apart from one inferred from
	// an interfaceName.
	SlugSources map[string]string
	// Interfaces is the field's GraphQL union possibleTypes, verbatim. They
	// are reported, never returned as slugs: feeding an interfaceName back to
	// the API is exactly the silent-wrong-answer this type exists to prevent.
	Interfaces []string
	// Unresolved are union members no source could name, i.e. slugs that are
	// missing from Slugs even though the API accepts them.
	Unresolved []string
	// Reason is the plain-words explanation of why the answer is less than
	// confirmed. It is empty only when every slug came from a confirmed
	// source; when Slugs is nil it is §7.10's publishable_reason.
	Reason string
}

// SourceUnionInferred is the provenance of a slug derived from a GraphQL
// interfaceName because the project source had no pair for it — which is how
// every plugin-provided block arrives, since node_modules is deliberately not
// scanned. It is deliberately distinct from SourceProjectSource so an agent
// can tell a confirmed slug from an inferred one at a glance.
const SourceUnionInferred = "inferred-from-interface-name"

// SourceMixed is a field whose slugs did not all come from the same place.
// It never appears alone: SlugSources always says which slug came from where.
const SourceMixed = "mixed"

// ResolveBlocks answers ONE blocks field.
//
// interfaces are that field's union possibleTypes (interfaceNames) and are the
// AUTHORITATIVE SET of what the field accepts — Page_Layout has five members
// and Form_Fields has nine completely different ones, so no project-wide list
// can be correct for both. The order is:
//
//  1. the profile's pin for this field path
//  2. the field's own GraphQL union, each member resolved to a slug
//  3. blockType values observed in this field's sampled documents
//  4. unknown, with a reason
func ResolveBlocks(fieldPath string, interfaces []string, src BlockSources) BlockResolution {
	ifaces := dedupeSorted(interfaces)
	if len(ifaces) == 0 {
		ifaces = nil
	}

	if slugs := dedupeSorted(src.Configured[fieldPath]); len(slugs) > 0 {
		return BlockResolution{
			Slugs:       slugs,
			Source:      SourceConfigured,
			SlugSources: uniformSources(slugs, SourceConfigured),
			Interfaces:  ifaces,
		}
	}

	if len(ifaces) > 0 {
		return resolveFromUnion(fieldPath, ifaces, src)
	}

	if slugs := dedupeSorted(src.Observed[fieldPath]); len(slugs) > 0 {
		return BlockResolution{
			Slugs:       slugs,
			Source:      SourceObserved,
			SlugSources: uniformSources(slugs, SourceObserved),
			Reason: fmt.Sprintf("%q has no GraphQL union to enumerate, so its blockTypes are only "+
				"those seen in sampled documents; the field may accept more", fieldPath),
		}
	}

	return BlockResolution{
		Slugs:      nil,
		Source:     SourceUnknown,
		Interfaces: ifaces,
		Reason:     UnresolvedBlocksReason(fieldPath),
	}
}

// resolveFromUnion turns a field's possibleTypes into slugs, one member at a
// time, recording where each slug came from.
func resolveFromUnion(fieldPath string, ifaces []string, src BlockSources) BlockResolution {
	observed := map[string]bool{}
	for _, s := range src.Observed[fieldPath] {
		observed[s] = true
	}

	res := BlockResolution{Interfaces: ifaces, SlugSources: map[string]string{}}
	inferred := []string{}
	for _, iface := range ifaces {
		slug, source := src.SlugByInterface[iface], SourceProjectSource
		if slug == "" {
			// No pair in project source. A plugin-provided block lives in
			// node_modules, which §7.10 never scans, so the only thing left is
			// Payload's own naming rule run backwards.
			slug, source = SlugFromInterfaceName(iface), SourceUnionInferred
			if slug != "" && observed[slug] {
				// A real document carries this exact blockType, which is
				// stronger evidence than the heuristic that produced it.
				source = SourceObserved
			}
		}
		if slug == "" {
			res.Unresolved = append(res.Unresolved, iface)
			continue
		}
		if _, dup := res.SlugSources[slug]; !dup {
			res.Slugs = append(res.Slugs, slug)
		}
		if source == SourceUnionInferred {
			inferred = append(inferred, slug)
		}
		res.SlugSources[slug] = source
	}

	if len(res.Slugs) == 0 {
		return BlockResolution{
			Source:     SourceUnknown,
			Interfaces: ifaces,
			Unresolved: res.Unresolved,
			Reason:     UnresolvedBlocksReason(fieldPath),
		}
	}
	sort.Strings(res.Slugs)
	res.Source = weakestSource(res.SlugSources)

	switch {
	case len(res.Unresolved) > 0:
		res.Reason = fmt.Sprintf("%q accepts %d block types but PayCLI could only name %d: "+
			"no slug could be resolved for %s",
			fieldPath, len(ifaces), len(res.Slugs), strings.Join(res.Unresolved, ", "))
	case len(inferred) > 0:
		res.Reason = fmt.Sprintf("%s in %q %s inferred from the GraphQL interfaceName because the "+
			"project source declares no matching `slug:`/`interfaceName:` pair (a plugin-provided "+
			"block lives in node_modules, which is never scanned)",
			strings.Join(inferred, ", "), fieldPath, plural(len(inferred), "was", "were"))
	}
	return res
}

// SlugFromInterfaceName runs Payload's GraphQL type naming backwards for a
// block whose slug PayCLI could not read from project source.
//
// Payload names a block's union member after its interfaceName when one is
// declared and after the PascalCased slug otherwise, so lower-casing the first
// letter recovers the slug for every single-word and camelCase slug. Verified
// against @payloadcms/plugin-form-builder, whose nine Form_Fields members
// Checkbox, Country, Email, Message, Number, Select, State, Text and Textarea
// invert to exactly the nine slugs it declares (checkbox, country, email,
// message, number, select, state, text, textarea), and against a live form
// document whose fields[].blockType values are text, email and textarea.
//
// It refuses the cases it cannot invert rather than guessing: an acronym run
// (FAQBlock) would yield fAQBlock and a kebab-case slug's PascalCase is not
// recoverable at all. Returning "" there makes the member show up in
// BlockResolution.Unresolved, which is the honest answer.
func SlugFromInterfaceName(name string) string {
	r := []rune(name)
	if len(r) == 0 || !isUpper(r[0]) {
		return ""
	}
	if len(r) > 1 && isUpper(r[1]) {
		return ""
	}
	r[0] = r[0] - 'A' + 'a'
	return string(r)
}

// sourceRank orders provenance from strongest to weakest.
func sourceRank(source string) int {
	switch source {
	case SourceConfigured:
		return 0
	case SourceProjectSource:
		return 1
	case SourceObserved:
		return 2
	case SourceUnionInferred:
		return 3
	default:
		return 4
	}
}

// weakestSource is the field-level source: the weakest provenance any of its
// slugs carries, because a field is only as trustworthy as its least
// trustworthy member. A field whose slugs came from two different places
// reports "mixed" rather than naming one of them and hiding the other.
func weakestSource(bySlug map[string]string) string {
	distinct := map[string]bool{}
	worst, worstRank := SourceUnknown, -1
	for _, s := range bySlug {
		distinct[s] = true
		if r := sourceRank(s); r > worstRank {
			worst, worstRank = s, r
		}
	}
	if len(distinct) > 1 {
		return SourceMixed
	}
	return worst
}

func uniformSources(slugs []string, source string) map[string]string {
	out := make(map[string]string, len(slugs))
	for _, s := range slugs {
		out[s] = source
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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

// ResolveShardBlocks resolves EVERY blocks field of a shard, one field at a
// time, and records the answer on the shard.
//
// The per-field union is read from the field's own GraphQL type
// (Page_Layout, Form_Fields, …) rather than from a project-wide list, which is
// the whole point: the project source scan cannot know which collection a
// block belongs to, so attaching its bag to every blocks field made
// `pay describe forms` advertise page layout blocks for a form-builder field.
//
// It is idempotent and safe to run over an already-resolved shard; the caller
// must call Finalize afterwards, because the answer is part of the shard hash.
func ResolveShardBlocks(shard *Shard, schema *Schema, src BlockSources) {
	if shard == nil {
		return
	}
	for _, f := range shard.Fields {
		if f.PayloadType != TypeBlocks {
			continue
		}
		res := ResolveBlocks(f.Path, UnionMembers(schema, f.GraphQLType), src)
		shard.SetBlockField(BlockField{
			Path:           f.Path,
			Slugs:          res.Slugs,
			Source:         res.Source,
			SlugSources:    res.SlugSources,
			InterfaceNames: res.Interfaces,
			Unresolved:     res.Unresolved,
			Reason:         res.Reason,
		})
	}
}

// UnionMembers returns the possibleTypes of a field's GraphQL type when that
// type is a union, and nil otherwise. nil means "the API published no union
// for this field", which is a genuinely different answer from "the union is
// empty" and is what sends ResolveBlocks down its non-GraphQL ladder.
func UnionMembers(schema *Schema, graphQLType *string) []string {
	if schema == nil || graphQLType == nil || *graphQLType == "" {
		return nil
	}
	t := schema.Type(*graphQLType)
	if t == nil || t.Kind != KindUnion {
		return nil
	}
	return t.PossibleTypeNames()
}

// maxObservedBlockDepth bounds the walk ObservedBlockTypes does. Three levels
// covers a blocks field nested inside a group inside a group; deeper than that
// the cost of walking every sampled document stops paying for itself.
const maxObservedBlockDepth = 3

// ObservedBlockTypes harvests the blockType values actually present in sampled
// documents, keyed by the dotted field path they appeared at.
//
// This is the only source that is both per-field and evidence-based, so it is
// what keeps the REST-only path (no GraphQL, therefore no union) from having
// to guess. It reports what was SEEN, never what is allowed: the caller must
// not present it as a complete list.
func ObservedBlockTypes(docs []map[string]any) map[string][]string {
	out := map[string][]string{}
	for _, doc := range docs {
		collectBlockTypes(doc, "", out, 0)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func collectBlockTypes(obj map[string]any, prefix string, out map[string][]string, depth int) {
	if depth > maxObservedBlockDepth {
		return
	}
	for name, v := range obj {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		switch t := v.(type) {
		case map[string]any:
			collectBlockTypes(t, path, out, depth+1)
		case []any:
			for _, item := range t {
				row, ok := item.(map[string]any)
				if !ok {
					continue
				}
				bt, _ := row["blockType"].(string)
				if bt == "" {
					continue
				}
				if !containsString(out[path], bt) {
					out[path] = append(out[path], bt)
				}
			}
		}
	}
}
