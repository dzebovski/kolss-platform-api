package botcheck

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

var (
	ErrVerificationFailed = errors.New("bot verification failed")
	ErrProviderUnavailable = errors.New("bot verification provider unavailable")
)

// BotVerifier validates anti-bot tokens. Implementations must never log the token.
type BotVerifier interface {
	Verify(ctx context.Context, token, remoteIP string) error
}

type TurnstileConfig struct {
	SecretKey       string
	AllowedHostnames map[string]struct{}
	ExpectedAction  string
	HTTPClient      *http.Client
	SiteverifyURL   string
}

type TurnstileVerifier struct {
	secret          string
	allowedHostnames map[string]struct{}
	expectedAction  string
	client          *http.Client
	endpoint        string
}

func NewTurnstileVerifier(cfg TurnstileConfig) *TurnstileVerifier {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	endpoint := cfg.SiteverifyURL
	if endpoint == "" {
		endpoint = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	}
	action := cfg.ExpectedAction
	if action == "" {
		action = "lead_submission"
	}
	return &TurnstileVerifier{
		secret:           cfg.SecretKey,
		allowedHostnames: cfg.AllowedHostnames,
		expectedAction:   action,
		client:           client,
		endpoint:         endpoint,
	}
}

type siteverifyResponse struct {
	Success    bool     `json:"success"`
	Action     string   `json:"action"`
	Hostname   string   `json:"hostname"`
	ErrorCodes []string `json:"error-codes"`
}

func (v *TurnstileVerifier) Verify(ctx context.Context, token, remoteIP string) error {
	token = strings.TrimSpace(token)
	if token == "" || v.secret == "" {
		return ErrVerificationFailed
	}

	form := url.Values{}
	form.Set("secret", v.secret)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ErrProviderUnavailable
	}

	var parsed siteverifyResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	if !parsed.Success {
		return ErrVerificationFailed
	}
	if parsed.Action != v.expectedAction {
		return ErrVerificationFailed
	}
	hostname := strings.ToLower(strings.TrimSpace(parsed.Hostname))
	if _, ok := v.allowedHostnames[hostname]; !ok {
		return ErrVerificationFailed
	}
	return nil
}

// DisabledVerifier always succeeds. Use only with BOTCHECK_DISABLED=true for local/tests.
type DisabledVerifier struct{}

func (DisabledVerifier) Verify(context.Context, string, string) error { return nil }

func HostnamesSet(hostnames []string) map[string]struct{} {
	out := make(map[string]struct{}, len(hostnames))
	for _, h := range hostnames {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			out[h] = struct{}{}
		}
	}
	return out
}
