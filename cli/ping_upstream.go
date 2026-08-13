package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// upstreamPingTimeout is the short deadline for an upstream connectivity/auth probe.
const upstreamPingTimeout = 10 * time.Second

// PingUpstream probes an Anthropic-compatible upstream endpoint (POST baseURL +
// /v1/messages) with a text-only request (max_tokens=1) to verify reachability
// and authentication.
//
// Ping semantics (same as vision.Pinger): a network/timeout error or a 401/403
// response is a failure (endpoint unreachable or auth invalid). Any other HTTP
// response is a pass (endpoint reachable + auth OK). A model-level 400 (e.g.
// max_tokens too small) is NOT a failure — it proves the endpoint accepted and
// authenticated the call.
//
// model is the model name to put in the request body (the upstream may reject
// it with a model-level error, which is still a pass). baseURL should not have
// a trailing slash.
func PingUpstream(ctx context.Context, baseURL, apiKey, model string) error {
	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "ping"}}},
		},
		"max_tokens": 1,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("upstream ping: marshal: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, upstreamPingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pingCtx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/v1/messages", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("upstream ping: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	return classifyUpstreamPing(resp, err)
}

// classifyUpstreamPing applies the ping semantics to an HTTP response/error pair.
func classifyUpstreamPing(resp *http.Response, err error) error {
	if err != nil {
		return fmt.Errorf("upstream: unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("upstream: auth failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}
