package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"math"
	"sort"
)

// promptDiagnosticKey deliberately changes at each process start. The HMAC
// can correlate an initial generation with a reroll in the same log without
// publishing a stable hash of private prompt text.
var promptDiagnosticKey = newPromptDiagnosticKey() //nolint:gochecknoglobals

type promptDiagnostic struct {
	Fingerprint   string
	Turns         int
	UserTurns     int
	ModelTurns    int
	FunctionTurns int
	SystemBytes   int
	TextBytes     int
	NonTextParts  int
}

func summarizePrompt(payload map[string]any) promptDiagnostic {
	var out promptDiagnostic
	digest := hmac.New(sha256.New, promptDiagnosticKey[:])
	writeDiagnosticBytes(digest, []byte("vproxy-prompt-diagnostic-v1\x00"))

	if system, ok := payload["systemInstruction"].(map[string]any); ok {
		writeDiagnosticParts(system["parts"], true, &out)
	}
	if contents, ok := payload["contents"].([]any); ok {
		for _, rawContent := range contents {
			content, ok := rawContent.(map[string]any)
			if !ok {
				continue
			}
			role, _ := content["role"].(string)
			out.Turns++
			switch role {
			case "user":
				out.UserTurns++
			case "model":
				out.ModelTurns++
			case "function", "tool":
				out.FunctionTurns++
			}
			writeDiagnosticParts(content["parts"], false, &out)
		}
	}
	// Hash the complete normalized prompt tree, including media references,
	// function arguments/results, thought markers and all role/part metadata.
	writeCanonicalDiagnosticValue(digest, payload["systemInstruction"])
	writeCanonicalDiagnosticValue(digest, payload["contents"])
	sum := digest.Sum(nil)
	out.Fingerprint = hex.EncodeToString(sum[:16])
	return out
}

func writeDiagnosticParts(
	rawParts any,
	system bool,
	out *promptDiagnostic,
) {
	parts, _ := rawParts.([]any)
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := part["text"].(string); ok {
			out.TextBytes += len(text)
			if system {
				out.SystemBytes += len(text)
			}
			continue
		}
		out.NonTextParts++
	}
}

func newPromptDiagnosticKey() [32]byte {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		panic("generate prompt diagnostic key: " + err.Error())
	}
	return key
}

func writeCanonicalDiagnosticValue(dst hash.Hash, value any) {
	switch typed := value.(type) {
	case nil:
		writeDiagnosticBytes(dst, []byte{'n'})
	case bool:
		if typed {
			writeDiagnosticBytes(dst, []byte{'b', 1})
		} else {
			writeDiagnosticBytes(dst, []byte{'b', 0})
		}
	case string:
		writeDiagnosticString(dst, 's', typed)
	case float64:
		var encoded [9]byte
		encoded[0] = 'f'
		binary.BigEndian.PutUint64(encoded[1:], math.Float64bits(typed))
		writeDiagnosticBytes(dst, encoded[:])
	case map[string]any:
		writeDiagnosticBytes(dst, []byte{'m'})
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		writeDiagnosticLength(dst, len(keys))
		for _, key := range keys {
			writeDiagnosticString(dst, 'k', key)
			writeCanonicalDiagnosticValue(dst, typed[key])
		}
	case []any:
		writeDiagnosticBytes(dst, []byte{'a'})
		writeDiagnosticLength(dst, len(typed))
		for _, item := range typed {
			writeCanonicalDiagnosticValue(dst, item)
		}
	default:
		// Converted HTTP prompts use only the cases above. Keep diagnostics
		// deterministic for tests/custom converters that insert typed values.
		writeDiagnosticBytes(dst, []byte{'j'})
		encoded, err := json.Marshal(typed)
		if err != nil {
			writeDiagnosticString(dst, 'e', err.Error())
			return
		}
		writeDiagnosticLength(dst, len(encoded))
		writeDiagnosticBytes(dst, encoded)
	}
}

func writeDiagnosticString(dst hash.Hash, marker byte, value string) {
	writeDiagnosticBytes(dst, []byte{marker})
	writeDiagnosticLength(dst, len(value))
	writeDiagnosticBytes(dst, []byte(value))
}

func writeDiagnosticLength(dst hash.Hash, length int) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(length))
	writeDiagnosticBytes(dst, encoded[:])
}

func writeDiagnosticBytes(dst hash.Hash, value []byte) {
	_, _ = dst.Write(value)
}
