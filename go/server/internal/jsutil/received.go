package jsutil

import "encoding/json"

// Received is the end of Node's ERR_INVALID_ARG_TYPE message ("Received
// ...") for a request value that reached a Node API wanting a string: a
// decoded JSON value (json.Number, bool, nil, []any, map[string]any) or a
// repeated form field ([]string).
func Received(v any) string {
	switch x := v.(type) {
	case nil:
		return "Received null"
	case []any, []string:
		return "Received an instance of Array"
	case map[string]any:
		return "Received an instance of Object"
	case bool:
		if x {
			return "Received type boolean (true)"
		}
		return "Received type boolean (false)"
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			f = StringToNumber(string(x))
		}
		text := NumberString(f)
		if f == 0 && len(x) > 0 && x[0] == '-' {
			text = "-0"
		}
		if len(text) > 28 {
			text = text[:25] + "..."
		}
		return "Received type number (" + text + ")"
	}
	return "Received undefined"
}
