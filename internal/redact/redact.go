// Package redact masks credentials and message content in bounded log excerpts.
// Adapted from the supplied vertex-singbox reference; supports Gemini, OpenAI
// and Anthropic payloads, including incomplete JSON and proxy URL credentials.
package redact

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 占位符常量。导出是因为运行日志摘要与面板脱敏视图需要同一组标记
// （例如导入时识别"该文件正文不可用于排障"）。
const (
	// CredentialMarker 是凭据值的统一占位符。
	CredentialMarker = "[凭据已脱敏]"
	// TruncatedMarker 用于"无法判定身份"的截断残片（连键名都没读完的尾部）。
	TruncatedMarker = "[截断片段已脱敏]"
	// NonJSONMarker 用于整体屏蔽的非 JSON 正文（宁可整段不给人看）。
	NonJSONMarker = "[非 JSON 正文已整体脱敏]"
	// ExcerptTruncatedSuffix 是摘要被限长截断时的显式标记。
	ExcerptTruncatedSuffix = "…[截断]"
	// unknownRole 是取不到 role 时的标注。
	unknownRole = "未知"
)

// contentContainers 是"其内字符串值一律视为内容"的键名（值为对象或数组时才生效）。
//
//nolint:gochecknoglobals // Constant-like key table
var contentContainers = map[string]bool{
	"contents": true, "systeminstruction": true, "candidates": true, "content": true,
	"parts": true, "args": true, "arguments": true, "functioncall": true,
	"functionresponse": true, "inlinedata": true,
	"messages": true, "input": true, "output": true, "system": true,
	"tool_calls": true, "tools": true, "filedata": true,
}

// strictContainers 是"其内没有任何例外"的容器键名：客户端工具参数名不可预知，
// 按协议常量白名单放行会漏掉真实内容（评审 D2）。
//
//nolint:gochecknoglobals // Constant-like key table
var strictContainers = map[string]bool{"args": true, "arguments": true}

// protocolConstantKeys 是容器内**保留**的协议常量键（非用户内容，保留后仍能读懂
// "谁说的 / 哪个工具 / 是不是图 / 为何结束"）。
//
//nolint:gochecknoglobals // Constant-like key table
var protocolConstantKeys = map[string]bool{
	"role": true, "name": true, "finishreason": true, "mimetype": true,
}

// credentialNames 是凭据类键名的显式子集；此外还按子串匹配（见 IsCredentialName），
// 因为上游字段名不可枚举（x-goog-api-key、recaptchaToken、access_token 等）。
//
//nolint:gochecknoglobals // Constant-like key table
var credentialNames = map[string]bool{
	"key": true, "apikey": true, "auth": true, "authorization": true, "cookie": true,
	"password": true, "passwd": true, "pwd": true, "sig": true, "signature": true,
	"thoughtsignature": true,
}

var urlUserInfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#@]+@`) //nolint:gochecknoglobals
var urlQueryParam = regexp.MustCompile(`([?&])([^=&#\s]+)=([^&#\s]*)`)          //nolint:gochecknoglobals

// maskedMarkerPattern 识别脱敏产物的占位符（导入脱敏导出文件时提示用）。
//
//nolint:gochecknoglobals // Compiled-once regexp
var maskedMarkerPattern = regexp.MustCompile(`\[凭据已脱敏\]|消息第\d+次脱敏|\[截断片段已脱敏\]|\[非 JSON 正文已整体脱敏\]`)

// inlineQuotedCred 匹配"引号键:引号值"形式的凭据赋值（错误文本里内嵌的 JSON 片段）。
//
//nolint:gochecknoglobals // Compiled-once regexp
var inlineQuotedCred = regexp.MustCompile(`"([A-Za-z0-9_\-.]+)"\s*:\s*"([^"]*)"`)

// inlineBareCred 匹配 key=value / key: value 形式的**凭据**赋值（头部、查询串、日志片段）。
//
// 关键：模式**由凭据名驱动**（而不是"先匹配任意键名再判断"）——否则前一个非凭据键
// （如 "rejected: key=SECRET"）会把它后面的凭据赋值一并吃掉，导致漏遮。
// 名称部分与 IsCredentialName 的口径一致：显式子集 + 子串（token/secret/password/…）+ "key" 结尾。
//
//nolint:gochecknoglobals // Compiled-once regexp
var inlineBareCred = regexp.MustCompile(`(?i)\b([A-Za-z0-9_\-]*(?:key|token|secret|password|passwd|pwd|auth|authorization|cookie|signature|sig))\b\s*[:=]\s*([^\s,;&"']+)`)

// authSchemeToken 匹配 "Bearer/Basic/Digest/OAuth <token>" 形式的凭据值——
// 这类值的真正秘密是方案名之后的那个词，只靠键值规则会把方案名当值而漏掉 token。
//
//nolint:gochecknoglobals // Compiled-once regexp
var authSchemeToken = regexp.MustCompile(`(?i)\b(bearer|basic|digest|oauth)\s+[^\s,;"']+`)

