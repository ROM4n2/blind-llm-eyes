package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// pingTimeout is the short deadline for a connectivity/auth probe.
const pingTimeout = 10 * time.Second

// classifyPing applies the ping semantics to an HTTP response/error pair (see
// the Pinger interface docs). A network/timeout error or 401/403 is a failure;
// any other response is a pass, with non-2xx logged as a warning.
func classifyPing(resp *http.Response, err error, log *slog.Logger, label string) error {
	if err != nil {
		return fmt.Errorf("%s: unreachable: %w", label, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s: auth failed (HTTP %d): %s", label, resp.StatusCode, string(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if log != nil {
			log.Warn("ping got non-2xx (treated as reachable)",
				"label", label,
				"status", resp.StatusCode,
				"body", string(body),
			)
		}
	}
	return nil
}

// Ping probes the MiMo (Anthropic Messages API) endpoint with a text-only
// request (max_tokens=1) to verify reachability and authentication.
func (c *Client) Ping(ctx context.Context) error {
	body := map[string]any{
		"model": c.Model,
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "ping"}}},
		},
		"max_tokens": 1,
		"thinking":   map[string]string{"type": "disabled"},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("mimo ping: marshal: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pingCtx, http.MethodPost, c.BaseURL+"/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("mimo ping: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	resp, err := httpClient.Do(req)
	return classifyPing(resp, err, c.Log, "mimo")
}

// Ping probes the OpenAI-compatible endpoint with a text-only request.
func (c *OpenAIClient) Ping(ctx context.Context) error {
	body := map[string]any{
		"model": c.Model,
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "ping"}}},
		},
		"max_tokens": 1,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("openai ping: marshal: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pingCtx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("openai ping: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	resp, err := httpClient.Do(req)
	return classifyPing(resp, err, c.Log, "openai")
}
