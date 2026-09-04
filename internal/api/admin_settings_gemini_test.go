package api

import (
	"strings"
	"testing"
)

func TestMaskAPIKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "", want: ""},
		{in: "short", want: "*****"},
		{in: "123456789", want: "*********"},
		{in: "AQ.ExampleKeyForUnitTestsOnly12345", want: "AQ.Ex****2345"},
		{in: "  AQ.ExampleKeyForUnitTestsOnly12345  ", want: "AQ.Ex****2345"},
	}
	for _, tc := range cases {
		if got := maskAPIKey(tc.in); got != tc.want {
			t.Fatalf("maskAPIKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// 脱敏值绝不能包含完整密钥。
	full := "AQ.ExampleKeyForUnitTestsOnly12345"
	if masked := maskAPIKey(full); masked == full {
		t.Fatal("masked value must differ from the raw key")
	}
	// 测试夹具禁止使用真实密钥：任何以真实密钥前缀开头的完整凭据都不应出现。
	if masked := maskAPIKey(full); !strings.Contains(masked, "*") {
		t.Fatal("masked value must contain asterisks")
	}
}
