package api

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

const (
	maxClaudePromptSettingBytes     = 1 << 20
	maxClaudePromptRecordBytes      = 1 << 20
	maxClaudePromptReplacementRules = 32
)

type claudePromptPolicyResult struct {
	OriginalPrompt   string
	EffectivePrompt  string
	HadSystem        bool
	ReplacementCount int
	ReplacementRules int
	MatchedRules     int
	RuleMatchCounts  []int
	InjectionApplied bool
}

// applyClaudePromptPolicy operates on the normalized Chat request produced
// from an Anthropic Messages request. Literal replacements are applied first,
// then the configured text is injected, so replacement rules cannot
// accidentally rewrite newly injected policy text.
func applyClaudePromptPolicy(
	chatBody map[string]any,
	cfg config.ConfigProvider,
) (claudePromptPolicyResult, error) {
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
		return result, nil
	}

	effective := original
	if cfg.ClaudePromptReplacementEnabled() {
		rules := cfg.ClaudePromptReplacementRules()
		if err := validateClaudePromptReplacementRules(rules); err != nil {
			return result, err
		}
		result.ReplacementRules = len(rules)
		result.RuleMatchCounts = make([]int, len(rules))
		limit := maxClaudeProcessedPromptBytes(cfg)
		for index, rule := range rules {
			if rule.From == "" {
				continue
			}
			matches := strings.Count(effective, rule.From)
			result.RuleMatchCounts[index] = matches
			if matches == 0 {
				continue
			}
			if !claudeReplacementSizeAllowed(
				len(effective),
				len(rule.From),
				len(rule.To),
				matches,
				limit,
			) {
				return result, fmt.Errorf(
					"claude system prompt exceeds %d bytes after replacement rule %d",
					limit,
					index+1,
				)
			}
			result.ReplacementCount += matches
			result.MatchedRules++
			effective = strings.ReplaceAll(effective, rule.From, rule.To)
		}
	}

	if cfg.ClaudePromptInjectionEnabled() {
		injection := cfg.ClaudePromptInjectionText()
		if injection != "" {
			result.InjectionApplied = true
			var combined string
			var err error
			if cfg.ClaudePromptInjectionPosition() == "prepend" {
				combined, err = joinClaudeSystemPromptsWithinLimit(
					injection,
					effective,
					maxClaudeProcessedPromptBytes(cfg),
				)
			} else {
				combined, err = joinClaudeSystemPromptsWithinLimit(
					effective,
					injection,
					maxClaudeProcessedPromptBytes(cfg),
				)
			}
			if err != nil {
				return result, err
			}
			effective = combined
		}
	}
	result.EffectivePrompt = effective

	if effective == original {
		return result, nil
	}
	rewritten := make([]any, 0, len(nonSystemMessages)+1)
	if effective != "" {
		rewritten = append(rewritten, map[string]any{
			"role": "system", "content": effective,
		})
	}
	rewritten = append(rewritten, nonSystemMessages...)
	chatBody["messages"] = rewritten
	return result, nil
}

func validateClaudePromptReplacementRules(
	rules []config.ClaudePromptReplacementRule,
) error {
	if len(rules) > maxClaudePromptReplacementRules {
		return fmt.Errorf(
			"configured Claude prompt replacement rules exceed the limit of %d",
			maxClaudePromptReplacementRules,
		)
	}
	seen := make(map[string]struct{}, len(rules))
	totalBytes := 0
	for index, rule := range rules {
		if rule.From == "" {
			return fmt.Errorf("claude prompt replacement rule %d has an empty source", index+1)
		}
		if _, duplicate := seen[rule.From]; duplicate {
			return fmt.Errorf("claude prompt replacement rule %d has a duplicate source", index+1)
		}
		seen[rule.From] = struct{}{}
		totalBytes += len(rule.From) + len(rule.To)
		if totalBytes > maxClaudePromptSettingBytes {
			return fmt.Errorf("configured Claude prompt replacement rules exceed 1 MiB")
		}
	}
	return nil
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

func joinClaudeSystemPromptsWithinLimit(first, second string, limit int) (string, error) {
	separatorBytes := 0
	if first != "" && second != "" {
		separatorBytes = 2
	}
	if len(first) > limit || len(second) > limit-len(first)-separatorBytes {
		return "", fmt.Errorf("claude system prompt exceeds %d bytes after injection", limit)
	}
	return joinClaudeSystemPrompts(first, second), nil
}

func maxClaudeProcessedPromptBytes(cfg config.ConfigProvider) int {
	const fallback = 64 << 20
	if cfg == nil || cfg.MaxRequestMB() < 1 {
		return fallback
	}
	return cfg.MaxRequestMB() << 20
}

func claudeReplacementSizeAllowed(current, from, to, matches, limit int) bool {
	if current > limit {
		return false
	}
	if matches == 0 || to <= from {
		return true
	}
	return matches <= (limit-current)/(to-from)
}

type claudePromptRecord struct {
	OriginalPrompt     string
	EffectivePrompt    string
	Model              string
	Endpoint           string
	ReceivedAt         time.Time
	HadSystem          bool
	ReplacementCount   int
	ReplacementRules   int
	MatchedRules       int
	RuleMatchCounts    []int
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
	ruleMatchCounts := append([]int(nil), result.RuleMatchCounts...)
	s.latest.Store(&claudePromptRecord{
		OriginalPrompt:     original,
		EffectivePrompt:    effective,
		Model:              model,
		Endpoint:           endpoint,
		ReceivedAt:         time.Now().UTC(),
		HadSystem:          result.HadSystem,
		ReplacementCount:   result.ReplacementCount,
		ReplacementRules:   result.ReplacementRules,
		MatchedRules:       result.MatchedRules,
		RuleMatchCounts:    ruleMatchCounts,
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
	copyOfRecord := *record
	copyOfRecord.RuleMatchCounts = append([]int(nil), record.RuleMatchCounts...)
	return copyOfRecord, true
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
			"replacement_rules":   record.ReplacementRules,
			"matched_rules":       record.MatchedRules,
			"rule_match_counts":   record.RuleMatchCounts,
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
