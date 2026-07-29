package transform

import (
	"strings"
	"testing"
)

var benchmarkConvertedKey string //nolint:gochecknoglobals

func BenchmarkSnakeToCamel(b *testing.B) {
	for name, input := range map[string]string{
		"typical":       "max_output_tokens",
		"many_segments": strings.Repeat("field_value_", 8) + "tail",
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				benchmarkConvertedKey = SnakeToCamel(input)
			}
		})
	}
}
