package base64x

import (
	"encoding/base64"
	"testing"
)

func TestDecodeStringVariants(t *testing.T) {
	payload := []byte{0xfb, 0xff, 0xbf, 0x68, 0x65, 0x6c, 0x6c, 0x6f}
	standard := base64.StdEncoding.EncodeToString(payload)
	rawStandard := base64.RawStdEncoding.EncodeToString(payload)
	urlSafe := base64.URLEncoding.EncodeToString(payload)

	for name, encoded := range map[string]string{
		"standard padded": standard,
		"standard raw":    rawStandard,
		"URL-safe padded": urlSafe,
		"URL-safe raw":    base64.RawURLEncoding.EncodeToString(payload),
		"mixed alphabets": "+_+/aGVsbG8=",
		"line breaks":     standard[:4] + "\r\n" + standard[4:],
		"raw line breaks": rawStandard[:4] + "\n" + rawStandard[4:],
	} {
		t.Run(name, func(t *testing.T) {
			decoded, err := DecodeString(encoded)
			if err != nil {
				t.Fatalf("DecodeString(%q): %v", encoded, err)
			}
			if string(decoded) != string(payload) {
				t.Fatalf("decoded=%v, want %v", decoded, payload)
			}
		})
	}

	if _, err := DecodeString("@@@@"); err == nil {
		t.Fatal("invalid Base64 must return an error")
	}
}
