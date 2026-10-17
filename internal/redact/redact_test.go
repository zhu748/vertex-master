package redact

import (
	"strings"
	"testing"
)

// 本包是面板脱敏与运行日志摘要的共用口径。面板「脱敏查看/导出」的完整行为矩阵
// 由 internal/api 的 mask 测试锁定；这里覆盖 redact 自身新增/导出的原语。

func TestMaskerMaskBody(t *testing.T) {
	m := NewMasker()
	body := `{"contents":[{"role":"user","parts":[{"text":"机密正文"}]}],` +
		`"recaptchaToken":"03AFcWeA-secret","generationConfig":{"temperature":0.7}}`
	got := m.MaskBody(body, "")
	if strings.Contains(got, "机密正文") {
		t.Fatalf("内容未脱敏: %s", got)
	}
	if strings.Contains(got, "03AFcWeA-secret") {
		t.Fatalf("凭据未脱敏: %s", got)
	}
	if !strings.Contains(got, CredentialMarker) {
		t.Fatalf("缺少凭据占位符: %s", got)
	}
	// 协议常量键与非内容字段保留。
	if !strings.Contains(got, `"role":"user"`) || !strings.Contains(got, `"temperature":0.7`) {
		t.Fatalf("协议字段不应被改动: %s", got)
	}
}

func TestMaskerNonJSONBody(t *testing.T) {
	if got := NewMasker().MaskBody("<html>user content</html>", ""); got != `"`+NonJSONMarker+`"` {
		t.Fatalf("非 JSON 正文应整体屏蔽，得到 %s", got)
	}
}

func TestMaskInline(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantAbsent  string
		wantPresent string
	}{
		{"URL 查询串", "GET https://x.example/y?key=AIzaSy-secret&alt=json failed", "AIzaSy-secret", CredentialMarker},
		{"引号键值", `upstream said {"api_key":"sk-live-1234"} rejected`, "sk-live-1234", CredentialMarker},
		{"裸键值", "Authorization: Bearer abc.def.ghi denied", "abc.def.ghi", CredentialMarker},
		{"token 子串", "refresh_token=rt-9999 invalid", "rt-9999", CredentialMarker},
	}
	for _, tc := range cases {
		got := MaskInline(tc.in)
		if strings.Contains(got, tc.wantAbsent) {
			t.Fatalf("%s: 残留敏感值: %s", tc.name, got)
		}
		if !strings.Contains(got, tc.wantPresent) {
			t.Fatalf("%s: 缺少占位符: %s", tc.name, got)
		}
	}
	// 非凭据文本保持原样（不得因脱敏破坏诊断信息）。
	if got := MaskInline("节点 hk-01 触发 429 限制"); got != "节点 hk-01 触发 429 限制" {
		t.Fatalf("非凭据文本被改动: %s", got)
	}
}

func TestExcerpt(t *testing.T) {
	// 多行错误文本：折叠为单行（日志按行读取）。
	got := Excerpt("第一行\n第二行\t带制表", 100)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("摘要应折叠空白: %q", got)
	}
	// 按 rune 截断 + 显式标记（中文不得截出半个字符）。
	long := strings.Repeat("敏感", 100)
	truncated := Excerpt(long, 10)
	if !strings.HasSuffix(truncated, ExcerptTruncatedSuffix) {
		t.Fatalf("超长应带截断标记: %q", truncated)
	}
	if n := len([]rune(strings.TrimSuffix(truncated, ExcerptTruncatedSuffix))); n != 10 {
		t.Fatalf("截断长度应为 10 rune，得到 %d: %q", n, truncated)
	}
	// JSON 正文走逐字段脱敏。
	js := `{"contents":[{"role":"user","parts":[{"text":"秘密内容"}]}],"apiKey":"k-1"}`
	got = Excerpt(js, 400)
	if strings.Contains(got, "秘密内容") || strings.Contains(got, "k-1") {
		t.Fatalf("JSON 摘要未脱敏: %s", got)
	}
	// 空串与未超限原样返回。
	if Excerpt("", 10) != "" {
		t.Fatal("空串应返回空串")
	}
	if Excerpt("短", 10) != "短" {
		t.Fatal("未超限应原样返回")
	}
}

func TestMaskHeadersAndURL(t *testing.T) {
	got := MaskHeaders(`{"Authorization":["Bearer t-1"],"Content-Type":["application/json"]}`)
	if strings.Contains(got, "t-1") {
		t.Fatalf("Authorization 未脱敏: %s", got)
	}
	if !strings.Contains(got, "application/json") {
		t.Fatalf("非凭据头应保留: %s", got)
	}
	if maskedURL := MaskURL("https://x/y?key=abc&z=1"); strings.Contains(maskedURL, "abc") || !strings.Contains(maskedURL, "z=1") {
		t.Fatalf("URL 脱敏应只换凭据值: %s", maskedURL)
	}
	if !ContainsMarker(MaskURL("https://x/y?key=abc")) {
		t.Fatal("ContainsMarker 应识别脱敏产物")
	}
	if ContainsMarker("普通日志行") {
		t.Fatal("ContainsMarker 不应误报")
	}
}

func TestIsCredentialName(t *testing.T) {
	for _, name := range []string{"API_KEY", "X-Goog-Api-Key", "Authorization", "Cookie", "recaptchaToken", "thoughtSignature", "client_secret"} {
		if !IsCredentialName(name) {
			t.Fatalf("%s 应判为凭据", name)
		}
	}
	for _, name := range []string{"Content-Type", "model", "finishReason", "candidates"} {
		if IsCredentialName(name) {
			t.Fatalf("%s 不应判为凭据", name)
		}
	}
}
