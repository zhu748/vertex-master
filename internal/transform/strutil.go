package transform

import (
	"strings"
)

// SnakeToCamel 将 snake_case 转为 camelCase。
//
// 无下划线则原样返回（已是 camelCase 的键经此函数不变，这点对 generationConfig
// 的键转换很重要：temperature/topP/topK 等保持不动）。
func SnakeToCamel(s string) string {
	if !strings.Contains(s, "_") {
		return s
	}
	parts := strings.Split(s, "_")
	var b strings.Builder
	b.WriteString(parts[0])
	for _, p := range parts[1:] {
		b.WriteString(pyTitle(p))
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

// pyTitle 把单个词归一为首字母大写、其余小写。
// （Go 的 strings.Title 不会把其余字母转小写，故自实现。）
func pyTitle(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}
