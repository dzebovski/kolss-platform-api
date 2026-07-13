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

func TestBuildTelegramNotificationMessage(t *testing.T) {
	msg := BuildTelegramNotificationMessage(map[string]any{
		"name":          "Іван <Менеджер>",
		"phone":         "+380501112233",
		"source_system": "site_form",
		"client_info":   "Які меблі вам потрібно виготовити?: Кухня & шафа\nЯк вам зручно спілкуватися?: Telegram",
		"crm_url":       "https://crm.example/crm/leads/1?a=1&b=2",
	})
	want := "🔔 Нова заявка!\n👤 Ім'я: Іван &lt;Менеджер&gt;\n\nЯкі меблі вам потрібно виготовити?: Кухня &amp; шафа\nЯк вам зручно спілкуватися?: Telegram\n📞 Тел: +380501112233\n🌐 Джерело: Site Form\n🔗 Посилання на CRM: <a href=\"https://crm.example/crm/leads/1?a=1&amp;b=2\">Відкрити в CRM</a>"
	if msg != want {
		t.Fatalf("message mismatch\n got: %q\nwant: %q", msg, want)
	}
}

func TestBuildSlackNotificationMessageSkipsEmptyClientInfo(t *testing.T) {
	msg := BuildSlackNotificationMessage(map[string]any{
		"name":          "Іван",
		"phone":         "+380501112233",
		"source_system": "meta_lead_ads",
		"client_info":   "   ",
		"crm_url":       "https://crm.example/crm/leads/1",
	})
	want := "🔔 Нова заявка!\n👤 Ім'я: Іван\n📞 Тел: +380501112233\n🌐 Джерело: Facebook Forms\n🔗 Посилання на CRM: https://crm.example/crm/leads/1"
	if msg != want {
		t.Fatalf("message mismatch\n got: %q\nwant: %q", msg, want)
	}
}
