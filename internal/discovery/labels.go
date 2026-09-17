package discovery

import (
	"regexp"
	"strings"
)

// deletedCountRe is §7.5 probe 8's parser. Payload's bulk-delete endpoint
// answers `Deleted 0 Crm Contacts successfully.` (verified live) and the
// string is produced by req.t('general:deletedCountSuccessfully'), so it is
// English only because Accept-Language: en happens to be honoured and the
// project happens to ship `en`.
//
// The parse is therefore fail-soft by contract: a miss is not an error, it
// simply moves labels.source to "derived" and records LABELS_UNAVAILABLE.
var deletedCountRe = regexp.MustCompile(`^Deleted \d+ (.+) successfully\.$`)

// ParseDeleteMessage extracts the plural label from a bulk-delete message.
// The second result is false for a translated, reworded or custom message.
func ParseDeleteMessage(message string) (string, bool) {
	m := deletedCountRe.FindStringSubmatch(strings.TrimSpace(message))
	if m == nil {
		return "", false
	}
	plural := strings.TrimSpace(m[1])
	if plural == "" {
		return "", false
	}
	return plural, true
}

// DeriveLabels is the fallback: the title-cased slug with - and _ turned into
// spaces (crm-contacts -> "Crm Contacts"). Labels are never blank, never null
// and never half-parsed.
//
// singularType is the entity's GraphQL type name when one is known
// (CrmContact); it yields a genuinely singular label without any pluralisation
// guessing, which §7.3 forbids. Without it the plural form is reused, because
// inventing a singular by stripping an "s" mangles real slugs (`cms` -> `Cm`).
func DeriveLabels(slug, singularType string) Labels {
	plural := TitleFromSlug(slug)
	singular := plural
	if singularType != "" {
		singular = HumanizeField(singularType)
	}
	return Labels{Singular: singular, Plural: plural, Source: SourceDerived}
}

// LabelsFromMessage builds the labels for a successful probe-8 parse. The
// singular still comes from the GraphQL type name or the slug, because the
// message only ever carries the plural.
func LabelsFromMessage(slug, singularType, plural string) Labels {
	l := DeriveLabels(slug, singularType)
	l.Plural = plural
	l.Source = SourceBulkDeleteMessage
	return l
}

// TitleFromSlug turns a slug into a title-cased label.
func TitleFromSlug(slug string) string {
	if slug == "" {
		return ""
	}
	parts := strings.FieldsFunc(slug, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == ' ' || r == '/'
	})
	for i, p := range parts {
		parts[i] = titleWord(p)
	}
	if len(parts) == 0 {
		return titleWord(slug)
	}
	return strings.Join(parts, " ")
}
