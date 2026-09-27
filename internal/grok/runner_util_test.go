package grok

import (
	"encoding/json"
	"testing"
)

// TestCheckToolset covers an empty chat allowlist and a media allowlist.
// extra is what grok offered beyond the allowlist. missing is what it left out.
func TestCheckToolset(t *testing.T) {
	cases := []struct {
		raw, allow     string
		extra, missing int
		wantErr        bool
	}{
		{`[]`, ``, 0, 0, false},
		{`["image_gen"]`, ``, 1, 0, false},
		{`null`, ``, 0, 0, true},
		{`["image_gen"]`, `image_gen`, 0, 0, false},
		{`["image_gen","run_terminal_command"]`, `image_gen`, 1, 0, false},
		{`[]`, `image_gen`, 0, 1, false},
		{`["image_edit"]`, `image_gen,image_edit`, 0, 1, false},
	}
	for _, tc := range cases {
		var allow []string
		if tc.allow != "" {
			allow = splitComma(tc.allow)
		}
		extra, missing, err := CheckToolset(json.RawMessage(tc.raw), allow)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%s: want error", tc.raw)
			}
			continue
		}
		if err != nil || len(extra) != tc.extra || len(missing) != tc.missing {
			t.Fatalf("%s allow %s extra %v missing %v err %v", tc.raw, tc.allow, extra, missing, err)
		}
	}
}

// splitComma splits a test allowlist. Empty stays nil when the caller skips it.
func splitComma(s string) []string {
	var out []string
	for _, p := range []byte(s) {
		_ = p
	}
	cur := ""
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(s[i])
	}
	return out
}
