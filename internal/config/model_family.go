package config

import "strings"

// IsVideoModel identifies catalogue entries which require the long-running Veo
// API, rather than Gemini's anonymous streamGenerateContent operation.
func IsVideoModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimPrefix(model, "models/")
	return strings.HasPrefix(model, "veo-")
}
