package discovery

import (
	"context"
	"sort"
	"strings"
)

// FallbackNone is the value PayCLI sends as fallback-locale on every read when
// localisation is enabled (§7.9a). sanitizeFallbackLocale maps
// 'false' | 'none' | 'null' to false, so an untranslated field comes back
// null/absent instead of masquerading as a translation.
//
// This is not a display preference. config.localization.fallback defaults to
// true, so without it `pay get pages 1 --locale de` returns English strings for
// every untranslated field, and an agent that read-modify-writes copies English
// into de and believes it translated the page.
const FallbackNone = "none"

// LocaleWarnUnverified is the warning code attached when --locale could not be
// validated client-side.
const LocaleWarnUnverified = "locale_unverified"

// LocaleUnverifiedMessage is §7.9c's message, verbatim. It is verbatim because
// the behaviour it describes is invisible: an unknown locale code is silently
// coerced to the default locale with HTTP 200 and no warning.
const LocaleUnverifiedMessage = "This project's locale codes could not be enumerated. Payload silently returns the DEFAULT locale for an unrecognised code (HTTP 200, no error), so a typo here is indistinguishable from a correct request."

// LocaleUnverifiedHint is §7.9c's hint, verbatim modulo the profile name.
func LocaleUnverifiedHint(profile string) string {
	if profile == "" {
		profile = "local"
	}
	return "pin them once: pay config set profiles." + profile + ".locales en,de"
}

// LocaleAnalysis is the result of reading a `?locale=all` sample.
type LocaleAnalysis struct {
	// Candidates are the true locale codes, or nil when the sample proved
	// nothing. These are real codes, unlike the formatName()-mangled names the
	// GraphQL LocaleInputType enum exposes.
	Candidates []string
	// Localized are the field names that came back as a locale map.
	Localized []string
	// NotLocalized are the field names that did not, on a document where at
	// least one sibling did — which is the only evidence REST offers for a
	// per-field false.
	NotLocalized []string
}

// AnalyzeLocaleAll implements §7.9b step 2. A localized field comes back as
// {"en": …, "de": …} and the keys are true codes. The candidate set is the
// intersection of the key sets that appeared in at least two fields across the
// sampled documents — one field alone could be an ordinary group whose keys
// happen to look like codes.
func AnalyzeLocaleAll(docs []map[string]any) LocaleAnalysis {
	counts := map[string]int{}     // joined key set -> number of fields that produced it
	sets := map[string][]string{}  // joined key set -> the keys
	localized := map[string]bool{} // field -> produced a map
	objectish := map[string]bool{} // field -> produced a JSON object at all
	seenField := map[string]bool{} // field -> seen anywhere

	for _, doc := range docs {
		for name, v := range doc {
			if isReservedDocKey(name) {
				continue
			}
			seenField[name] = true
			obj, ok := v.(map[string]any)
			if !ok || len(obj) == 0 {
				continue
			}
			objectish[name] = true
			keys := sortedKeys(obj)
			if !looksLikeLocaleKeys(keys) {
				continue
			}
			joined := strings.Join(keys, "\x00")
			counts[joined]++
			sets[joined] = keys
			localized[name] = true
		}
	}

	var candidates []string
	for joined, n := range counts {
		if n < 2 {
			continue
		}
		if candidates == nil {
			candidates = append([]string(nil), sets[joined]...)
			continue
		}
		candidates = intersect(candidates, sets[joined])
	}
	sort.Strings(candidates)

	a := LocaleAnalysis{Candidates: candidates}
	if len(localized) == 0 {
		// Nothing proved localized, so nothing can be proved NOT localized
		// either: fields[].localized stays null.
		return a
	}
	for name := range seenField {
		if localized[name] {
			a.Localized = append(a.Localized, name)
		} else {
			a.NotLocalized = append(a.NotLocalized, name)
		}
	}
	sort.Strings(a.Localized)
	sort.Strings(a.NotLocalized)
	return a
}

// reservedDocKeys never carry a locale map, so they would only add noise.
var reservedDocKeys = map[string]bool{
	"id": true, "createdAt": true, "updatedAt": true, "_status": true,
	"deletedAt": true, "sizes": true,
}

func isReservedDocKey(name string) bool { return reservedDocKeys[name] }