// isAuthScheme 判断词是否为认证方案名（其后的 token 由 authSchemeToken 单独处理，
// 裸键值规则不再重复替换，避免留下多余占位符）。
func isAuthScheme(word string) bool {
	switch strings.ToLower(word) {
	case "bearer", "basic", "digest", "oauth":
		return true
	default:
		return false
	}
}

// Masker 是一次记录脱敏的跨 body 状态：role 由左到右遇到的 role 字段维护，
// count 是逻辑消息序号，lastRole 与 started 一起实现"角色变化才 +1、每个 body 首段另起新号"。
// 请求体与响应体共用同一个 Masker，才能共享同一套逻辑消息编号。
type Masker struct {
	role     string
	lastRole string
	count    int
	started  bool
}

// NewMasker 创建一个跨 body 保持编号连续的脱敏器。
func NewMasker() *Masker { return &Masker{} }

// contentMarker 返回下一段内容占位符（按逻辑消息推进序号）。
func (st *Masker) contentMarker() string {
	role := st.role
	if role == "" {
		role = unknownRole
	}
	if !st.started || role != st.lastRole {
		st.count++
		st.lastRole = role
		st.started = true
	}
	return "[" + role + "]消息第" + strconv.Itoa(st.count) + "次脱敏"
}

// containerFrame 记录一个已进入的内容容器：depth 是容器自身的嵌套深度，
// strict 表示该容器内没有任何协议常量例外（args / arguments 子树）。
type containerFrame struct {
	depth  int
	strict bool
}

// MaskBody 对一段正文脱敏。initialRole 是该 body 的起始角色（响应体传 "model"：
// 上游返回的内容恒由模型产出，而流式首帧可能不带 role 字段）；请求体传空串。
func (st *Masker) MaskBody(body, initialRole string) string {
	if body == "" {
		return body
	}
	if !looksLikeJSON(body) {
		// 无法判定结构 → 整段不给人看（宁可丢信息，不给片段）。
		return `"` + NonJSONMarker + `"`
	}
	st.role = initialRole
	st.started = false // 每个 body 的首段内容另起新号

	var b strings.Builder
	b.Grow(len(body))

	depth := 0                      // 对象/数组嵌套深度
	var containers []containerFrame // 已进入的内容容器栈（非空 = 容器内字符串一律脱敏）
	i := 0
	for i < len(body) {
		switch body[i] {
		case '{', '[':
			depth++
			b.WriteByte(body[i])
			i++
		case '}', ']':
			if depth > 0 {
				depth--
			}
			for len(containers) > 0 && containers[len(containers)-1].depth > depth {
				containers = containers[:len(containers)-1]
			}
			b.WriteByte(body[i])
			i++
		case ':':
			b.WriteByte(body[i])
			i++
		case '"':
			end := stringEnd(body, i)
			if end < 0 {
				// 未闭合字面量（截断残片）：身份不可判定，一律不复制原文。
				b.WriteString(`"` + TruncatedMarker + `"`)
				return b.String()
			}
			key, valueStart, isKV := keyOf(body, i, end)
			if !isKV {
				// Standalone array values may be opaque tokens (e.g. reCAPTCHA's
				// ["rresp", token]). Without a field name, hide them conservatively.
				b.WriteString(`"` + st.contentMarker() + `"`)
				i = end + 1
				continue
			}
			// 键名已确定：先做容器登记与角色切换，再决定这个值怎么处理。
			// 必须在这里做——键名与它的冒号在同一次迭代里被消费（见下方 i 的推进），
			// 单独写在 ':' 分支里永远看不到这些键。
			lower := strings.ToLower(key)
			if lower == "systeminstruction" && !strictInside(containers) {
				st.role = "system" // 系统指令没有 role 字段：进它之前先把角色切到 system
			}
			if contentContainers[lower] && valueStart < len(body) && (body[valueStart] == '{' || body[valueStart] == '[') {
				// 容器自身的深度 = 当前深度 + 1（它的开括号紧跟其后）。
				containers = append(containers, containerFrame{depth: depth + 1, strict: strictContainers[lower]})
			}
			b.WriteString(body[i:valueStart]) // 键名与冒号：原样
			if valueStart >= len(body) || body[valueStart] != '"' {
				// 非字符串值（对象/数组/数字/布尔/null）：交给后续字节继续处理。
				i = valueStart
				continue
			}
			valueEnd := stringEnd(body, valueStart)
			if valueEnd < 0 {
				// 值未闭合（正文被截断）：按键身份补占位符，绝不保留半截原文。
				switch {
				case IsCredentialName(key):
					b.WriteString(`"` + CredentialMarker + `"`)
				case isContentKey(key) || len(containers) > 0:
					b.WriteString(`"` + st.contentMarker() + `"`)
				default:
					b.WriteString(`"` + TruncatedMarker + `"`)
				}
				return b.String()
			}
			switch {
			case !strictInside(containers) && strings.EqualFold(key, "role"):
				// role 值本身是协议常量（且用于后续标注），原样保留。
				st.role = safeRole(body[valueStart+1 : valueEnd])
				b.WriteString(body[valueStart : valueEnd+1])
			case !strictInside(containers) && protocolConstantKeys[lower]:
				b.WriteString(body[valueStart : valueEnd+1])
			case IsCredentialName(key):
				b.WriteString(`"` + CredentialMarker + `"`)
			case isContentKey(key) || len(containers) > 0:
				b.WriteString(`"` + st.contentMarker() + `"`)
			default:
				b.WriteString(body[valueStart : valueEnd+1])
			}
			i = valueEnd + 1
		default:
			b.WriteByte(body[i])
			i++
		}
	}
	return b.String()
}

