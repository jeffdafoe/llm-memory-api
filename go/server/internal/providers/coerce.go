package providers

import (
	"math"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// asNumber coerces a configuration value to a finite number (Node's
// coerce.js). ok is false for null, undefined, "", and anything Number()
// does not make finite. Storage keeps what the operator typed (numeric
// columns arrive as strings); each provider sends numbers.
func asNumber(v any) (n float64, ok bool, err error) {
	if v == nil || v == undefined || isStr(v, "") {
		return 0, false, nil
	}
	f, err := jsutil.ValueNumber(v)
	if err != nil {
		return 0, false, err
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, nil
	}
	return f, true, nil
}

// singleNonNullType resolves a JSON Schema type to one coercible name: a
// string as is, or a union with exactly one non-"null" string member.
func singleNonNullType(t any) (string, bool) {
	if s, ok := t.(string); ok {
		return s, true
	}
	if a, ok := t.([]any); ok {
		var nonNull []any
		for _, e := range a {
			if !isStr(e, "null") {
				nonNull = append(nonNull, e)
			}
		}
		if len(nonNull) == 1 {
			if s, ok := nonNull[0].(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

func tryParseJSON(s string) (any, bool) {
	v, err := jsutil.Parse([]byte(s))
	if err != nil {
		return nil, false
	}
	return v, true
}

const maxSafeInteger = 1<<53 - 1

func isIntegerText(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	if i == len(s) {
		return false
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// CoerceToSchema converts string-encoded scalars in parsed tool arguments to
// the type the JSON Schema declares (Node's coerceToSchema). Models routed
// through OpenRouter often send every scalar as a string ({"qty":"1"}); a
// value is converted only when it cleanly matches the declared type, and an
// array or object argument sent whole as a JSON string is parsed first.
// Anything else is returned unchanged for downstream validation. Objects and
// arrays are changed in place.
func CoerceToSchema(value, schema any) any {
	s, ok := schema.(*object)
	if !ok {
		// Not an object (or an array, which has no "type").
		return value
	}
	typ, ok := singleNonNullType(s.Value("type"))
	if !ok {
		return value
	}
	switch typ {
	case "integer":
		str, ok := value.(string)
		if !ok {
			return value
		}
		trimmed := jsutil.Trim(str)
		if !isIntegerText(trimmed) {
			return value
		}
		n := jsutil.StringToNumber(trimmed)
		if n != math.Trunc(n) || math.Abs(n) > maxSafeInteger {
			return value
		}
		return n
	case "number":
		str, ok := value.(string)
		if !ok {
			return value
		}
		trimmed := jsutil.Trim(str)
		if trimmed == "" {
			return value
		}
		n := jsutil.StringToNumber(trimmed)
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return value
		}
		return n
	case "boolean":
		str, ok := value.(string)
		if !ok {
			return value
		}
		switch jsutil.ToLower(jsutil.Trim(str)) {
		case "true":
			return true
		case "false":
			return false
		}
		return value
	case "object":
		if str, ok := value.(string); ok {
			parsed, ok := tryParseJSON(str)
			if _, isObj := parsed.(*object); !ok || !isObj {
				return value
			}
			value = parsed
		}
		v, isObj := value.(*object)
		props, propsObj := s.Value("properties").(*object)
		if isObj && propsObj {
			for _, key := range v.Keys() {
				if key == "__proto__" || key == "constructor" || key == "prototype" {
					continue
				}
				if !props.Has(key) {
					continue
				}
				v.Set(key, CoerceToSchema(v.Value(key), props.Value(key)))
			}
		}
		return value
	case "array":
		if str, ok := value.(string); ok {
			parsed, ok := tryParseJSON(str)
			if _, isArr := parsed.([]any); !ok || !isArr {
				return value
			}
			value = parsed
		}
		if a, isArr := value.([]any); isArr && truthy(s.Value("items")) {
			items := s.Value("items")
			for i := range a {
				a[i] = CoerceToSchema(a[i], items)
			}
		}
		return value
	}
	return value
}

// CoerceToolArgs coerces a tool call's arguments against the tool's
// parameters schema, which may also arrive as a JSON string.
func CoerceToolArgs(input, parameters any) any {
	schema := parameters
	if str, ok := schema.(string); ok {
		parsed, ok := tryParseJSON(str)
		if !ok {
			return input
		}
		schema = parsed
	}
	if !typeofObject(schema) {
		return input
	}
	return CoerceToSchema(input, schema)
}