// looksLikeLocaleKeys rejects key sets that cannot be locale codes, so a group
// field named {title, description} is not mistaken for two locales. Codes are
// short and restricted to letters, digits, '-', '_' and '.'.
func looksLikeLocaleKeys(keys []string) bool {
	if len(keys) == 0 {
		return false
	}
	for _, k := range keys {
		if k == "" || len(k) > 12 {
			return false
		}
		for _, r := range k {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '-', r == '_', r == '.':
			default:
				return false
			}
		}
	}
	return true
}

func intersect(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, s := range b {
		in[s] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if in[s] {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CrossValidateLocales is §7.9b step 2's GraphQL check: accept the locale=all
// candidates only when their count matches LocaleInputType's and every
// candidate formatName()s to an enum name.
func CrossValidateLocales(candidates, enumNames []string) bool {
	if len(candidates) == 0 || len(enumNames) == 0 {
		return false
	}
	if len(candidates) != len(enumNames) {
		return false
	}
	have := make(map[string]bool, len(enumNames))
	for _, n := range enumNames {
		have[n] = true
	}
	for _, c := range candidates {
		if !have[FormatName(c)] {
			return false
		}
	}
	return true
}

// LocaleInput is §7.9b's ladder input, already fetched.
type LocaleInput struct {
	// Configured is the profile's locales pin.
	Configured []string
	// Analysis is the locale=all sample result.
	Analysis LocaleAnalysis
	// EnumNames are LocaleInputType's enumValues, which are
	// formatName()-mangled and are therefore NEVER usable as codes.
	EnumNames []string
	// GraphQLReachable reports whether the cross-validation could run at all.
	GraphQLReachable bool
}

// ResolveLocales walks §7.9b, stopping at the first hit. The mangled GraphQL
// enum names are deliberately not returned as codes: en-US is exposed as
// en_US and es.419 as es_419, and introspection never exposes the underlying
// value, so codes harvested that way would be rejected by PayCLI's own
// validation if fed back.
func ResolveLocales(in LocaleInput) (locales []string, source string, count *int) {
	if len(in.Configured) > 0 {
		return dedupeSorted(in.Configured), SourceConfigured, intPtr(len(dedupeSorted(in.Configured)))
	}
	if len(in.Analysis.Candidates) > 0 {
		if in.GraphQLReachable && CrossValidateLocales(in.Analysis.Candidates, in.EnumNames) {
			return in.Analysis.Candidates, SourceLocaleAll, intPtr(len(in.Analysis.Candidates))
		}
		if !in.GraphQLReachable {
			return in.Analysis.Candidates, SourceLocaleAllUnverified, intPtr(len(in.Analysis.Candidates))
		}
	}
	if len(in.EnumNames) > 0 {
		// Mangled names inform the count and nothing else.
		return []string{}, SourceGraphQLEnumMangled, intPtr(len(in.EnumNames))
	}
	return []string{}, SourceUnknown, nil
}

// LocalesValidatable reports §7.9c: --locale is checked client-side only when
// the codes are configured or were cross-validated from a locale=all sample.
// Anything else is passed through with the locale_unverified warning.
func LocalesValidatable(source string) bool {
	return source == SourceConfigured || source == SourceLocaleAll
}

// resolveLocalization implements §7.9 end to end: decide whether the project is
// localised at all, then enumerate the true codes.
//
// The `enabled` answer is tri-state. GraphQL settles it outright — the plural
// Query field either takes a `locale` argument or it does not — while REST
// cannot, because `locale` is accepted and ignored when localisation is off.
func (d *Discoverer) resolveLocalization(ctx context.Context, rows map[string]*row,
	schema *Schema, entities map[string]*entity, m *Manifest) Localization {

	loc := Localization{
		Locales:       []string{},
		LocalesSource: SourceUnknown,
		EnabledSource: SourceUnknown,
		DefaultSource: SourceUnknown,
	}

	graphQLReachable := len(schema.Types) > 0
	var enabled *bool
	for _, e := range entities {
		if e.Localized == nil {
			continue
		}
		if *e.Localized {
			enabled = boolPtr(true)
			break
		}
		enabled = boolPtr(false)
	}
	loc.Enabled = enabled
	if enabled != nil {
		loc.EnabledSource = SourceGraphQLArgAbsent
		if *enabled {
			loc.EnabledSource = SourceGraphQLArg
		}
	}

	enumNames := LocaleEnumNames(schema)
	if enabled != nil && !*enabled && len(enumNames) == 0 {
		loc.LocalesSource = SourceGraphQLArgAbsent
		return loc
	}

	// §7.9b step 2: pick the collection with the most fields whose value is a
	// JSON object at depth 0 and sample it with locale=all.
	var analysis LocaleAnalysis
	if slug := d.pickLocaleSampleCollection(ctx, rows); slug != "" {
		docs := d.sampleDocs(ctx, slug, sampleLimit, true)
		if len(docs) > 0 {
			analysis = AnalyzeLocaleAll(docs)
			d.localeAnalysis[slug] = analysis
			d.localeSampled[slug] = true
		}
	}

	locales, source, count := ResolveLocales(LocaleInput{
		Configured:       d.opt.ConfiguredLocales,
		Analysis:         analysis,
		EnumNames:        enumNames,
		GraphQLReachable: graphQLReachable,
	})
	loc.Locales = locales
	loc.LocalesSource = source
	loc.LocaleCount = count

	// GraphQL settles `enabled` outright, but it is not the only evidence
	// there is. On a project with graphQL.disable = true the plural Query
	// field cannot be inspected at all, and `enabled` would stay nil forever —
	// which is what kept fallback-locale=none off every read and let
	// PayCLI answer --locale de with DEFAULT-locale text (§1 conflict 35).
	//
	// Two positive, REST-only proofs settle it instead. They are positive
	// ONLY: an empty candidate set must never set enabled = false, because a
	// sampled collection may simply have no localized fields.
	if loc.Enabled == nil {
		switch {
		case len(d.opt.ConfiguredLocales) > 0:
			// Nobody pins locale codes on a non-localised project.
			loc.Enabled, loc.EnabledSource = boolPtr(true), SourceConfigured
		case source == SourceLocaleAll || source == SourceLocaleAllUnverified:
			// ?locale=all came back with per-locale maps: only a localized
			// field does that.
			loc.Enabled, loc.EnabledSource = boolPtr(true), SourceLocaleAll
		default:
			// Neither GraphQL nor REST settled it (§7.6, LOCALIZATION_UNKNOWN).
			m.AddLimitation(LimLocalizationUnknown, "", "", "")
		}
	}

	if loc.Enabled != nil && *loc.Enabled {
		// §7.9a: fallback-locale=none on every read, so an untranslated field
		// comes back null instead of masquerading as a translation.
		loc.FallbackDefault = strPtr(FallbackNone)
	}
	// localization.defaultLocale is not exposed by any API surface. It is left
	// null, with provenance, rather than guessed from locales[0]: the codes
	// are sorted, so that guess is right only when the default happens to sort
	// first, and it is shipped to agents in PROJECT.md as a flat fact.
	if len(locales) == 0 && (loc.Enabled == nil || *loc.Enabled) {
		m.AddLimitation(LimLocalesUnknown, "", "", "")
	}
	return loc
}

// pickLocaleSampleCollection implements §7.9b's "the collection with the most
// fields whose value is a JSON object at depth 0". It prefers a readable,
// non-internal collection so the sample costs one cheap request.
func (d *Discoverer) pickLocaleSampleCollection(ctx context.Context, rows map[string]*row) string {
	candidates := make([]string, 0, len(rows))
	for slug, r := range rows {
		if r.kind != kindCollection || IsInternal(slug) {
			continue
		}
		candidates = append(candidates, slug)
	}
	sort.Strings(candidates)

	best, bestScore := "", 0
	// firstReadable is the fallback for a project whose collections all score
	// zero, i.e. one whose fields are plain scalars at depth 0.
	firstReadable := ""
	for _, slug := range candidates {
		docs := d.sampleDocs(ctx, slug, 1, false)
		if len(docs) == 0 {
			continue
		}
		if firstReadable == "" {
			firstReadable = slug
		}
		score := 0
		for _, v := range docs[0] {
			if _, ok := v.(map[string]any); ok {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = slug, score
		}
		if bestScore >= 3 {
			// Good enough: more sampling only costs requests.
			break
		}
	}
	if best == "" {
		// The "most JSON-object fields at depth 0" heuristic is aimed at group
		// fields, and a localized *text* field is a plain string in the
		// ordinary view — so a flat-schema project scores zero everywhere and
		// would never be sampled with ?locale=all at all. Sampling the first
		// readable collection instead costs one request and is the difference
		// between knowing this project is localised and silently answering
		// --locale with DEFAULT-locale text (§7.9a). It cannot produce a false
		// positive: AnalyzeLocaleAll still requires two fields to agree on a
		// locale-shaped key set.
		best = firstReadable
	}
	_ = ctx
	return best
}
