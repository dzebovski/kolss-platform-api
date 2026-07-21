package metaleads

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestClientBuildsVersionedSignedRequestsAndUsesCursor(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v25.0/page-kyiv/leadgen_forms" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer page-token" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("appsecret_proof") != AppSecretProof("app-secret", "page-token") {
			t.Fatal("missing or invalid appsecret_proof")
		}
		if r.URL.Query().Get("after") != "cursor-1" {
			t.Fatalf("after=%q", r.URL.Query().Get("after"))
		}
		body, _ := json.Marshal(map[string]any{
			"data": []map[string]any{{"id": "form-1", "name": "Form"}},
			"paging": map[string]any{
				"cursors": map[string]string{"after": "cursor-2"},
				"next":    "https://untrusted.example/never-followed",
			},
		})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	client := NewClient(Config{
		GraphAPIVersion: "v25.0",
		AppSecret:       "app-secret",
		Pages:           []Page{{OfficeCode: "kyiv", PageID: "page-kyiv", Token: "page-token"}},
	})
	client.BaseURL = "https://graph.facebook.test"
	client.HTTP = httpClient
	forms, next, err := client.ListForms(context.Background(), "page-kyiv", "cursor-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(forms) != 1 || forms[0].ID != "form-1" || next != "cursor-2" {
		t.Fatalf("forms=%#v next=%q", forms, next)
	}
}

func TestClientClassifiesOAuthError(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": "expired token", "type": "OAuthException", "code": 190,
		}})
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	client := NewClient(Config{
		GraphAPIVersion: "v25.0",
		AppSecret:       "app-secret",
		Pages:           []Page{{PageID: "page-kyiv", Token: "page-token"}},
	})
	client.BaseURL = "https://graph.facebook.test"
	client.HTTP = httpClient
	_, err := client.FetchLead(context.Background(), "page-kyiv", "lead-1")
	graphErr, ok := err.(*GraphError)
	if !ok || !graphErr.OAuth() || graphErr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("error=%#v", err)
	}
}

func TestFetchLeadPreservesUnknownGraphFieldsInRawPayload(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"id":"lead-1","field_data":[],"future_meta_field":{"value":42}}`,
			)),
			Header: make(http.Header),
		}, nil
	})}
	client := NewClient(Config{
		GraphAPIVersion: "v25.0",
		AppSecret:       "app-secret",
		Pages:           []Page{{PageID: "page-kyiv", Token: "page-token"}},
	})
	client.HTTP = httpClient
	lead, err := client.FetchLead(context.Background(), "page-kyiv", "lead-1")
	if err != nil {
		t.Fatal(err)
	}
	if lead.ID != "lead-1" || !strings.Contains(string(lead.RawPayload), "future_meta_field") {
		t.Fatalf("lead=%#v raw=%s", lead, lead.RawPayload)
	}
}

func TestClientDoesNotExposeSignedRequestURLOnTransportFailure(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("request failed for https://graph.example?appsecret_proof=sensitive")
	})}
	client := NewClient(Config{
		GraphAPIVersion: "v25.0",
		AppSecret:       "app-secret",
		Pages:           []Page{{PageID: "page-kyiv", Token: "page-token"}},
	})
	client.HTTP = httpClient
	_, err := client.FetchLead(context.Background(), "page-kyiv", "lead-1")
	if err == nil || strings.Contains(err.Error(), "appsecret_proof") || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unsafe error=%v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
