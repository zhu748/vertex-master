package api

import "fmt"

// normalizeGeminiRequestBody unwraps CountTokens-style request envelopes and
// rejects values that the compatibility normalizer would otherwise discard.
// String and object shorthand remain supported for existing clients.
func normalizeGeminiRequestBody(body map[string]any) (map[string]any, error) {
	if raw, exists := body["generateContentRequest"]; exists {
		request, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("generateContentRequest must be an object")
		}
		body = request
	}
	if raw, exists := body["contents"]; exists {
		if err := validateGeminiContents(raw, "contents"); err != nil {
			return nil, err
		}
	}
	return body, nil
}

func validateGeminiContents(value any, path string) error {
	switch contents := value.(type) {
	case string:
		return nil
	case map[string]any:
		return validateGeminiContent(contents, path)
	case []any:
		for index, raw := range contents {
			itemPath := fmt.Sprintf("%s[%d]", path, index)
			switch content := raw.(type) {
			case string:
			case map[string]any:
				if err := validateGeminiContent(content, itemPath); err != nil {
					return err
				}
			default:
				return fmt.Errorf("%s must be a string or object", itemPath)
			}
		}
		return nil
	default:
		return fmt.Errorf("%s must be a string, object, or array", path)
	}
}

func validateGeminiContent(content map[string]any, path string) error {
	parts, hasParts := content["parts"]
	legacyContent, hasLegacyContent := content["content"]
	if hasParts && hasLegacyContent {
		return fmt.Errorf("%s cannot contain both parts and content", path)
	}
	switch {
	case hasParts:
		return validateGeminiParts(parts, path+".parts")
	case hasLegacyContent:
		return validateGeminiParts(legacyContent, path+".content")
	default:
		return nil
	}
}

func validateGeminiParts(value any, path string) error {
	switch parts := value.(type) {
	case nil, string, map[string]any:
		return nil
	case []any:
		for index, raw := range parts {
			switch raw.(type) {
			case string, map[string]any:
			default:
				return fmt.Errorf("%s[%d] must be a string or object", path, index)
			}
		}
		return nil
	default:
		return fmt.Errorf("%s must be a string, object, or array", path)
	}
}
