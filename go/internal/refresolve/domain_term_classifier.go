package refresolve

import (
	"context"
	"fmt"
	"time"

	"toolkit/internal/inference/router"
	"toolkit/internal/qwenctx"
	"toolkit/internal/rubric"
)

// DomainTermRubricClassifier wraps the rubric registry + inference
// router to implement DomainTermClassifier via the
// reference-domain-term-detector rubric defined at
// blueprints/rubrics/reference-domain-term-detector.toml.
//
// Construction wires the rubric registry; per-call IsDomainTerm
// composes the rubric prompt, runs inference via the router, and
// parses the response.
//
// Failure modes:
//   - Rubric not loaded / not deployed: returns (false, 0, error).
//     The detector's permissive fallback skips the domain-term step
//     and continues with the rule-based shapes.
//   - Inference timeout / network error: same — returns error and
//     the detector skips.
//   - Parse failure (NoMatch / Multiple): returns (false, 0, error).
//
// T7 replaces this implementation with a trained sklearn classifier
// behind the same DomainTermClassifier interface; the hot-swap
// works because the detector reads the active classifier at call
// time via NewDetector's classifier arg.
type DomainTermRubricClassifier struct {
	registry *rubric.Registry
	router   *router.Router
}

// NewDomainTermRubricClassifier returns a classifier ready to plug
// into NewDetector. The registry must contain the
// reference-domain-term-detector rubric; the router must be live.
//
// Returns an error if the rubric is missing or undeployed so the
// caller (server startup) can decide whether to proceed without
// domain-term detection.
func NewDomainTermRubricClassifier(registry *rubric.Registry, r *router.Router) (*DomainTermRubricClassifier, error) {
	if registry == nil {
		return nil, fmt.Errorf("rubric registry required")
	}
	if r == nil {
		return nil, fmt.Errorf("inference router required")
	}
	def, ok := registry.Get(rubricName)
	if !ok {
		return nil, fmt.Errorf("rubric %q not found in registry", rubricName)
	}
	if !def.IsDeployed {
		return nil, fmt.Errorf("rubric %q is not deployed", rubricName)
	}
	return &DomainTermRubricClassifier{registry: registry, router: r}, nil
}

const rubricName = "reference-domain-term-detector"

// IsDomainTerm returns (true, conf, nil) when the phrase classifies
// as a domain term with confidence ≥ 0.6 (the detector threshold).
// Returns (false, conf, nil) for genuine not-domain-term labels;
// (false, 0, err) for inference / parse failures.
//
// Confidence model (single-class rubric, no native confidence):
//   - "domain-term" → 0.8
//   - "unclear"     → 0.5
//   - "not-domain-term" → 0.0
//
// The detector's downstream threshold is 0.6, so "domain-term"
// passes and "unclear" doesn't — matching the rubric's "bias toward
// not-domain-term when generic" guidance.
func (c *DomainTermRubricClassifier) IsDomainTerm(ctx context.Context, phrase string) (bool, float64, error) {
	def, ok := c.registry.Get(rubricName)
	if !ok {
		return false, 0, fmt.Errorf("rubric %q not found in registry", rubricName)
	}
	system, user := rubric.ComposeClassify(def, phrase)

	// Stamp the task_id so the universal inference_invocations row
	// attributes this call to the detector rubric (bug 1328's
	// universal-per-call telemetry shape).
	ctx = qwenctx.WithTaskID(ctx, rubricName)
	// Bound per-call latency. The design doc names ~500ms as the
	// target (§2.4); the live measurement at T6 verification time
	// observed 251ms for "Tripolar Invariant" against the actual
	// Qwen at localhost:8081. A 3s cap gives a ~12× safety margin
	// over the observed median and still well under the 2.5s total
	// handler budget when no other resolvers are slow. Tune in
	// dispatch-policy.toml if needed; tighter caps cause spurious
	// fallback-to-rule-based on slow-Qwen days.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	genResult, err := c.router.Generate(ctx, user, system)
	if err != nil {
		return false, 0, fmt.Errorf("router.Generate: %w", err)
	}
	parsed := rubric.ParseSingleClass(genResult.Text, def.OutputEnum)
	switch parsed.Kind {
	case rubric.ParsedSingle:
		switch parsed.Label {
		case "domain-term":
			return true, 0.8, nil
		case "unclear":
			return false, 0.5, nil
		case "not-domain-term":
			return false, 0.0, nil
		default:
			return false, 0, fmt.Errorf("unexpected label %q", parsed.Label)
		}
	case rubric.ParsedUnclassifiable:
		return false, 0, nil
	case rubric.ParsedMultiple:
		return false, 0, fmt.Errorf("rubric returned multiple labels: %v", parsed.Labels)
	default:
		return false, 0, fmt.Errorf("rubric response did not match enum: %s", genResult.Text)
	}
}