// strictInside 判断当前是否处于"无例外"容器（args / arguments 或其子树）内。
func strictInside(containers []containerFrame) bool {
	for _, c := range containers {
		if c.strict {
			return true
		}
	}
	return false
}

// looksLikeJSON 判断正文是否以 JSON 对象/数组开头（允许前导空白）。
// 非 JSON 正文（HTML/JS/纯文本上游错误页等）无法逐字段判定，整体屏蔽。
func looksLikeJSON(body string) bool {
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// nextNonSpace 返回 i 之后第一个非空白字节的下标。
func nextNonSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

// stringEnd 返回 s[start]（'"'）之后的下一个未转义引号下标；未闭合返回 -1。
func stringEnd(s string, start int) int {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // 跳过被转义的下一个字节
		case '"':
			return i
		}
	}
	return -1
}

// keyOf 判断 body[quote..end] 是否为"键名"（其后仅空白 + ':'），是则返回键名与值的起点。
func keyOf(body string, quote, end int) (key string, valueStart int, ok bool) {
	j := end + 1
	for j < len(body) && (body[j] == ' ' || body[j] == '\t' || body[j] == '\n' || body[j] == '\r') {
		j++
	}
	if j >= len(body) || body[j] != ':' {
		return "", 0, false
	}
	j++
	var name string
	if err := json.Unmarshal([]byte(body[quote:end+1]), &name); err != nil {
		return "", 0, false
	}
	return name, nextNonSpace(body, j), true
}

// isContentKey 判断键名是否直接承载消息正文（值必须是字符串才替换）。
func isContentKey(key string) bool {
	switch strings.ToLower(key) {
	case "text", "content", "data", "input", "output", "instructions", "system", "arguments", "prompt", "fileuri", "image_url", "url":
		return true
	default:
		return false
	}
}

