package metaleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const graphResponseLimit = 2 * 1024 * 1024

var leadFields = strings.Join([]string{
	"id",
	"created_time",
	"ad_id",
	"ad_name",
	"adset_id",
	"adset_name",
	"campaign_id",
	"campaign_name",
	"form_id",
	"platform",
	"is_organic",
	"field_data",
	"custom_disclaimer_responses",
}, ",")

type Client struct {
	Version   string
	AppSecret string
	Pages     map[string]Page
	HTTP      *http.Client
	BaseURL   string
}

func NewClient(cfg Config) *Client {
	pages := make(map[string]Page, len(cfg.Pages))
	for _, page := range cfg.Pages {
		pages[page.PageID] = page
	}
	return &Client{
		Version:   strings.TrimPrefix(cfg.GraphAPIVersion, "/"),
		AppSecret: cfg.AppSecret,
		Pages:     pages,
		HTTP:      &http.Client{Timeout: 15 * time.Second},
		BaseURL:   "https://graph.facebook.com",
	}
}

func (c *Client) Page(pageID string) (Page, bool) {
	page, ok := c.Pages[strings.TrimSpace(pageID)]
	return page, ok
}

func (c *Client) FetchLead(ctx context.Context, pageID, leadID string) (Lead, error) {
	var lead Lead
	values := url.Values{"fields": []string{leadFields}}
	err := c.get(ctx, pageID, strings.TrimSpace(leadID), values, &lead)
	return lead, err
}

func (c *Client) FetchPage(ctx context.Context, pageID string) (PageInfo, error) {
	var page PageInfo
	values := url.Values{"fields": []string{"id,name"}}
	err := c.get(ctx, pageID, strings.TrimSpace(pageID), values, &page)
	return page, err
}

func (c *Client) ListForms(ctx context.Context, pageID, after string) ([]Form, string, error) {
	values := url.Values{
		"fields": []string{"id,name,status,locale,questions"},
		"limit":  []string{"100"},
	}
	if after != "" {
		values.Set("after", after)
	}
	var response pageResponse[Form]
	if err := c.get(ctx, pageID, strings.TrimSpace(pageID)+"/leadgen_forms", values, &response); err != nil {
		return nil, "", err
	}
	return response.Data, nextCursor(response.Paging), nil
}

func (c *Client) ListLeads(ctx context.Context, pageID, formID, after string) ([]Lead, string, error) {
	values := url.Values{
		"fields": []string{leadFields},
		"limit":  []string{"100"},
	}
	if after != "" {
		values.Set("after", after)
	}
	var response pageResponse[Lead]
	if err := c.get(ctx, pageID, strings.TrimSpace(formID)+"/leads", values, &response); err != nil {
		return nil, "", err
	}
	return response.Data, nextCursor(response.Paging), nil
}

func nextCursor(paging graphPaging) string {
	if strings.TrimSpace(paging.Next) == "" {
		return ""
	}
	return strings.TrimSpace(paging.Cursors.After)
}

func (c *Client) get(ctx context.Context, pageID, path string, values url.Values, target any) error {
	page, ok := c.Page(pageID)
	if !ok {
		return fmt.Errorf("unknown Meta page %q", pageID)
	}
	base := strings.TrimRight(c.BaseURL, "/")
	version := strings.Trim(c.Version, "/")
	if base == "" || version == "" || strings.Trim(path, "/") == "" {
		return errors.New("invalid Meta Graph API configuration")
	}
	values.Set("appsecret_proof", AppSecretProof(c.AppSecret, page.Token))
	requestURL := base + "/" + version + "/" + strings.TrimLeft(path, "/") + "?" + values.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+page.Token)
	request.Header.Set("Accept", "application/json")

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return &transportError{err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, graphResponseLimit+1))
	if err != nil {
		return err
	}
	if len(body) > graphResponseLimit {
		return errors.New("Meta Graph API response exceeds size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope graphErrorEnvelope
		_ = json.Unmarshal(body, &envelope)
		envelope.Error.HTTPStatus = response.StatusCode
		if envelope.Error.Message == "" {
			envelope.Error.Message = http.StatusText(response.StatusCode)
		}
		return &envelope.Error
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode Meta Graph API response: %w", err)
	}
	return nil
}

type transportError struct {
	err error
}

func (e *transportError) Error() string {
	if e == nil || e.err == nil {
		return "Meta Graph API transport failure"
	}
	if errors.Is(e.err, context.DeadlineExceeded) {
		return "Meta Graph API request timed out"
	}
	return "Meta Graph API transport failure"
}

func (e *transportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}
