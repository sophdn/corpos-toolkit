package stringutil

import "testing"

func TestContainsCaseInsensitive(t *testing.T) {
	cases := []struct {
		name, haystack, needle string
		want                   bool
	}{
		{"empty needle matches", "anything", "", true},
		{"empty haystack non-empty needle", "", "x", false},
		{"needle longer than haystack", "ab", "abc", false},
		{"exact case match", "Hello World", "Hello", true},
		{"case-insensitive prefix", "Hello World", "hello", true},
		{"case-insensitive middle", "the QUICK brown fox", "quick", true},
		{"case-insensitive suffix", "router generate: empty choices in response", "EMPTY CHOICES IN RESPONSE", true},
		{"absent", "the quick brown fox", "zebra", false},
		{"single char match", "abcdef", "C", true},
		{"single char absent", "abcdef", "Z", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ContainsCaseInsensitive(tc.haystack, tc.needle)
			if got != tc.want {
				t.Errorf("ContainsCaseInsensitive(%q, %q) = %v, want %v", tc.haystack, tc.needle, got, tc.want)
			}
		})
	}
}
