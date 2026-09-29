package redact

import (
	"strings"
	"testing"
)

func TestLineOriginValuePrivacy(t *testing.T) {
	receipt := func() map[string]interface{} {
		return map[string]interface{}{"version": float64(1), "commitSha": strings.Repeat("a", 40), "workspaceKey": "o/r", "state": "measured", "replacedLines": float64(1), "tracedLines": float64(1), "skippedFiles": float64(0), "cappedFiles": float64(0), "replacedFrom": []interface{}{map[string]interface{}{"sha": strings.Repeat("b", 40), "lines": float64(1)}}}
	}
	if len(projectLineOriginValues(receipt())) == 0 {
		t.Fatal("dropped valid receipt")
	}
	for _, key := range []string{"commitSha", "workspaceKey", "state", "tracedLines"} {
		r := receipt()
		r[key] = "SOURCE_CANARY"
		if len(projectLineOriginValues(r)) != 0 {
			t.Fatalf("source survived %s", key)
		}
	}
	r := receipt()
	r["replacedFrom"] = []interface{}{map[string]interface{}{"sha": "SOURCE_CANARY", "lines": float64(1)}}
	if len(projectLineOriginValues(r)) != 0 {
		t.Fatal("source survived nested SHA")
	}
}
