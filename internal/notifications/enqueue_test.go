package notifications

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestTelegramChatIDsKyivIncludesPrimaryAndUniqueAdditionalIDs(t *testing.T) {
	outbox := Outbox{
		TelegramChatIDKyiv:            "-100111",
		TelegramAdditionalChatIDsKyiv: " -1002833157899, -100111, -100222 ",
	}
	if got, want := outbox.telegramChatIDs("kyiv"), []string{"-100111", "-1002833157899", "-100222"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("telegramChatIDs() = %#v, want %#v", got, want)
	}
}

func TestDeliveryDestinationsRouteKyivToTelegramAndWarsawToSlack(t *testing.T) {
	outbox := Outbox{
		TelegramChatIDKyiv:            "-100111",
		TelegramAdditionalChatIDsKyiv: "-100222",
		SlackChannelIDWarsaw:          " C123WARSAW ",
	}
	if got, want := outbox.deliveryDestinations("kyiv"), []deliveryDestination{
		{channel: "telegram", destination: "-100111"},
		{channel: "telegram", destination: "-100222"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Kyiv destinations = %#v, want %#v", got, want)
	}
	if got, want := outbox.deliveryDestinations("warsaw"), []deliveryDestination{
		{channel: "slack", destination: "C123WARSAW"},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Warsaw destinations = %#v, want %#v", got, want)
	}
	if got, want := outbox.SlackChannelID("warsaw"), "C123WARSAW"; got != want {
		t.Fatalf("SlackChannelID(warsaw) = %q, want %q", got, want)
	}
	if got := outbox.SlackChannelID("kyiv"); got != "" {
		t.Fatalf("SlackChannelID(kyiv) = %q, want empty", got)
	}
}

func TestCRMLeadURLUsesCRMDomainRoot(t *testing.T) {
	leadID := uuid.MustParse("ceaf7ee5-28fe-4133-8a54-84dda27d0f8b")
	if got := crmLeadURL("https://crm.kolss.eu/crm/leads/:id?stale=true", leadID); got == nil || *got != "https://crm.kolss.eu/crm/leads/ceaf7ee5-28fe-4133-8a54-84dda27d0f8b" {
		t.Fatalf("crmLeadURL() = %v", got)
	}
}
