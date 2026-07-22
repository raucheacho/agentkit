// Package tools provides agent capabilities for interacting with the environment.
package tools

import (
	"context"
	"fmt"
	"reflect"
)

// Tool is the interface that all agent tools must implement.
type Tool interface {
	// Name returns the tool name used in function calls.
	Name() string
	// Description returns a description of what the tool does.
	Description() string
	// Parameters returns the JSON Schema for tool parameters.
	Parameters() map[string]interface{}
	// Execute runs the tool with the given parameters.
	Execute(ctx context.Context, params map[string]interface{}) (string, error)
}

// validateParams validates tool parameters against JSON schema.
func validateParams(params map[string]interface{}, schema map[string]interface{}) error {
	if schema == nil {
		return nil
	}

	schemaType, _ := schema["type"].(string)
	if schemaType != "object" && schemaType != "" {
		return fmt.Errorf("schema must be object type, got %s", schemaType)
	}

	// Check required fields
	if required, ok := schema["required"].([]interface{}); ok {
		for _, req := range required {
			reqStr, ok := req.(string)
			if !ok {
				continue
			}
			if _, exists := params[reqStr]; !exists {
				return fmt.Errorf("missing required parameter: %s", reqStr)
			}
		}
	}

	// Validate properties
	if properties, ok := schema["properties"].(map[string]interface{}); ok {
		for key, value := range params {
			if propSchema, exists := properties[key]; exists {
				if err := validateValue(value, propSchema.(map[string]interface{}), key); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// validateValue validates a single value against its schema.
func validateValue(value interface{}, schema map[string]interface{}, path string) error {
	expectedType, _ := schema["type"].(string)

	// Type validation
	if expectedType != "" {
		actualType := getJSONType(value)
		if actualType != expectedType {
			// Allow number to be integer or float
			if expectedType == "number" && (actualType == "integer" || actualType == "number") {
				// OK
			} else if expectedType == "integer" && actualType == "number" {
				// Check if it's actually an integer
				if _, ok := value.(int); !ok && value != float64(int64(value.(float64))) {
					return fmt.Errorf("%s should be integer, got %s", path, actualType)
				}
			} else {
				return fmt.Errorf("%s should be %s, got %s", path, expectedType, actualType)
			}
		}
	}

	// Enum validation
	if enum, ok := schema["enum"].([]interface{}); ok {
		found := false
		for _, allowed := range enum {
			if reflect.DeepEqual(value, allowed) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s must be one of %v", path, enum)
		}
	}

	// String validations
	if str, ok := value.(string); ok {
		if minLen, ok := schema["minLength"].(float64); ok {
			if len(str) < int(minLen) {
				return fmt.Errorf("%s must be at least %d characters", path, int(minLen))
			}
		}
		if maxLen, ok := schema["maxLength"].(float64); ok {
			if len(str) > int(maxLen) {
				return fmt.Errorf("%s must be at most %d characters", path, int(maxLen))
			}
		}
	}

	// Number validations
	if num, ok := value.(float64); ok {
		if min, ok := schema["minimum"].(float64); ok {
			if num < min {
				return fmt.Errorf("%s must be >= %v", path, min)
			}
		}
		if max, ok := schema["maximum"].(float64); ok {
			if num > max {
				return fmt.Errorf("%s must be <= %v", path, max)
			}
		}
	}

	// Array validation
	if arr, ok := value.([]interface{}); ok {
		if itemsSchema, ok := schema["items"].(map[string]interface{}); ok {
			for i, item := range arr {
				if err := validateValue(item, itemsSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		if minItems, ok := schema["minItems"].(float64); ok {
			if len(arr) < int(minItems) {
				return fmt.Errorf("%s must have at least %d items", path, int(minItems))
			}
		}
		if maxItems, ok := schema["maxItems"].(float64); ok {
			if len(arr) > int(maxItems) {
				return fmt.Errorf("%s must have at most %d items", path, int(maxItems))
			}
		}
	}

	// Object validation
	if obj, ok := value.(map[string]interface{}); ok {
		if propSchema, ok := schema["properties"].(map[string]interface{}); ok {
			for k, v := range obj {
				if ps, exists := propSchema[k]; exists {
					if err := validateValue(v, ps.(map[string]interface{}), fmt.Sprintf("%s.%s", path, k)); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// getJSONType returns the JSON type name for a Go value.
func getJSONType(v interface{}) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case int, int8, int16, int32, int64:
		return "integer"
	case uint, uint8, uint16, uint32, uint64:
		return "integer"
	case float32, float64:
		// Check if it's actually an integer
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			return "integer"
		}
		return "number"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	case nil:
		return "null"
	default:
		// Check for slice and map types
		reflectType := reflect.TypeOf(v)
		switch reflectType.Kind() {
		case reflect.Slice, reflect.Array:
			return "array"
		case reflect.Map:
			return "object"
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return "integer"
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return "integer"
		case reflect.Float32, reflect.Float64:
			return "number"
		case reflect.Bool:
			return "boolean"
		case reflect.String:
			return "string"
		default:
			return "object"
		}
	}
}
