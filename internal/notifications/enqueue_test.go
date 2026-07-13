package notifications

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestTelegramChatIDsKyivIncludesPrimaryAndUniqueAdditionalIDs(t *testing.T) {
	enqueuer := Enqueuer{
		TelegramChatIDKyiv:            "-100111",
		TelegramAdditionalChatIDsKyiv: " -1002833157899, -100111, -100222 ",
	}
	if got, want := enqueuer.telegramChatIDs("kyiv"), []string{"-100111", "-1002833157899", "-100222"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("telegramChatIDs() = %#v, want %#v", got, want)
	}
}

func TestCRMLeadURLUsesCRMDomainRoot(t *testing.T) {
	leadID := uuid.MustParse("ceaf7ee5-28fe-4133-8a54-84dda27d0f8b")
	if got := crmLeadURL("https://crm.kolss.eu/crm/leads/:id?stale=true", leadID); got == nil || *got != "https://crm.kolss.eu/crm/leads/ceaf7ee5-28fe-4133-8a54-84dda27d0f8b" {
		t.Fatalf("crmLeadURL() = %v", got)
	}
}
