package deepl

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestNewClientSelectsEndpointFromKey(t *testing.T) {
	if got := NewClient("free-key:fx").baseURL; got != freeAPIBaseURL {
		t.Fatalf("free base URL = %q", got)
	}
	if got := NewClient("pro-key").baseURL; got != proAPIBaseURL {
		t.Fatalf("pro base URL = %q", got)
	}
}

func TestTranslateSendsExpectedRequest(t *testing.T) {
	client := NewClient("test-key")
	client.baseURL = "https://deepl.test"
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/translate" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "DeepL-Auth-Key test-key" {
			t.Fatalf("authorization = %q", got)
		}
		var body translateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Text) != 1 || body.Text[0] != "Привіт" || body.SourceLanguage != "UK" || body.TargetLanguage != "EN-GB" || !body.PreserveFormatting {
			t.Fatalf("body = %#v", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"translations":[{"text":"Hello"}]}`)),
		}, nil
	})}
	translated, err := client.Translate(context.Background(), "Привіт", "UK", "EN-GB")
	if err != nil {
		t.Fatal(err)
	}
	if translated != "Hello" {
		t.Fatalf("translation = %q", translated)
	}
}

func TestTranslateReturnsTypedProviderError(t *testing.T) {
	client := NewClient("test-key")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"message":"rate limited"}`)),
		}, nil
	})}
	_, err := client.Translate(context.Background(), "Cześć", "PL", "EN-GB")
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("error = %#v", err)
	}
}
