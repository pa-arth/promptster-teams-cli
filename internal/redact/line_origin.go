package redact

import (
	"math"
	"regexp"
)

var lineOriginSHA = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var lineOriginWorkspace = regexp.MustCompile(`^(?:[A-Za-z0-9_.+-]+/[A-Za-z0-9_.+-]+|[a-f0-9]{16})$`)

// Validate values as well as keys: source cannot hide inside an allowed SHA or
// count field. This is the on-device mirror of CommitLineOriginSchema.
func projectLineOriginValues(data map[string]interface{}) map[string]interface{} {
	empty := func() map[string]interface{} { return map[string]interface{}{} }
	str := func(key string) string { s, _ := data[key].(string); return s }
	count := func(value interface{}) (int, bool) {
		switch n := value.(type) {
		case int:
			return n, n >= 0 && float64(n) <= 9007199254740991
		case float64:
			if n >= 0 && n <= 9007199254740991 && n == math.Trunc(n) {
				return int(n), true
			}
		}
		return 0, false
	}
	v, ok := count(data["version"])
	if !ok || v != 1 || !lineOriginSHA.MatchString(str("commitSha")) || !lineOriginWorkspace.MatchString(str("workspaceKey")) || len(str("workspaceKey")) > 300 {
		return empty()
	}
	state := str("state")
	if state != "measured" && state != "partial" && state != "unavailable" {
		return empty()
	}
	numbers := map[string]int{}
	for _, k := range []string{"replacedLines", "tracedLines", "skippedFiles", "cappedFiles"} {
		n, ok := count(data[k])
		if !ok {
			return empty()
		}
		numbers[k] = n
	}
	origins, ok := data["replacedFrom"].([]interface{})
	if !ok || len(origins) > 1000 {
		return empty()
	}
	total := 0
	seen := map[string]bool{}
	for _, o := range origins {
		m, ok := o.(map[string]interface{})
		if !ok {
			return empty()
		}
		sha, _ := m["sha"].(string)
		n, valid := count(m["lines"])
		if !valid || n < 1 || !lineOriginSHA.MatchString(sha) || seen[sha] {
			return empty()
		}
		seen[sha] = true
		total += n
		if total > numbers["replacedLines"] {
			return empty()
		}
	}
	if total != numbers["tracedLines"] || numbers["tracedLines"] > numbers["replacedLines"] {
		return empty()
	}
	if state == "measured" && (numbers["skippedFiles"] != 0 || numbers["cappedFiles"] != 0 || numbers["tracedLines"] != numbers["replacedLines"]) {
		return empty()
	}
	return data
}
