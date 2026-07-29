package api

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

const (
	maxClaudePromptSettingBytes = 1 << 20
	maxClaudePromptRecordBytes  = 1 << 20
)

type claudePromptPolicyResult struct {
	OriginalPrompt   string
	EffectivePrompt  string
	HadSystem        bool
	ReplacementCount int
	InjectionApplied bool
}

// applyClaudePromptPolicy operates on the normalized Chat request produced
// from an Anthropic Messages request. Literal replacements are applied first,
// then the configured text is injected, so replacement rules cannot
// accidentally rewrite newly injected policy text.
func applyClaudePromptPolicy(
	chatBody map[string]any,
	cfg config.ConfigProvider,
) claudePromptPolicyResult {
	messages, _ := chatBody["messages"].([]any)
	systemTexts := make([]string, 0, 2)
	nonSystemMessages := make([]any, 0, len(messages))
	hadSystem := false
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		role := stringValue(message["role"])
		if message != nil && (role == "system" || role == "developer") {
			hadSystem = true
			if text, ok := message["content"].(string); ok && text != "" {
				systemTexts = append(systemTexts, text)
			}
			continue
		}
		nonSystemMessages = append(nonSystemMessages, rawMessage)
	}

	original := strings.Join(systemTexts, "\n\n")
	result := claudePromptPolicyResult{
		OriginalPrompt:  original,
		EffectivePrompt: original,
		HadSystem:       hadSystem,
	}
	if cfg == nil {
		return result
	}

	effective := original
	if cfg.ClaudePromptReplacementEnabled() {
		from := cfg.ClaudePromptReplaceFrom()
		if from != "" {
			result.ReplacementCount = strings.Count(effective, from)
			if result.ReplacementCount > 0 {
				effective = strings.ReplaceAll(effective, from, cfg.ClaudePromptReplaceTo())
			}
		}
	}

	if cfg.ClaudePromptInjectionEnabled() {
		injection := cfg.ClaudePromptInjectionText()
		if injection != "" {
			result.InjectionApplied = true
			if cfg.ClaudePromptInjectionPosition() == "prepend" {
				effective = joinClaudeSystemPrompts(injection, effective)
			} else {
				effective = joinClaudeSystemPrompts(effective, injection)
			}
		}
	}
	result.EffectivePrompt = effective

	if effective == original {
		return result
	}
	rewritten := make([]any, 0, len(nonSystemMessages)+1)
	if effective != "" {
		rewritten = append(rewritten, map[string]any{
			"role": "system", "content": effective,
		})
	}
	rewritten = append(rewritten, nonSystemMessages...)
	chatBody["messages"] = rewritten
	return result
}

func joinClaudeSystemPrompts(first, second string) string {
	switch {
	case first == "":
		return second
	case second == "":
		return first
	default:
		return first + "\n\n" + second
	}
}

type claudePromptRecord struct {
	OriginalPrompt     string
	EffectivePrompt    string
	Model              string
	Endpoint           string
	ReceivedAt         time.Time
	HadSystem          bool
	ReplacementCount   int
	InjectionApplied   bool
	OriginalBytes      int
	EffectiveBytes     int
	OriginalTruncated  bool
	EffectiveTruncated bool
}

type claudePromptStore struct {
	latest atomic.Pointer[claudePromptRecord]
}

func (s *claudePromptStore) Record(
	model string,
	endpoint string,
	result claudePromptPolicyResult,
) {
	if s == nil {
		return
	}
	original, originalTruncated := truncateClaudePromptRecord(result.OriginalPrompt)
	effective, effectiveTruncated := truncateClaudePromptRecord(result.EffectivePrompt)
	s.latest.Store(&claudePromptRecord{
		OriginalPrompt:     original,
		EffectivePrompt:    effective,
		Model:              model,
		Endpoint:           endpoint,
		ReceivedAt:         time.Now().UTC(),
		HadSystem:          result.HadSystem,
		ReplacementCount:   result.ReplacementCount,
		InjectionApplied:   result.InjectionApplied,
		OriginalBytes:      len(result.OriginalPrompt),
		EffectiveBytes:     len(result.EffectivePrompt),
		OriginalTruncated:  originalTruncated,
		EffectiveTruncated: effectiveTruncated,
	})
}

func (s *claudePromptStore) Latest() (claudePromptRecord, bool) {
	if s == nil {
		return claudePromptRecord{}, false
	}
	record := s.latest.Load()
	if record == nil {
		return claudePromptRecord{}, false
	}
	return *record, true
}

func (s *claudePromptStore) Clear() {
	if s != nil {
		s.latest.Store(nil)
	}
}

func truncateClaudePromptRecord(value string) (string, bool) {
	if len(value) <= maxClaudePromptRecordBytes {
		return value, false
	}
	end := maxClaudePromptRecordBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end], true
}

func (adm *AdminHandler) adminClaudePromptLatest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		record, ok := adm.claudePrompts.Latest()
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"available": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"available":           true,
			"original_prompt":     record.OriginalPrompt,
			"effective_prompt":    record.EffectivePrompt,
			"model":               record.Model,
			"endpoint":            record.Endpoint,
			"received_at":         record.ReceivedAt.Format(time.RFC3339Nano),
			"had_system":          record.HadSystem,
			"replacement_count":   record.ReplacementCount,
			"injection_applied":   record.InjectionApplied,
			"original_bytes":      record.OriginalBytes,
			"effective_bytes":     record.EffectiveBytes,
			"original_truncated":  record.OriginalTruncated,
			"effective_truncated": record.EffectiveTruncated,
		})
	case http.MethodDelete:
		adm.claudePrompts.Clear()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		adm.adminMethodNotAllowed(w)
	}
}
