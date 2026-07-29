package api

import "testing"

type testRequestedModelResolver struct {
	aliases map[string]string
}

func (r testRequestedModelResolver) FakePrefixes() []string {
	return []string{"假流式-", "fake-"}
}

func (r testRequestedModelResolver) ResolveModelName(model string) string {
	if resolved, ok := r.aliases[model]; ok {
		return resolved
	}
	return model
}

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

func TestResolveRequestedModelRecognizesFakeAliases(t *testing.T) {
	resolver := testRequestedModelResolver{aliases: map[string]string{
		"flash":       "gemini-3.6-flash",
		"fakeFlash":   "fake-gemini-3.6-flash",
		"cnFakeFlash": "假流式-gemini-3.6-flash",
	}}
	tests := []struct {
		name      string
		input     string
		wantModel string
		wantFake  bool
	}{
		{name: "plain alias", input: "flash", wantModel: "flash"},
		{name: "fake prefix around alias", input: "fake-flash", wantModel: "flash", wantFake: true},
		{name: "fake alias target", input: "fakeFlash", wantModel: "gemini-3.6-flash", wantFake: true},
		{name: "chinese fake alias target", input: "cnFakeFlash", wantModel: "gemini-3.6-flash", wantFake: true},
		{name: "direct model", input: "gemini-3.6-flash", wantModel: "gemini-3.6-flash"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotFake := resolveRequestedModel(tc.input, resolver)
			if gotModel != tc.wantModel || gotFake != tc.wantFake {
				t.Fatalf(
					"resolveRequestedModel(%q)=(%q,%v), want (%q,%v)",
					tc.input, gotModel, gotFake, tc.wantModel, tc.wantFake,
				)
			}
		})
	}
}
