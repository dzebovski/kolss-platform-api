package metaleads

import (
	"encoding/json"
	"time"
)

const (
	maxAttempts = 10
	batchSize   = 20
)

type Page struct {
	OfficeCode string
	PageID     string
	Token      string
}

type Config struct {
	Enabled                bool
	GraphAPIVersion        string
	AppID                  string
	AppSecret              string
	WebhookVerifyToken     string
	Pages                  []Page
	IngestAfter            time.Time
	ReconciliationInterval time.Duration
	ReconciliationLookback time.Duration
	AlertTelegramBotToken  string
	AlertTelegramChatID    string
}

type webhookPayload struct {
	Object string         `json:"object"`
	Entry  []webhookEntry `json:"entry"`
}

type webhookEntry struct {
	ID      string          `json:"id"`
	Time    int64           `json:"time"`
	Changes []webhookChange `json:"changes"`
}

type webhookChange struct {
	Field string             `json:"field"`
	Value webhookChangeValue `json:"value"`
}

type webhookChangeValue struct {
	LeadgenID string `json:"leadgen_id"`
	PageID    string `json:"page_id"`
	FormID    string `json:"form_id"`
	AdID      string `json:"ad_id"`
	AdgroupID string `json:"adgroup_id"`
	CreatedAt int64  `json:"created_time"`
}

type Lead struct {
	ID                        string                     `json:"id"`
	CreatedTime               string                     `json:"created_time"`
	AdID                      string                     `json:"ad_id"`
	AdName                    string                     `json:"ad_name"`
	AdsetID                   string                     `json:"adset_id"`
	AdsetName                 string                     `json:"adset_name"`
	CampaignID                string                     `json:"campaign_id"`
	CampaignName              string                     `json:"campaign_name"`
	FormID                    string                     `json:"form_id"`
	FormName                  string                     `json:"form_name"`
	Platform                  string                     `json:"platform"`
	IsOrganic                 *bool                      `json:"is_organic"`
	FieldData                 []LeadField                `json:"field_data"`
	CustomDisclaimerResponses []CustomDisclaimerResponse `json:"custom_disclaimer_responses"`
	RawPayload                json.RawMessage            `json:"-"`
}

func (l *Lead) UnmarshalJSON(data []byte) error {
	type leadAlias Lead
	var decoded leadAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*l = Lead(decoded)
	l.RawPayload = append(l.RawPayload[:0], data...)
	return nil
}

type PageInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type LeadField struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

type CustomDisclaimerResponse struct {
	CheckboxKey string `json:"checkbox_key"`
	IsChecked   string `json:"is_checked"`
}

type Form struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Status    string          `json:"status"`
	Locale    string          `json:"locale"`
	Questions json.RawMessage `json:"questions"`
}

type pageResponse[T any] struct {
	Data   []T         `json:"data"`
	Paging graphPaging `json:"paging"`
}

type graphPaging struct {
	Cursors struct {
		After string `json:"after"`
	} `json:"cursors"`
	Next string `json:"next"`
}

type graphErrorEnvelope struct {
	Error GraphError `json:"error"`
}

type GraphError struct {
	HTTPStatus   int    `json:"-"`
	Message      string `json:"message"`
	Type         string `json:"type"`
	Code         int    `json:"code"`
	ErrorSubcode int    `json:"error_subcode"`
	IsTransient  bool   `json:"is_transient"`
	TraceID      string `json:"fbtrace_id"`
}

func (e *GraphError) Error() string {
	if e == nil {
		return "Meta Graph API error"
	}
	if e.Code != 0 {
		return "Meta Graph API error code " + itoa(e.Code) + ": " + e.Message
	}
	return "Meta Graph API HTTP " + itoa(e.HTTPStatus) + ": " + e.Message
}

func (e *GraphError) OAuth() bool {
	return e != nil && (e.Code == 102 || e.Code == 190)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buffer [20]byte
	i := len(buffer)
	for value > 0 {
		i--
		buffer[i] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		i--
		buffer[i] = '-'
	}
	return string(buffer[i:])
}
