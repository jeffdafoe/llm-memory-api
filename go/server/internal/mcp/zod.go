package mcp

import (
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// zod error texts. The SDK validates requests with zod and puts a failure's
// message, JSON.stringify(issues, null, 2), into its JSON-RPC errors; these
// build the same text for the issues the ported checks find.

// issue is one zod "invalid_type" issue.
type issue struct {
	expected string
	path     []string
	received string
}

// receivedName is the type name zod writes after "received".
func receivedName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case *jsutil.Object:
		return "object"
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	if v == jsutil.Undefined {
		return "undefined"
	}
	return "number"
}

func (i issue) message() string {
	return "Invalid input: expected " + i.expected + ", received " + i.received
}

// zodText is the message of a zod error with these issues.
func zodText(issues []issue) string {
	var b strings.Builder
	b.WriteString("[\n")
	for n, is := range issues {
		b.WriteString("  {\n")
		b.WriteString(`    "expected": ` + jsutil.Marshal(is.expected) + ",\n")
		b.WriteString(`    "code": "invalid_type",` + "\n")
		b.WriteString(`    "path": [`)
		if len(is.path) == 0 {
			b.WriteString("],\n")
		} else {
			b.WriteString("\n")
			for k, p := range is.path {
				b.WriteString("      " + jsutil.Marshal(p))
				if k < len(is.path)-1 {
					b.WriteString(",")
				}
				b.WriteString("\n")
			}
			b.WriteString("    ],\n")
		}
		b.WriteString(`    "message": ` + jsutil.Marshal(is.message()) + "\n")
		b.WriteString("  }")
		if n < len(issues)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]")
	return b.String()
}

// checkType appends an issue when v is not of the zod type: "string",
// "object" (a non-array object), "record" (the same), "array", "boolean".
// Undefined fails unless optional.
func checkType(issues []issue, v any, expected string, optional bool, path ...string) ([]issue, bool) {
	if v == jsutil.Undefined && optional {
		return issues, true
	}
	ok := false
	switch expected {
	case "string":
		_, ok = v.(string)
	case "object", "record":
		_, ok = v.(*jsutil.Object)
	case "array":
		_, ok = v.([]any)
	case "boolean":
		_, ok = v.(bool)
	}
	if !ok {
		issues = append(issues, issue{expected: expected, path: path, received: receivedName(v)})
	}
	return issues, ok
}
