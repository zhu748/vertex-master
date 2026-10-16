package vertex

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

// TestLiveGemini37FlashThinkingLevels verifies that the anonymous upstream
// accepts every officially supported Gemini 3.7 Flash thinking level
// (LOW/MEDIUM/HIGH; MINIMAL returns an API validation error per the official
// model docs, so it is intentionally absent). It is opt-in so normal
// unit-test runs never make external requests.
func TestLiveGemini37FlashThinkingLevels(t *testing.T) {
	if os.Getenv("VERTEX_LIVE_THINKING_TEST") == "" {
		t.Skip("set VERTEX_LIVE_THINKING_TEST=1 to run")
	}

	cfg := config.DefaultConfig()
	cfg.MaxRetries = 0
	cfg.ParallelPoolEnabled = false
	cfg.RequestTimeout = 60
	client := NewVertexAIClient(config.StaticProvider(cfg))

	for _, level := range []string{"LOW", "MEDIUM", "HIGH"} {
		t.Run(level, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			response, err := client.CompleteChat(ctx, "gemini-3.7-flash", map[string]any{
				"contents": []any{map[string]any{
					"role":  "user",
					"parts": []any{map[string]any{"text": "Reply with exactly OK."}},
				}},
				"generationConfig": map[string]any{
					"maxOutputTokens": 64,
					"thinkingConfig": map[string]any{
						"thinkingLevel": level,
					},
				},
			})
			if err != nil {
				t.Fatalf("gemini-3.7-flash thinkingLevel=%s rejected: %v", level, err)
			}
			if response == nil {
				t.Fatalf("gemini-3.7-flash thinkingLevel=%s returned nil response", level)
			}
			t.Logf("gemini-3.7-flash thinkingLevel=%s accepted", level)
		})
	}
}

// TestLiveGemini4ArgonThinkingLevels verifies that the anonymous upstream
// accepts every officially supported Gemini 4 Argon thinking level
// (LOW/MEDIUM/HIGH; MINIMAL and thinking_budget return HTTP 400 per the
// official model card, so they are intentionally absent). It is opt-in so
// normal unit-test runs never make external requests.
func TestLiveGemini4ArgonThinkingLevels(t *testing.T) {
	if os.Getenv("VERTEX_LIVE_THINKING_TEST") == "" {
		t.Skip("set VERTEX_LIVE_THINKING_TEST=1 to run")
	}

	cfg := config.DefaultConfig()
	cfg.MaxRetries = 0
	cfg.ParallelPoolEnabled = false
	cfg.RequestTimeout = 60
	client := NewVertexAIClient(config.StaticProvider(cfg))

	for _, level := range []string{"LOW", "MEDIUM", "HIGH"} {
		t.Run(level, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			response, err := client.CompleteChat(ctx, "gemini-4-argon", map[string]any{
				"contents": []any{map[string]any{
					"role":  "user",
					"parts": []any{map[string]any{"text": "Reply with exactly OK."}},
				}},
				"generationConfig": map[string]any{
					"maxOutputTokens": 64,
					"thinkingConfig": map[string]any{
						"thinkingLevel": level,
					},
				},
			})
			if err != nil {
				t.Fatalf("gemini-4-argon thinkingLevel=%s rejected: %v", level, err)
			}
			if response == nil {
				t.Fatalf("gemini-4-argon thinkingLevel=%s returned nil response", level)
			}
			t.Logf("gemini-4-argon thinkingLevel=%s accepted", level)
		})
	}
}
