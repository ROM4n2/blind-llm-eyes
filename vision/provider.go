package vision

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ROM4n2/blind-llm-eyes/config"
	"github.com/ROM4n2/blind-llm-eyes/metrics"
)

// VisionProvider 是视觉描述服务的抽象接口。
// 任何视觉后端（MiMo、GPT-4o、Claude Vision 等）只要实现此接口即可无缝替换。
type VisionProvider interface {
	// DescribeImage 将一张 base64 编码的图片转换为文字描述。
	// imageSize 是原始字节数（base64 解码后），用于调用方决定超时策略。
	DescribeImage(ctx context.Context, base64Data, mediaType string, imageSize int64) (string, error)
}

// ContextualVisionProvider 是 VisionProvider 的可选扩展：支持带对话上下文描述图片。
// handler 和 pool 层会先断言此接口，成功则调用 DescribeImageWithContext，
// 失败则回退到 VisionProvider.DescribeImage（无上下文行为）。
// 这样 VisionProvider 接口零改动，所有现有 provider/mock 自动兼容。
type ContextualVisionProvider interface {
	// DescribeImageWithContext 带对话上下文描述图片。
	// contextText 是最近 N 轮对话的纯文本摘要，可为空（等价于 DescribeImage）。
	DescribeImageWithContext(ctx context.Context, base64Data, mediaType string, imageSize int64, contextText string) (string, error)
}

// Pinger is an optional interface for providers that support a lightweight
// connectivity/auth check without sending an image. The doctor/setup flows
// type-assert to Pinger; providers that don't implement it are skipped.
//
// Ping semantics: a network/timeout error or a 401/403 response is a failure
// (endpoint unreachable or auth invalid). Any other HTTP response is a pass
// (endpoint reachable + auth OK). A model-level 400 (e.g. max_tokens too small)
// is NOT a failure — it proves the endpoint accepted and authenticated the call.
type Pinger interface {
	Ping(ctx context.Context) error
}

// GLM-4V-Flash free-tier defaults (Zhipu AI BigModel platform).
// The model is free to use but still requires an API key from
// https://open.bigmodel.cn — the "free" refers to zero cost per call,
// removing the payment barrier for onboarding.
const (
	GLMFreeBaseURL = "https://open.bigmodel.cn/api/paas/v4"
	GLMFreeModel   = "glm-4v-flash"
)

// BuildProvider constructs a single VisionProvider from a pool-style ProviderCfg.
// It validates the required fields (base_url, api_key, model) and dispatches on
// Type:
//   - "mimo" = Anthropic Messages API
//   - "openai_compatible" = OpenAI /v1/chat/completions
//   - "glm_free" = GLM-4V-Flash free tier (auto-fills base_url + model)
//
// For "glm_free", base_url and model are optional (auto-filled with
// GLMFreeBaseURL / GLMFreeModel); api_key is still required.
// Safe to call multiple times in one process (no global metric registration).
func BuildProvider(pc config.ProviderCfg, logger *slog.Logger) (VisionProvider, error) {
	// glm_free auto-fills base_url and model; other types require all three.
	if pc.Type == "glm_free" {
		if pc.BaseURL == "" {
			pc.BaseURL = GLMFreeBaseURL
		}
		if pc.Model == "" {
			pc.Model = GLMFreeModel
		}
	}
	if pc.BaseURL == "" {
		return nil, fmt.Errorf("provider %q: base_url is required", pc.Name)
	}
	if pc.APIKey == "" {
		return nil, fmt.Errorf("provider %q: api_key is required", pc.Name)
	}
	if pc.Model == "" {
		return nil, fmt.Errorf("provider %q: model is required", pc.Name)
	}
	switch pc.Type {
	case "mimo":
		return NewClient(
			strings.TrimRight(pc.BaseURL, "/"),
			pc.APIKey, pc.Model, pc.Timeout, pc.LargeTimeout,
			pc.LargeImageThreshold, pc.DescriptionCap, pc.SupportedFormats, logger,
		), nil
	case "openai_compatible", "glm_free":
		return NewOpenAIClient(
			strings.TrimRight(pc.BaseURL, "/"),
			pc.APIKey, pc.Model, pc.Timeout, pc.LargeTimeout,
			pc.LargeImageThreshold, pc.DescriptionCap, pc.SupportedFormats, logger,
		), nil
	default:
		return nil, fmt.Errorf("provider %q: unknown type %q (want \"mimo\", \"openai_compatible\", or \"glm_free\")", pc.Name, pc.Type)
	}
}

// BuildSingleProvider constructs a single VisionProvider from a VisionCfg
// (single-provider mode). It validates the required fields and builds a MiMo
// (Anthropic Messages API) client.
func BuildSingleProvider(vc config.VisionCfg, logger *slog.Logger) (VisionProvider, error) {
	if vc.BaseURL == "" {
		return nil, fmt.Errorf("vision: base_url is required")
	}
	if vc.APIKey == "" {
		return nil, fmt.Errorf("vision: api_key is required")
	}
	if vc.Model == "" {
		return nil, fmt.Errorf("vision: model is required")
	}
	return NewClient(
		strings.TrimRight(vc.BaseURL, "/"),
		vc.APIKey, vc.Model, vc.Timeout, vc.LargeTimeout,
		vc.LargeImageThreshold, vc.DescriptionCap, vc.SupportedFormats, logger,
	), nil
}

// BuildPool constructs a multi-provider Pool from a list of ProviderCfg entries.
// Each entry is built via BuildProvider and wrapped with a circuit breaker.
// logger and m may be nil (NewPool applies sensible defaults).
func BuildPool(providers []config.ProviderCfg, logger *slog.Logger, m *metrics.Metrics) (*Pool, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("vision_providers is empty")
	}
	entries := make([]PoolEntry, 0, len(providers))
	for _, pc := range providers {
		p, err := BuildProvider(pc, logger)
		if err != nil {
			return nil, err
		}
		entries = append(entries, PoolEntry{
			Name:                pc.Name,
			Provider:            p,
			Priority:            pc.Priority,
			CB:                  NewCircuitBreaker(pc.CircuitBreaker.FailureThreshold, pc.CircuitBreaker.ResetTimeout),
			Timeout:             pc.Timeout,
			LargeTimeout:        pc.LargeTimeout,
			LargeImageThreshold: pc.LargeImageThreshold,
		})
	}
	return NewPool(entries, logger, m)
}
