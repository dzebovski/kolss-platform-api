package metaleads

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

type MappedLead struct {
	ExternalID              string
	Name                    *string
	Phone                   *string
	Email                   *string
	ProductInterest         *string
	ProjectStage            *string
	CommunicationPreference *string
	CityRegion              *string
	SourceNote              *string
	SourceCreatedAt         *time.Time
	AdID                    *string
	AdName                  *string
	CampaignID              *string
	CampaignName            *string
	FormID                  *string
	FormName                *string
	Platform                *string
	IsOrganic               *string
}

var nonPhoneCharacters = regexp.MustCompile(`[^0-9+]`)

var productAliases = normalizedSet(
	"які_меблі_вам_потрібно_виготовити?",
	"що_ви_хочете_замовити?",
	"jakiej_kuchni_szukasz?",
)

var projectStageAliases = normalizedSet(
	"на_якому_етапі_перебуває_ваш_проєкт?",
	"на_якому_етапі_ваш_проєкт?",
	"na_jakim_etapie_jesteś?",
)

var communicationAliases = normalizedSet(
	"як_вам_зручно_спілкуватися?",
	"jak_wolisz_się_kontaktować?",
)

var standardFieldAliases = normalizedSet(
	"full_name", "first_name", "last_name", "phone_number", "phone", "email",
	"city", "city_region", "state", "province",
)

func MapLead(lead Lead) MappedLead {
	values := make(map[string]string, len(lead.FieldData))
	originalNames := make(map[string]string, len(lead.FieldData))
	for _, field := range lead.FieldData {
		key := normalizeFieldName(field.Name)
		value := strings.TrimSpace(strings.Join(field.Values, ", "))
		if key == "" || value == "" {
			continue
		}
		values[key] = value
		originalNames[key] = strings.TrimSpace(field.Name)
	}

	name := values[normalizeFieldName("full_name")]
	if name == "" {
		name = strings.TrimSpace(strings.Join([]string{
			values[normalizeFieldName("first_name")],
			values[normalizeFieldName("last_name")],
		}, " "))
	}
	phone := firstValue(values, "phone_number", "phone")
	email := strings.ToLower(firstValue(values, "email"))
	city := firstValue(values, "city", "city_region", "state", "province")
	product := valueForAliases(values, productAliases)
	projectStage := valueForAliases(values, projectStageAliases)
	communication := valueForAliases(values, communicationAliases)

	noteLines := make([]string, 0, len(values)+len(lead.CustomDisclaimerResponses))
	keys := make([]string, 0, len(values))
	for key := range values {
		if _, standard := standardFieldAliases[key]; standard {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		label := originalNames[key]
		if label == "" {
			label = strings.ReplaceAll(key, "_", " ")
		}
		noteLines = append(noteLines, label+": "+values[key])
	}
	for _, disclaimer := range lead.CustomDisclaimerResponses {
		if key := strings.TrimSpace(disclaimer.CheckboxKey); key != "" {
			noteLines = append(noteLines, key+": "+strings.TrimSpace(disclaimer.IsChecked))
		}
	}

	createdAt := parseMetaTime(lead.CreatedTime)
	isOrganic := (*string)(nil)
	if lead.IsOrganic != nil {
		value := "false"
		if *lead.IsOrganic {
			value = "true"
		}
		isOrganic = &value
	}
	return MappedLead{
		ExternalID:              "l:" + strings.TrimSpace(lead.ID),
		Name:                    clean(name),
		Phone:                   clean(normalizePhone(phone)),
		Email:                   clean(email),
		ProductInterest:         clean(product),
		ProjectStage:            clean(projectStage),
		CommunicationPreference: clean(communication),
		CityRegion:              clean(city),
		SourceNote:              clean(strings.Join(noteLines, "\n")),
		SourceCreatedAt:         createdAt,
		AdID:                    clean(lead.AdID),
		AdName:                  clean(lead.AdName),
		CampaignID:              clean(lead.CampaignID),
		CampaignName:            clean(lead.CampaignName),
		FormID:                  clean(lead.FormID),
		FormName:                clean(lead.FormName),
		Platform:                clean(lead.Platform),
		IsOrganic:               isOrganic,
	}
}

func normalizedSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[normalizeFieldName(value)] = struct{}{}
	}
	return out
}

func normalizeFieldName(value string) string {
	var out strings.Builder
	separator := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			separator = false
			continue
		}
		if !separator && out.Len() > 0 {
			out.WriteByte('_')
			separator = true
		}
	}
	return strings.Trim(out.String(), "_")
}

func valueForAliases(values map[string]string, aliases map[string]struct{}) string {
	for key, value := range values {
		if _, ok := aliases[key]; ok {
			return value
		}
	}
	return ""
}

func firstValue(values map[string]string, aliases ...string) string {
	for _, alias := range aliases {
		if value := strings.TrimSpace(values[normalizeFieldName(alias)]); value != "" {
			return value
		}
	}
	return ""
}

func normalizePhone(value string) string {
	value = nonPhoneCharacters.ReplaceAllString(strings.TrimSpace(value), "")
	if strings.HasPrefix(value, "00") {
		value = "+" + strings.TrimPrefix(value, "00")
	}
	return value
}

func clean(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func parseMetaTime(value string) *time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05-0700"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			parsed = parsed.UTC()
			return &parsed
		}
	}
	return nil
}