// IsCredentialName 判断键/头/查询参数名是否为凭据：显式清单 + 子串（token/secret/password/
// api-key 等）+ "key" 结尾，宁可多遮不可漏遮。
func IsCredentialName(name string) bool {
	n := strings.ToLower(name)
	if credentialNames[n] {
		return true
	}
	for _, s := range []string{"token", "secret", "password", "apikey", "api_key", "api-key", "credential"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return strings.HasSuffix(n, "key")
}

// MaskHeaders 脱敏 headers JSON（marshalHeaders 的产物：map[string][]string）。
// 解析失败时**整体隐藏**而不是原样保留——那正是最可能泄露凭据的路径。
func MaskHeaders(headers string) string {
	if strings.TrimSpace(headers) == "" {
		return headers
	}
	var m map[string][]string
	if err := json.Unmarshal([]byte(headers), &m); err != nil {
		return `{"_masked":"[头信息解析失败，为避免泄露已整体隐藏]"}`
	}
	out := make(map[string][]string, len(m))
	for name, vals := range m {
		if IsCredentialName(name) {
			out[name] = []string{CredentialMarker}
			continue
		}
		out[name] = vals
	}
	data, err := json.Marshal(out)
	if err != nil {
		return `{"_masked":"[头信息序列化失败，为避免泄露已整体隐藏]"}`
	}
	return string(data)
}

// MaskURL 脱敏最终 URL 里的凭据查询参数（如 ?key=...），其余部分逐字节保留。
func MaskURL(url string) string {
	if url == "" {
		return url
	}
	return maskURLCredentials(url)
}

func maskURLCredentials(value string) string {
	value = urlUserInfo.ReplaceAllString(value, "${1}"+CredentialMarker+"@")
	return urlQueryParam.ReplaceAllStringFunc(value, func(match string) string {
		parts := urlQueryParam.FindStringSubmatch(match)
		key, err := url.QueryUnescape(parts[2])
		if err != nil || IsCredentialName(key) {
			return parts[1] + parts[2] + "=" + CredentialMarker
		}
		return match
	})
}

func safeRole(role string) string {
	switch role {
	case "user", "model", "assistant", "system", "tool", "function":
		return role
	default:
		return unknownRole
	}
}

// ContainsMarker 判断文本是否带脱敏占位符（导入脱敏导出文件时提示用）。
func ContainsMarker(s string) bool { return maskedMarkerPattern.MatchString(s) }

// MaskInline 对**非 JSON 的任意文本**做尽力而为的凭据脱敏：URL 查询串 +
// "键": "值" 与 键=值/键: 值 形式。用于运行日志的错误摘要——这类文本可能内嵌
// 上游返回的片段，无法按 JSON 结构逐字段判定，只能按键名模式保守替换。
func MaskInline(s string) string {
	if s == "" {
		return s
	}
	s = maskURLCredentials(s)
	// 方案名 + token 先于键值规则处理：否则键值规则会把方案名当"值"替换掉，
	// 而真正的 token 被留在线性扫描的下一段里。
	s = authSchemeToken.ReplaceAllString(s, "${1} "+CredentialMarker)
	s = inlineQuotedCred.ReplaceAllStringFunc(s, func(m string) string {
		sub := inlineQuotedCred.FindStringSubmatch(m)
		if len(sub) == 3 && IsCredentialName(sub[1]) {
			return `"` + sub[1] + `":"` + CredentialMarker + `"`
		}
		return m
	})
	s = inlineBareCred.ReplaceAllStringFunc(s, func(m string) string {
		sub := inlineBareCred.FindStringSubmatch(m)
		if len(sub) == 3 && IsCredentialName(sub[1]) && !isAuthScheme(sub[2]) {
			sep := ":"
			if i := strings.IndexAny(m, ":="); i >= 0 {
				sep = string(m[i])
			}
			return sub[1] + sep + CredentialMarker
		}
		return m
	})
	return s
}

// maxInlineBodyRunes 是"非 JSON 文本仍按错误消息保留"的长度上限：
// 超过它就无法再当作一句错误消息看待，按正文整体屏蔽（见 looksLikeBody）。
const maxInlineBodyRunes = 512

// looksLikeBody 判断**非 JSON** 文本是否属于"正文/payload"而非错误消息：
//   - 含 HTML/脚本标记（上游错误页、网关拦截页等）；
//   - 或长度超过 maxInlineBodyRunes（无法再当作一句错误消息）。
//
// 这类文本无法逐字段脱敏，按 http_log 的同一口径**整体屏蔽**；
// 而简短的上游错误消息（"connection reset by peer"、"429 rate limited" 等）
// 是排障的关键信息，保留（仅做凭据脱敏 + 限长）。
func looksLikeBody(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range []string{"<html", "<!doctype", "<body", "<script", "</"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return utf8.RuneCountInString(s) > maxInlineBodyRunes
}

// Excerpt 把任意文本压成**可安全写入运行日志**的摘要：
//   - 空白折叠为单空格（日志按行读取，正文换行会破坏行结构）；
//   - JSON 正文走 Masker 逐字段脱敏，非 JSON 文本走 MaskInline 键名脱敏；
//   - 按 rune 截断到 maxRunes，并追加显式截断标记（不得静默丢尾）。
//
// 这是运行日志唯一允许输出"上游/请求内容"的入口（Issue #36 评审批次 §1）：
// 任何级别（含 DEBUG）都不得用完整正文替代本函数。
func Excerpt(s string, maxRunes int) string {
	if s == "" {
		return s
	}
	s = collapseSpaces(s)
	switch {
	case looksLikeJSON(s):
		s = NewMasker().MaskBody(s, "")
	case looksLikeBody(s):
		// 非 JSON 正文（HTML/超长文本）：无法逐字段判定，整体屏蔽但保留长度信息，
		// 让人知道"这里原本有内容、被红线挡掉了"（与 http_log 的非 JSON 口径一致）。
		s = NonJSONMarker + "（原长度 " + strconv.Itoa(utf8.RuneCountInString(s)) + " 字符）"
	default:
		// Error strings often prefix a JSON body with "Upstream Error: ".
		// Preserve the diagnostic prefix but mask the embedded payload as well.
		if start := strings.IndexByte(s, '{'); start >= 0 {
			s = s[:start] + NewMasker().MaskBody(s[start:], "")
		}
	}
	s = MaskInline(s)
	return truncateRunes(s, maxRunes)
}

// collapseSpaces 把换行/制表/连续空白压成单空格并修剪首尾。
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// truncateRunes 按 rune 截断并追加标记；未超限时原样返回。
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i] + ExcerptTruncatedSuffix
		}
		n++
	}
	return s
}
