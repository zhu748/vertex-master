package transform

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SnakeToCamel 将 snake_case 转为 camelCase。
//
// 无下划线则原样返回（已是 camelCase 的键经此函数不变，这点对 generationConfig
// 的键转换很重要：temperature/topP/topK 等保持不动）。
func SnakeToCamel(s string) string {
	firstUnderscore := strings.IndexByte(s, '_')
	if firstUnderscore < 0 {
		return s
	}
	asciiOnly := true
	for index := firstUnderscore + 1; index < len(s); index++ {
		if s[index] >= utf8.RuneSelf {
			asciiOnly = false
			break
		}
	}

	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:firstUnderscore])
	upperNext := true
	if asciiOnly {
		for index := firstUnderscore + 1; index < len(s); index++ {
			value := s[index]
			if value == '_' {
				upperNext = true
				continue
			}
			if upperNext {
				if value >= 'a' && value <= 'z' {
					value -= 'a' - 'A'
				}
				upperNext = false
			} else if value >= 'A' && value <= 'Z' {
				value += 'a' - 'A'
			}
			b.WriteByte(value)
		}
		return b.String()
	}

	for _, value := range s[firstUnderscore+1:] {
		if value == '_' {
			upperNext = true
			continue
		}
		if upperNext {
			value = unicode.ToUpper(value)
			upperNext = false
		} else {
			value = unicode.ToLower(value)
		}
		b.WriteRune(value)
	}
	return b.String()
}

// CamelToSnake 将 camelCase 转为 snake_case。
func CamelToSnake(s string) string {
	hasASCIIUpper := false
	for index := 0; index < len(s); index++ {
		if s[index] >= 'A' && s[index] <= 'Z' {
			hasASCIIUpper = true
			break
		}
	}
	if !hasASCIIUpper {
		return strings.ToLower(s)
	}

	var output strings.Builder
	output.Grow(len(s) + 4)
	nonASCII := false
	var previous byte
	for index := 0; index < len(s); index++ {
		original := s[index]
		value := original
		if original >= 'A' && original <= 'Z' {
			if index > 0 && ((previous >= 'a' && previous <= 'z') ||
				(previous >= '0' && previous <= '9')) {
				output.WriteByte('_')
			}
			value += 'a' - 'A'
		} else if original >= 0x80 {
			nonASCII = true
		}
		output.WriteByte(value)
		previous = original
	}
	converted := output.String()
	if nonASCII {
		// 与旧实现一致：边界只识别 ASCII，最终大小写转换仍覆盖 Unicode。
		return strings.ToLower(converted)
	}
	return converted
}
