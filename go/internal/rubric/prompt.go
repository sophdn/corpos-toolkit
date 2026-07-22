package rubric

import (
	"fmt"
	"strings"
)

const classifySystemPrompt = "You classify input text against a fixed label set using the supplied " +
	"rubric. Output ONLY the chosen label(s) — no explanation, no commentary, " +
	"no preamble. If none of the labels fit (the input is genuinely off-rubric), " +
	"reply with the single line 'unclassifiable'. Honesty matters: a wrong " +
	"label is worse than 'unclassifiable'."

// ComposeClassify builds the (system, user) prompt pair for a classify call.
// It is a Go port of inference_clients::dispatcher::compose_classify.
func ComposeClassify(def RubricDef, inputText string) (system, user string) {
	var sb strings.Builder
	sb.WriteString("## Rubric\n")
	sb.WriteString(def.PromptTemplate)
	sb.WriteString("\n\n## Allowed labels\n")
	for _, label := range def.OutputEnum {
		sb.WriteString("- ")
		sb.WriteString(label)
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')

	if len(def.Examples) > 0 {
		sb.WriteString("## Worked examples\n\n")
		for i, ex := range def.Examples {
			fmt.Fprintf(&sb, "Example %d:\nInput: %s\nLabel: %s\nReasoning: %s\n\n",
				i+1, ex.Text, ex.Label, ex.Reasoning)
		}
	}

	sb.WriteString("## Input\n")
	sb.WriteString(inputText)
	sb.WriteString("\n\nReply with EXACTLY ONE label from the list above (or 'unclassifiable').")

	return classifySystemPrompt, sb.String()
}
