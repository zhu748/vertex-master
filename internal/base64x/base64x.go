package base64x

import (
	"encoding/base64"
	"strings"
)

// DecodeString accepts standard and URL-safe Base64, with or without padding.
// CR/LF handling matches encoding/base64; mixed alphabets retain the legacy
// normalization fallback used by imported proxy subscriptions.
func DecodeString(value string) ([]byte, error) {
	hasURLAlphabet := strings.IndexByte(value, '-') >= 0 || strings.IndexByte(value, '_') >= 0
	payloadLength := len(value)
	encoding := encodingFor(hasURLAlphabet, payloadLength)
	if decoded, err := encoding.DecodeString(value); err == nil {
		return decoded, nil
	}

	if strings.IndexByte(value, '\r') >= 0 || strings.IndexByte(value, '\n') >= 0 {
		payloadLength = 0
		for index := range len(value) {
			if value[index] != '\r' && value[index] != '\n' {
				payloadLength++
			}
		}
		withoutLineBreaks := encodingFor(hasURLAlphabet, payloadLength)
		if withoutLineBreaks != encoding {
			if decoded, err := withoutLineBreaks.DecodeString(value); err == nil {
				return decoded, nil
			}
		}
	}

	normalized := strings.ReplaceAll(strings.ReplaceAll(value, "-", "+"), "_", "/")
	if padding := payloadLength % 4; padding != 0 {
		normalized += strings.Repeat("=", 4-padding)
	}
	return base64.StdEncoding.DecodeString(normalized) //nolint:wrapcheck
}

func encodingFor(urlAlphabet bool, payloadLength int) *base64.Encoding {
	if urlAlphabet {
		if payloadLength%4 != 0 {
			return base64.RawURLEncoding
		}
		return base64.URLEncoding
	}
	if payloadLength%4 != 0 {
		return base64.RawStdEncoding
	}
	return base64.StdEncoding
}
