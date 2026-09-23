package grok

import "testing"

// TestClassify covers the phrases grok 1.0.41 actually prints, plus the design-doc keywords.
func TestClassify(t *testing.T) {
	cases := []struct {
		msg, stderr, code string
	}{
		{"Couldn't set model 'no-such-model': Invalid params: \"unknown model id\". Run 'grok models' to see available models.", "", CodeModelNotFound},
		{"You've hit the rate limit for your plan.", "", CodeRateLimited},
		{"", "rate_limited", CodeRateLimited},
		{"You hit your free usage limit.", "", CodeUsageLimit},
		{"You hit your weekly limit.", "", CodeUsageLimit},
		{"usage_pool exhausted", "", CodeUsageLimit},
		{"Not signed in. Run `grok login` to use your SuperGrok subscription.", "", CodeNotLoggedIn},
		{"You are not authenticated.", "", CodeNotLoggedIn},
		{"something else broke", "", CodeFailed},
		{"Disk quota exceeded", "", CodeFailed},
	}
	for _, tc := range cases {
		ge := Classify(tc.msg, tc.stderr)
		if ge.Code != tc.code {
			t.Fatalf("%q/%q -> %s want %s (%s)", tc.msg, tc.stderr, ge.Code, tc.code, ge.Message)
		}
		if tc.code == CodeNotLoggedIn && !containsAny(ge.Message, "grok login") {
			t.Fatal(ge.Message)
		}
	}
}
