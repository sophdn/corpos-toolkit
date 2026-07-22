// Package stringutil hosts small string-shape helpers that handler
// packages otherwise duplicate.
//
// ## Intended use
//
// **Workflow served:** handlers that classify error strings or
// otherwise scan for ASCII-only sentinel substrings (e.g. the
// transient-inference classifier in knowledge) share one
// implementation rather than each inlining a fold-case scan.
//
// **Invocation pattern:** import `toolkit/internal/stringutil` and
// call `stringutil.ContainsCaseInsensitive(haystack, needle)`
// directly; the package is stateless.
//
// **Success shape:** ContainsCaseInsensitive reports whether haystack
// contains needle under ASCII case-folding. An empty needle matches
// anywhere; a needle longer than haystack returns false. Behaviour
// is byte-for-byte identical to the inline knowledge/handler.go
// scan it replaces.
//
// **Non-goals:** not UTF-8-aware (use strings.EqualFold or
// golang.org/x/text/cases for that); not a general string-search
// library; does not import any project package, so callers anywhere
// in the import graph can use it without cycle risk.
package stringutil
