package deepl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	freeAPIBaseURL = "https://api-free.deepl.com"
	proAPIBaseURL  = "https://api.deepl.com"
)

type APIError struct {
	StatusCode int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("DeepL request failed with status %d", e.StatusCode)
}

type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

func NewClient(apiKey string) *Client {
	key := strings.TrimSpace(apiKey)
	baseURL := proAPIBaseURL
	if strings.HasSuffix(key, ":fx") {
		baseURL = freeAPIBaseURL
	}
	return &Client{
		apiKey:  key,
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type translateRequest struct {
	Text               []string `json:"text"`
	SourceLanguage     string   `json:"source_lang"`
	TargetLanguage     string   `json:"target_lang"`
	PreserveFormatting bool     `json:"preserve_formatting"`
}

type translateResponse struct {
	Translations []struct {
		Text string `json:"text"`
	} `json:"translations"`
}

func (c *Client) Translate(
	ctx context.Context,
	text string,
	sourceLanguage string,
	targetLanguage string,
) (string, error) {
	if strings.TrimSpace(c.apiKey) == "" {
		return "", errors.New("DeepL API key is not configured")
	}
	payload, err := json.Marshal(translateRequest{
		Text:               []string{text},
		SourceLanguage:     sourceLanguage,
		TargetLanguage:     targetLanguage,
		PreserveFormatting: true,
	})
	if err != nil {
		return "", fmt.Errorf("encode DeepL request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(c.baseURL, "/")+"/v2/translate",
		bytes.NewReader(payload),
	)
	if err != nil {
		return "", fmt.Errorf("create DeepL request: %w", err)
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Kolss-CRM/1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send DeepL request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		return "", &APIError{StatusCode: resp.StatusCode}
	}

	var result translateResponse
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 256*1024))
	if err := decoder.Decode(&result); err != nil {
		return "", fmt.Errorf("decode DeepL response: %w", err)
	}
	if len(result.Translations) != 1 || strings.TrimSpace(result.Translations[0].Text) == "" {
		return "", errors.New("DeepL returned an empty translation")
	}
	return strings.TrimSpace(result.Translations[0].Text), nil
}
