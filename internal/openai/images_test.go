package openai

import (
	"strings"
	"testing"
)

// TestMapAspect covers the size table, pass-through ratios, approximations, and rejects.
func TestMapAspect(t *testing.T) {
	cases := []struct {
		size, aspect, want string
		approx             bool
		bad                bool
	}{
		{"1024x1024", "", "1:1", false, false},
		{"512x512", "", "1:1", false, false},
		{"256x256", "", "1:1", false, false},
		{"1792x1024", "", "16:9", false, false},
		{"1024x1792", "", "9:16", false, false},
		{"1536x1024", "", "3:2", false, false},
		{"1024x1536", "", "2:3", false, false},
		{"auto", "", "auto", false, false},
		{"", "", "auto", false, false},
		{"", "16:9", "16:9", false, false},
		{"", "4:3", "3:2", true, false},
		{"", "3:4", "2:3", true, false},
		{"", "21:9", "16:9", true, false},
		{"", "9:19.5", "9:16", true, false},
		{"1024x1024", "1:1", "", false, true},
		{"nope", "", "", false, true},
		{"", "square", "", false, true},
	}
	for _, tc := range cases {
		got, approx, err := mapAspect(tc.size, tc.aspect)
		if tc.bad {
			if err == nil {
				t.Fatalf("%s %s", tc.size, tc.aspect)
			}
			continue
		}
		if err != nil || got != tc.want || approx != tc.approx {
			t.Fatalf("%s/%s got %s approx %v err %v", tc.size, tc.aspect, got, approx, err)
		}
	}
}

// TestParseImageGenRejectsN names n and refuses stream.
func TestParseImageGenRejectsN(t *testing.T) {
	_, err := ParseImageGen([]byte(`{"prompt":"x","n":5}`))
	re, ok := err.(*RequestError)
	if !ok || re.Code != "unsupported_parameter" || !strings.Contains(re.Message, "n") {
		t.Fatal(err)
	}
	_, err = ParseImageGen([]byte(`{"prompt":"x","stream":true}`))
	re, _ = err.(*RequestError)
	if re == nil || !strings.Contains(re.Message, "stream") {
		t.Fatal(err)
	}
	gen, err := ParseImageGen([]byte(`{"prompt":"cat","size":"1792x1024","model":"gpt-image-1"}`))
	if err != nil || gen.Aspect != "16:9" || gen.N != 1 || gen.Format != "url" || len(gen.Ignored) != 1 || gen.Ignored[0] != "model" {
		t.Fatalf("%+v %v", gen, err)
	}
}
