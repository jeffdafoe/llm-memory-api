package mcp

import (
	_ "embed"
	"fmt"
	"math"
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// tools.json is Node's TOOLS array (routes/mcp.js) as JSON.stringify wrote
// it, indented. tools/list returns these definitions, so their text, key
// order and the order of the tools are what clients see; the golden test
// compares them with Node's.
//
//go:embed tools.json
var toolsJSON []byte

// toolDef is one entry of tools.json.
type toolDef struct {
	name string
	// def is the whole definition as a JavaScript value, for tools/list.
	def *jsutil.Object
	// schema is def.inputSchema.
	schema *jsutil.Object
}

var (
	toolDefs    []toolDef
	toolSchemas = map[string]*jsutil.Object{}
)

func init() {
	v, err := jsutil.Parse(toolsJSON)
	if err != nil {
		panic("mcp: tools.json: " + err.Error())
	}
	list, ok := v.([]any)
	if !ok {
		panic("mcp: tools.json is not an array")
	}
	for _, entry := range list {
		def, ok := entry.(*jsutil.Object)
		if !ok {
			panic("mcp: tools.json entry is not an object")
		}
		name, _ := def.Value("name").(string)
		schema, _ := def.Value("inputSchema").(*jsutil.Object)
		if name == "" || schema == nil {
			panic("mcp: tools.json entry without a name or inputSchema")
		}
		toolDefs = append(toolDefs, toolDef{name: name, def: def, schema: schema})
		toolSchemas[name] = schema
	}
}

// toolPermissions is Node's TOOL_PERMISSIONS: the agent_permissions name a
// tool needs.
var toolPermissions = map[string]string{
	"search":                  "mcp_search",
	"save_note":               "mcp_save_note",
	"list_notes":              "mcp_list_notes",
	"read_note":               "mcp_read_note",
	"delete_note":             "mcp_delete_note",
	"restore_note":            "mcp_delete_note",
	"edit_note":               "mcp_save_note",
	"move_note":               "mcp_save_note",
	"move_folder":             "mcp_save_note",
	"grep":                    "mcp_search",
	"read_instructions":       "mcp_read_note",
	"save_instructions":       "mcp_save_note",
	"chat_send":               "mcp_chat_send",
	"chat_receive":            "mcp_chat_receive",
	"chat_ack":                "mcp_chat_ack",
	"chat_status":             "mcp_chat_status",
	"mail_send":               "mcp_mail_send",
	"mail_check":              "mcp_mail_receive",
	"mail_receive":            "mcp_mail_receive",
	"mail_ack":                "mcp_mail_ack",
	"mail_edit":               "mcp_mail_send",
	"mail_unsend":             "mcp_mail_send",
	"mail_sent":               "mcp_mail_send",
	"mail_history":            "mcp_mail_receive",
	"agent_status":            "mcp_agent_status",
	"update_expertise":        "mcp_agent_status",
	"update_profile":          "mcp_agent_status",
	"activity_start":          "mcp_agent_status",
	"activity_stop":           "mcp_agent_status",
	"discussion_create":       "mcp_discussion_create",
	"discussion_list":         "mcp_discussion_list",
	"discussion_status":       "mcp_discussion_status",
	"discussion_pending":      "mcp_discussion_pending",
	"discussion_conclude":     "mcp_discussion_conclude",
	"discussion_cancel":       "mcp_discussion_conclude",
	"discussion_join":         "mcp_discussion_join",
	"discussion_leave":        "mcp_discussion_leave",
	"discussion_defer":        "mcp_discussion_join",
	"discussion_vote_propose": "mcp_discussion_vote_propose",
	"discussion_vote_cast":    "mcp_discussion_vote_cast",
	"discussion_vote_status":  "mcp_discussion_vote_status",
}

// inheritedMembers are the names Node's lookup tables (TOOL_PERMISSIONS,
// TOOL_SCHEMAS, TOOL_HANDLERS, plain object literals) answer from
// Object.prototype, with the text the member has in a template string.
// A tools/call naming one finds a "permission" no agent holds.
var inheritedMembers = map[string]string{
	"constructor":          "function Object() { [native code] }",
	"__defineGetter__":     "function __defineGetter__() { [native code] }",
	"__defineSetter__":     "function __defineSetter__() { [native code] }",
	"hasOwnProperty":       "function hasOwnProperty() { [native code] }",
	"__lookupGetter__":     "function __lookupGetter__() { [native code] }",
	"__lookupSetter__":     "function __lookupSetter__() { [native code] }",
	"isPrototypeOf":        "function isPrototypeOf() { [native code] }",
	"propertyIsEnumerable": "function propertyIsEnumerable() { [native code] }",
	"toString":             "function toString() { [native code] }",
	"valueOf":              "function valueOf() { [native code] }",
	"__proto__":            "[object Object]",
	"toLocaleString":       "function toLocaleString() { [native code] }",
}

// permission is TOOL_PERMISSIONS[name] as Node read it.
type permission struct {
	// text is the value in a template string ("Permission denied: <text>").
	text string
	// inherited is true for an Object.prototype member, which no
	// permissions list includes.
	inherited bool
}

// toolPermission returns TOOL_PERMISSIONS[name], and false when it is
// undefined (an unknown tool).
func toolPermission(name string) (permission, bool) {
	if p, ok := toolPermissions[name]; ok {
		return permission{text: p}, true
	}
	if text, ok := inheritedMembers[name]; ok {
		return permission{text: text, inherited: true}, true
	}
	return permission{}, false
}

// held is hasPermission: req.mcpPermissions.includes(permission).
func (p permission) held(granted []string) bool {
	if p.inherited {
		return false
	}
	for _, g := range granted {
		if g == p.text {
			return true
		}
	}
	return false
}

// MaxArrayItems is Node's MAX_ARRAY_ITEMS: the longest array a tool argument
// may be.
const MaxArrayItems = 1000

func badArgs(format string, args ...any) error {
	return apierr.New(http.StatusBadRequest, fmt.Sprintf(format, args...))
}

// validateToolArgs is Node's validateToolArgs: the arguments checked against
// the tool's inputSchema, every refusal a 400. args is a jsutil value, or
// jsutil.Undefined when the request had no arguments; the result is the
// arguments object (a new empty one for undefined).
func validateToolArgs(toolName string, args any) (*jsutil.Object, error) {
	schema, known := toolSchemas[toolName]
	if !known {
		if _, inherited := inheritedMembers[toolName]; !inherited {
			return nil, badArgs("Unknown tool: %s", toolName)
		}
		// TOOL_SCHEMAS[name] is an Object.prototype member: it has no
		// properties or required list, so any object passes.
	}

	if args == jsutil.Undefined {
		args = jsutil.NewObject()
	}
	obj, ok := args.(*jsutil.Object)
	if !ok {
		return nil, badArgs("Tool arguments must be an object, got %s", typeName(args))
	}
	if schema == nil {
		return obj, nil
	}

	properties, _ := schema.Value("properties").(*jsutil.Object)
	if properties == nil {
		properties = jsutil.NewObject()
	}
	required, _ := schema.Value("required").([]any)

	for _, f := range required {
		fieldName, _ := f.(string)
		value, has := obj.Get(fieldName)
		if !has || value == nil || value == jsutil.Undefined {
			return nil, badArgs("%s: \"%s\" is required", toolName, fieldName)
		}
		fieldSchema, _ := properties.Value(fieldName).(*jsutil.Object)
		if fieldSchema != nil && fieldSchema.Value("type") == "string" {
			s, isString := value.(string)
			if !isString || jsutil.Trim(s) == "" {
				return nil, badArgs("%s: \"%s\" must be a non-empty string", toolName, fieldName)
			}
		}
	}

	for _, fieldName := range properties.Keys() {
		value, has := obj.Get(fieldName)
		if !has || value == jsutil.Undefined {
			continue
		}
		fieldSchema, _ := properties.Value(fieldName).(*jsutil.Object)
		var expected any
		if fieldSchema != nil {
			expected = fieldSchema.Value("type")
		}
		switch expected {
		case "string":
			if _, ok := value.(string); !ok {
				return nil, badArgs("%s: \"%s\" must be a string", toolName, fieldName)
			}
		case "number":
			if !finiteNumber(value) {
				return nil, badArgs("%s: \"%s\" must be a finite number", toolName, fieldName)
			}
		case "boolean":
			if _, ok := value.(bool); !ok {
				return nil, badArgs("%s: \"%s\" must be a boolean", toolName, fieldName)
			}
		case "array":
			items, ok := value.([]any)
			if !ok {
				return nil, badArgs("%s: \"%s\" must be an array", toolName, fieldName)
			}
			if len(items) > MaxArrayItems {
				return nil, badArgs("%s: \"%s\" exceeds maximum array length (%d > %d)", toolName, fieldName, len(items), MaxArrayItems)
			}
			var itemType any
			if itemSchema, ok := fieldSchema.Value("items").(*jsutil.Object); ok {
				itemType = itemSchema.Value("type")
			}
			if jsutil.Truthy(itemType) {
				for i, item := range items {
					if !itemMatches(itemType, item) {
						typeText, _ := jsutil.ValueString(itemType)
						return nil, badArgs("%s: \"%s[%d]\" must be a %s", toolName, fieldName, i, typeText)
					}
				}
			}
		case "object":
			if _, ok := value.(*jsutil.Object); !ok {
				return nil, badArgs("%s: \"%s\" must be an object", toolName, fieldName)
			}
		}
	}
	return obj, nil
}

func itemMatches(itemType, item any) bool {
	switch itemType {
	case "string":
		_, ok := item.(string)
		return ok
	case "number":
		return finiteNumber(item)
	case "boolean":
		_, ok := item.(bool)
		return ok
	}
	return false
}

// finiteNumber is typeof v === 'number' && Number.isFinite(v). A JSON number
// too large for a double reads as Infinity, as JSON.parse reads it.
func finiteNumber(v any) bool {
	f, ok := jsutil.AsFloat(v)
	return ok && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// typeName is the type validateToolArgs names for an arguments value that is
// not an object.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	return "number"
}
