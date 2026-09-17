package discovery

import "testing"

func TestParseDeleteMessage(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"live english", "Deleted 0 Crm Contacts successfully.", "Crm Contacts", true},
		{"nonzero", "Deleted 12 Pages successfully.", "Pages", true},
		{"single word", "Deleted 1 Media successfully.", "Media", true},
		// The string goes through req.t, so a project that ships another
		// locale or overrides translations produces something else entirely.
		// That is a miss, not an error.
		{"german", "0 Seiten erfolgreich gelöscht.", "", false},
		{"custom endpoint", `{"ok":true}`, "", false},
		{"missing period", "Deleted 0 Pages successfully", "", false},
		{"empty", "", "", false},
		{"no count", "Deleted Pages successfully.", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseDeleteMessage(tt.in)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("ParseDeleteMessage(%q) = %q,%v want %q,%v", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestLabelsAreNeverBlank(t *testing.T) {
	tests := []struct {
		slug, singularType string
		wantSingular       string
		wantPlural         string
	}{
		{"crm-contacts", "CrmContact", "Crm Contact", "Crm Contacts"},
		{"pages", "Page", "Page", "Pages"},
		// No pluralisation guessing: without a GraphQL singular the plural
		// form is reused rather than mangled (cms -> Cm, physics -> Physic).
		{"cms", "", "Cms", "Cms"},
		{"form_submissions", "", "Form Submissions", "Form Submissions"},
	}
	for _, tt := range tests {
		l := DeriveLabels(tt.slug, tt.singularType)
		if l.Singular != tt.wantSingular || l.Plural != tt.wantPlural {
			t.Errorf("DeriveLabels(%q,%q) = %q/%q want %q/%q",
				tt.slug, tt.singularType, l.Singular, l.Plural, tt.wantSingular, tt.wantPlural)
		}
		if l.Source != SourceDerived {
			t.Errorf("source = %q, want derived", l.Source)
		}
		if l.Singular == "" || l.Plural == "" {
			t.Error("labels are never blank")
		}
	}
}

func TestLabelsFromMessageKeepsDerivedSingular(t *testing.T) {
	l := LabelsFromMessage("crm-contacts", "CrmContact", "Contacts")
	if l.Plural != "Contacts" {
		t.Errorf("plural = %q", l.Plural)
	}
	if l.Singular != "Crm Contact" {
		t.Errorf("singular = %q; the message only ever carries the plural", l.Singular)
	}
	if l.Source != SourceBulkDeleteMessage {
		t.Errorf("source = %q", l.Source)
	}
}

func TestTitleFromSlug(t *testing.T) {
	tests := map[string]string{
		"crm-contacts":       "Crm Contacts",
		"form_submissions":   "Form Submissions",
		"payload-jobs-stats": "Payload Jobs Stats",
		"pages":              "Pages",
		"":                   "",
	}
	for in, want := range tests {
		if got := TitleFromSlug(in); got != want {
			t.Errorf("TitleFromSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
