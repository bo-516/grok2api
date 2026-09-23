package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// CheckStrictSchema rejects a function.strict schema OpenAI would not accept.
// The node must be a JSON object with additionalProperties false, and every
// property key must appear in required. Nested objects and array items of type
// object follow the same rule. name is the function name used in the 400 message.
// An empty schema is 400 because strict mode needs parameters.
func CheckStrictSchema(name string, schema json.RawMessage) error {
	if len(bytes.TrimSpace(schema)) == 0 {
		return bad("unsupported_parameter", `function "`+name+`" with strict true requires parameters`)
	}
	var v any
	if json.Unmarshal(schema, &v) != nil {
		return bad("unsupported_parameter", `function "`+name+`" strict schema must be a JSON object`)
	}
	if err := checkStrictNode(name, "parameters", v); err != nil {
		return err
	}
	return nil
}

// checkStrictNode walks one object schema. path is the message location, such as parameters.properties.city.
// A node that is not an object, that omits additionalProperties false, or that leaves a property optional, is 400.
func checkStrictNode(name, path string, v any) error {
	obj, ok := v.(map[string]any)
	if !ok {
		return bad("unsupported_parameter", `function "`+name+`" strict schema at `+path+` must be an object`)
	}
	if t, _ := obj["type"].(string); t != "" && t != "object" {
		return bad("unsupported_parameter", `function "`+name+`" strict schema at `+path+` must have type object`)
	}
	if obj["additionalProperties"] != false {
		return bad("unsupported_parameter", `function "`+name+`" strict schema at `+path+` must set additionalProperties to false`)
	}
	props, _ := obj["properties"].(map[string]any)
	required := map[string]bool{}
	if req, ok := obj["required"].([]any); ok {
		for _, item := range req {
			if s, ok := item.(string); ok {
				required[s] = true
			}
		}
	}
	for key, child := range props {
		if !required[key] {
			return bad("unsupported_parameter", `function "`+name+`" strict schema at `+path+`.properties.`+key+` must be listed in required`)
		}
		childObj, _ := child.(map[string]any)
		if childObj == nil {
			continue
		}
		typ, _ := childObj["type"].(string)
		if typ == "object" || childObj["properties"] != nil {
			if err := checkStrictNode(name, path+".properties."+key, child); err != nil {
				return err
			}
		}
		if typ == "array" {
			if items, ok := childObj["items"].(map[string]any); ok {
				itemType, _ := items["type"].(string)
				if itemType == "object" || items["properties"] != nil {
					if err := checkStrictNode(name, path+".properties."+key+".items", items); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// MatchStrictArgs reports whether args is a JSON object matching a strict schema.
// Required keys must be present, extra keys are rejected, and property types are checked.
// schema is the same object accepted by CheckStrictSchema. A non-object args value fails.
func MatchStrictArgs(schema, args json.RawMessage) error {
	var obj map[string]any
	if json.Unmarshal(args, &obj) != nil {
		return fmt.Errorf("arguments must be a JSON object")
	}
	var sch any
	if json.Unmarshal(schema, &sch) != nil {
		return fmt.Errorf("strict schema is not an object")
	}
	if err := matchNode(sch, obj); err != nil {
		return err
	}
	return nil
}

// matchNode checks one object value against an object schema.
// A missing required key, an extra key, or a wrong property type returns an error naming the key.
func matchNode(schema any, obj map[string]any) error {
	sch, ok := schema.(map[string]any)
	if !ok {
		return fmt.Errorf("strict schema is not an object")
	}
	props, _ := sch["properties"].(map[string]any)
	required := map[string]bool{}
	if req, ok := sch["required"].([]any); ok {
		for _, item := range req {
			s, ok := item.(string)
			if !ok {
				continue
			}
			required[s] = true
			if _, present := obj[s]; !present {
				return fmt.Errorf("missing required property %s", s)
			}
		}
	}
	if sch["additionalProperties"] == false {
		for key := range obj {
			if _, known := props[key]; !known {
				return fmt.Errorf("unexpected property %s", key)
			}
		}
	}
	for key, child := range props {
		val, present := obj[key]
		if !present {
			continue
		}
		if err := matchValue(key, child, val); err != nil {
			return err
		}
	}
	return nil
}

// matchValue checks one property value against its schema node.
// key is included in the error. Nested objects are checked with matchNode. Unknown types are accepted.
func matchValue(key string, schema, val any) error {
	sch, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	if enum, ok := sch["enum"].([]any); ok {
		for _, item := range enum {
			if jsonEqual(item, val) {
				return nil
			}
		}
		return fmt.Errorf("property %s is not in enum", key)
	}
	typ, _ := sch["type"].(string)
	switch typ {
	case "", "null":
		if typ == "null" && val != nil {
			return fmt.Errorf("property %s must be null", key)
		}
		return nil
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("property %s must be a string", key)
		}
	case "number":
		if _, ok := val.(float64); !ok {
			return fmt.Errorf("property %s must be a number", key)
		}
	case "integer":
		n, ok := val.(float64)
		if !ok || n != math.Trunc(n) {
			return fmt.Errorf("property %s must be an integer", key)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("property %s must be a boolean", key)
		}
	case "array":
		items, ok := val.([]any)
		if !ok {
			return fmt.Errorf("property %s must be an array", key)
		}
		if itemSchema, ok := sch["items"].(map[string]any); ok {
			itemType, _ := itemSchema["type"].(string)
			if itemType == "object" {
				for i, item := range items {
					child, ok := item.(map[string]any)
					if !ok {
						return fmt.Errorf("property %s[%d] must be an object", key, i)
					}
					if err := matchNode(itemSchema, child); err != nil {
						return fmt.Errorf("property %s[%d]: %w", key, i, err)
					}
				}
			}
		}
	case "object":
		child, ok := val.(map[string]any)
		if !ok {
			return fmt.Errorf("property %s must be an object", key)
		}
		if err := matchNode(sch, child); err != nil {
			return fmt.Errorf("property %s: %w", key, err)
		}
	}
	return nil
}

// jsonEqual compares two decoded JSON values, including numbers.
// It is used for enum membership. A mismatch returns false.
func jsonEqual(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	if bytes.Equal(ab, bb) {
		return true
	}
	// 1 and 1.0 encode differently after a round trip through map values; compare numbers.
	af, aErr := strconv.ParseFloat(string(ab), 64)
	bf, bErr := strconv.ParseFloat(string(bb), 64)
	return aErr == nil && bErr == nil && af == bf
}
