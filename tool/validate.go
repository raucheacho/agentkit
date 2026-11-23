package tool

import (
	"fmt"
	"math"
	"reflect"
)

// Validate vérifie des arguments décodés contre un JSON Schema décodé. Elle
// couvre ce que les modèles ratent en pratique : paramètres obligatoires,
// types, enums, bornes de longueur et de valeur, éléments de tableaux et
// objets imbriqués. Les mots-clés non gérés sont ignorés.
func Validate(args map[string]any, schema map[string]any) error {
	if schema == nil {
		return nil
	}
	return validateValue(args, schema, "")
}

func validateValue(v any, schema map[string]any, path string) error {
	label := path
	if label == "" {
		label = "arguments"
	}
	if want, _ := schema["type"].(string); want != "" {
		if !typeMatches(v, want) {
			return fmt.Errorf("%s doit être de type %s, reçu %s", label, want, jsonType(v))
		}
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		found := false
		for _, e := range enum {
			if reflect.DeepEqual(e, v) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s doit valoir l'une de ces valeurs : %v", label, enum)
		}
	}

	switch x := v.(type) {
	case string:
		n := len([]rune(x))
		if m, ok := num(schema["minLength"]); ok && float64(n) < m {
			return fmt.Errorf("%s doit faire au moins %v caractères", label, m)
		}
		if m, ok := num(schema["maxLength"]); ok && float64(n) > m {
			return fmt.Errorf("%s doit faire au plus %v caractères", label, m)
		}
	case float64:
		if m, ok := num(schema["minimum"]); ok && x < m {
			return fmt.Errorf("%s doit être ≥ %v", label, m)
		}
		if m, ok := num(schema["maximum"]); ok && x > m {
			return fmt.Errorf("%s doit être ≤ %v", label, m)
		}
	case []any:
		if m, ok := num(schema["minItems"]); ok && float64(len(x)) < m {
			return fmt.Errorf("%s doit contenir au moins %v éléments", label, m)
		}
		if m, ok := num(schema["maxItems"]); ok && float64(len(x)) > m {
			return fmt.Errorf("%s doit contenir au plus %v éléments", label, m)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, it := range x {
				if err := validateValue(it, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				name, _ := r.(string)
				if _, present := x[name]; name != "" && !present {
					return fmt.Errorf("paramètre obligatoire manquant : %s", join(path, name))
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for k, val := range x {
			if ps, ok := props[k].(map[string]any); ok {
				if err := validateValue(val, ps, join(path, k)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func typeMatches(v any, want string) bool {
	got := jsonType(v)
	switch want {
	case "number":
		return got == "number" || got == "integer"
	case "integer":
		return got == "integer"
	}
	return got == want
}

// jsonType renvoie le type JSON d'une valeur issue de encoding/json.
func jsonType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}
