package vertex

import (
	"errors"
	"strings"
)

var ( //nolint:gochecknoglobals // Sentinel errors for errors.Is.
	ErrStreamIncomplete  = errors.New("upstream stream ended without a terminal finish reason")
	errInvalidStreamJSON = errors.New("upstream stream contains invalid JSON")
)

func newStreamProtocolError() *VertexError {
	return &VertexError{Code: 502, Status: StatusUnavailable, Kind: "protocol",
		Message: errInvalidStreamJSON.Error(), cause: errInvalidStreamJSON}
}

// Track the upstream signal, before any compatibility adapter can synthesize STOP.
// Prompt rejection is also a valid terminal response and need not have candidates.
type streamIntegrity struct {
	payload  bool
	terminal bool
	blocked  bool
	overflow bool
	others   map[int]candidateIntegrity // Allocated only for nonzero candidate indexes; bounded against malformed streams.
}

type candidateIntegrity struct{ payload, terminal bool }

func (s *streamIntegrity) observe(chunk StreamChunk) {
	if chunk.HasCanonicalText {
		s.payload = s.payload || chunk.CanonicalText.Text != ""
		s.terminal = s.terminal || isTerminalFinish(chunk.CanonicalText.FinishReason)
		return
	}
	if feedback, ok := chunk.Data["promptFeedback"].(map[string]any); ok {
		reason := strings.ToUpper(strings.TrimSpace(toStr(feedback["blockReason"])))
		if reason != "" && !isUnspecifiedBlockReason(reason) {
			s.blocked = true
		}
	}
	if candidates, ok := chunk.Data["candidates"].([]any); ok {
		for position, raw := range candidates {
			if candidate, ok := raw.(map[string]any); ok {
				index := toInt(candidate["index"], position)
				payload := streamCandidateHasPayload(candidate)
				terminal := isTerminalFinish(toStr(candidate["finishReason"]))
				if index == 0 {
					s.payload = s.payload || payload
					s.terminal = s.terminal || terminal
				} else if payload || terminal {
					previous, exists := s.others[index]
					if !exists && len(s.others) >= 64 {
						s.overflow = true
						continue
					}
					if s.others == nil {
						s.others = make(map[int]candidateIntegrity)
					}
					s.others[index] = candidateIntegrity{payload: previous.payload || payload, terminal: previous.terminal || terminal}
				}
			}
		}
	}
}

func isTerminalFinish(reason string) bool {
	reason = strings.ToUpper(strings.TrimSpace(reason))
	return reason != "" && !strings.Contains(reason, "UNSPECIFIED")
}

func (s *streamIntegrity) err() *VertexError {
	if s.blocked {
		return nil
	}
	incomplete := s.overflow || (s.payload && !s.terminal)
	for _, candidate := range s.others {
		incomplete = incomplete || (candidate.payload && !candidate.terminal)
	}
	if incomplete {
		return &VertexError{Code: 502, Status: StatusUnavailable, Kind: "incomplete",
			Message: ErrStreamIncomplete.Error(), cause: ErrStreamIncomplete}
	}
	return nil
}
