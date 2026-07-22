package refresolve

// detectBridgeShapes is the second-pass detector that promotes
// domain_term + external_technical primary references into bridge
// shapes — vault_candidate (PARSE_CONTEXT §3.2.3) and kiwix_bridge
// (§3.2.4). The bridges fire only when the primary shape detected
// AND the message provided a non-empty token, keeping latency
// proportional to actual ambiguity.
//
// Dedupes per (shape, token) so the same domain term doesn't emit
// two vault_candidate refs in one call.
func detectBridgeShapes(primary []Reference) []Reference {
	seen := map[ShapeCategory]map[string]bool{
		ShapeVaultCandidate: {},
		ShapeKiwixBridge:    {},
	}
	var out []Reference
	for _, ref := range primary {
		switch ref.Shape {
		case ShapeDomainTerm:
			if ref.Token == "" || seen[ShapeVaultCandidate][ref.Token] {
				continue
			}
			seen[ShapeVaultCandidate][ref.Token] = true
			out = append(out, Reference{
				Token:           ref.Token,
				Shape:           ShapeVaultCandidate,
				Confidence:      ref.Confidence,
				DetectionMethod: "bridge:domain_term",
				StartPos:        ref.StartPos,
				EndPos:          ref.EndPos,
			})
		case ShapeExternalTechnical:
			if ref.Token == "" || seen[ShapeKiwixBridge][ref.Token] {
				continue
			}
			seen[ShapeKiwixBridge][ref.Token] = true
			out = append(out, Reference{
				Token:           ref.Token,
				Shape:           ShapeKiwixBridge,
				Confidence:      ref.Confidence,
				DetectionMethod: "bridge:external_technical",
				StartPos:        ref.StartPos,
				EndPos:          ref.EndPos,
			})
		}
	}
	return out
}
