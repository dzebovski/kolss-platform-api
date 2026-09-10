package crmapi

import "testing"

func TestSourceLanguageForOffice(t *testing.T) {
	tests := []struct {
		office string
		want   string
		ok     bool
	}{
		{office: "kyiv", want: "UK", ok: true},
		{office: "warsaw", want: "PL", ok: true},
		{office: "berlin", want: "", ok: false},
	}
	for _, test := range tests {
		t.Run(test.office, func(t *testing.T) {
			got, ok := sourceLanguageForOffice(test.office)
			if got != test.want || ok != test.ok {
				t.Fatalf("sourceLanguageForOffice(%q) = %q, %v", test.office, got, ok)
			}
		})
	}
}
func TestNormalizeLanguages(t *testing.T) {
	if got := normalizeTargetLanguage("en"); got != "EN-GB" {
		t.Fatalf("normalizeTargetLanguage('en') = %q, want 'EN-GB'", got)
	}
	if got := normalizeTargetLanguage("PL "); got != "PL" {
		t.Fatalf("normalizeTargetLanguage('PL ') = %q, want 'PL'", got)
	}
	if got := normalizeSourceLanguage(" uk "); got != "UK" {
		t.Fatalf("normalizeSourceLanguage(' uk ') = %q, want 'UK'", got)
	}
}
