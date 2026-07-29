package transform

import (
	"strings"
	"testing"
)

var benchmarkNormalizedBase64 string //nolint:gochecknoglobals

func BenchmarkNormalizeBase64OneMiB(b *testing.B) {
	standard := strings.Repeat("ABcd+/09", (1<<20)/8)
	urlSafe := strings.Repeat("ABcd-_09", (1<<20)/8)

	for name, input := range map[string]string{
		"standard": standard,
		"url_safe": urlSafe,
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			for range b.N {
				benchmarkNormalizedBase64 = NormalizeBase64(input)
			}
		})
	}
}
