package tool

import (
	"strconv"
	"strings"
)

// Coerce corrige, selon le schéma, les valeurs que les modèles envoient
// souvent en texte : "2" pour un entier, "1.5" pour un nombre, "true" pour
// un booléen. Seule une chaîne qui représente exactement une valeur du type
// attendu est convertie ; tout le reste est laissé à la validation. Les
// objets et tableaux imbriqués sont parcourus. args est modifié sur place.
func Coerce(args map[string]any, schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	for name, v := range args {
		if sub, ok := props[name].(map[string]any); ok {
			args[name] = coerceValue(v, sub)
		}
	}
}

func coerceValue(v any, schema map[string]any) any {
	switch x := v.(type) {
	case string:
		s := strings.TrimSpace(x)
		switch schema["type"] {
		case "integer":
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return float64(n) // comme encoding/json
			}
		case "number":
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		case "boolean":
			switch strings.ToLower(s) {
			case "true":
				return true
			case "false":
				return false
			}
		}
	case map[string]any:
		if schema["type"] == "object" {
			Coerce(x, schema)
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i := range x {
				x[i] = coerceValue(x[i], items)
			}
		}
	}
	return v
}
