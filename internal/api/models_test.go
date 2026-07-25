package api

import "testing"

// ---- stripFakePrefix：剥离 "假流式-" / "fake-" 前缀 ----

func TestStripFakePrefix(t *testing.T) {
	fakePrefixes := []string{"假流式-", "fake-"}
	cases := []struct {
		name      string
		in        string
		wantModel string
		wantFake  bool
	}{
		{"chinese prefix", "假流式-gemini-2.5-flash", "gemini-2.5-flash", true},
		{"ascii prefix", "fake-gemini-2.5-pro", "gemini-2.5-pro", true},
		{"ascii prefix short", "fake-x", "x", true},
		{"no prefix passthrough", "gemini-2.5-flash", "gemini-2.5-flash", false},
		{"empty passthrough", "", "", false},
		{"prefix-like but not match", "fakegemini", "fakegemini", false},
		{"chinese prefix only", "假流式-", "", true},
		{"prefix inside name not stripped", "gemini-fake-thing", "gemini-fake-thing", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotModel, gotFake := stripFakePrefix(c.in, fakePrefixes)
			if gotModel != c.wantModel || gotFake != c.wantFake {
				t.Errorf("stripFakePrefix(%q)=(%q,%v)，期望 (%q,%v)",
					c.in, gotModel, gotFake, c.wantModel, c.wantFake)
			}
		})
	}
}
