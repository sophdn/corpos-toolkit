package stringutil

// ContainsCaseInsensitive reports whether haystack contains needle,
// ignoring ASCII case. An empty needle matches anywhere; a needle
// longer than haystack returns false. Lifted verbatim from
// knowledge/handler.go where it classifies llamacpp empty-choices
// errors.
func ContainsCaseInsensitive(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			a := haystack[i+j]
			b := needle[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
