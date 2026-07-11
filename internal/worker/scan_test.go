package worker

import "testing"

func TestDetectContentType(t *testing.T) {
	tests := []struct {
		name     string
		head     []byte
		declared string
		want     string
		ok       bool
	}{
		{"pdf", []byte("%PDF-1.7"), "", "application/pdf", true},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0}, "", "image/jpeg", true},
		{"png", []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, "", "image/png", true},
		{"webp", []byte("RIFF....WEBP"), "", "image/webp", true},
		{"txt", []byte("hello world\n"), "text/plain", "text/plain", true},
		{"csv", []byte("a,b,c\n1,2,3\n"), "text/csv", "text/csv", true},
		{"nul binary", []byte{0x00, 0x01, 0x02}, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := DetectContentType(tt.head, tt.declared)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("DetectContentType() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestBuildNotificationMessage(t *testing.T) {
	msg := BuildNotificationMessage(map[string]any{
		"name":          "Іван",
		"phone":         "+380501112233",
		"source_system": "site_form",
		"office_code":   "kyiv",
		"crm_url":       "https://crm.example/crm/leads/1",
	})
	want := "🔔 Нова заявка!\n🏢 Офіс: Kyiv\n👤 Ім'я: Іван\n📞 Тел: +380501112233\n🌐 Джерело: Site Form"
	if msg != want {
		t.Fatalf("message mismatch\n got: %q\nwant: %q", msg, want)
	}
}
