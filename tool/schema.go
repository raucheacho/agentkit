package tool

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// SchemaOf déduit un JSON Schema d'objet d'une struct Go.
//
// Règles :
//   - le nom vient de la balise json ; un champ sans balise garde son nom ;
//     json:"-" et les champs non exportés sont ignorés ;
//   - un champ est obligatoire sauf s'il porte omitempty ou est un pointeur ;
//   - desc:"…" donne la description, enum:"a,b,c" les valeurs permises ;
//   - les structs, tranches et maps imbriquées sont décrites récursivement.
func SchemaOf(v any) (json.RawMessage, error) {
	t := reflect.TypeOf(v)
	if t == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`), nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("SchemaOf attend une struct, reçu %s", t.Kind())
	}
	s, err := schemaFor(t, map[reflect.Type]bool{})
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

var timeType = reflect.TypeOf(time.Time{})

func schemaFor(t reflect.Type, seen map[reflect.Type]bool) (map[string]any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return map[string]any{"type": "string", "format": "date-time"}, nil
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Slice, reflect.Array:
		items, err := schemaFor(t.Elem(), seen)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map à clé non chaîne : %s", t)
		}
		vals, err := schemaFor(t.Elem(), seen)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": vals}, nil
	case reflect.Interface:
		return map[string]any{}, nil
	case reflect.Struct:
		if seen[t] {
			return nil, fmt.Errorf("type récursif : %s", t)
		}
		seen[t] = true
		defer delete(seen, t)
		props := map[string]any{}
		required := []string{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, omit := f.Name, false
			if tag, ok := f.Tag.Lookup("json"); ok {
				parts := strings.Split(tag, ",")
				if parts[0] == "-" {
					continue
				}
				if parts[0] != "" {
					name = parts[0]
				}
				for _, p := range parts[1:] {
					if p == "omitempty" || p == "omitzero" {
						omit = true
					}
				}
			}
			ps, err := schemaFor(f.Type, seen)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if d := f.Tag.Get("desc"); d != "" {
				ps["description"] = d
			}
			if e := f.Tag.Get("enum"); e != "" {
				ps["enum"] = strings.Split(e, ",")
			}
			props[name] = ps
			if !omit && f.Type.Kind() != reflect.Pointer {
				required = append(required, name)
			}
		}
		out := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			out["required"] = required
		}
		return out, nil
	}
	return nil, fmt.Errorf("type non pris en charge : %s", t)
}
