package events

import (
	"encoding/json"
	"strconv"
)

// jsonMarshalString is the single-place wrapper for json.Marshal of a
// string — used by MetricString to produce a valid JSON literal with
// proper escaping. Defined here rather than inline so MetricString stays
// a one-line constructor.
func jsonMarshalString(s string) ([]byte, error) {
	return json.Marshal(s)
}

// formatFloat renders a float64 the way encoding/json would: 'g' format
// with -1 precision. Matches what the JSON Schema validator sees when
// it inspects a numeric token, so MetricNumber(42).MarshalJSON() and
// json.Marshal(42.0) produce byte-identical output.
func formatFloat(n float64) string {
	return strconv.FormatFloat(n, 'g', -1, 64)
}
