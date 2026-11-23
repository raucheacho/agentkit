package tool

import (
	"encoding/json"
	"testing"
)

func TestCoerce(t *testing.T) {
	type line struct {
		Qty  int  `json:"qty"`
		Gift bool `json:"gift,omitempty"`
	}
	type in struct {
		Quantity int     `json:"quantity"`
		Price    float64 `json:"price"`
		Express  bool    `json:"express"`
		Note     string  `json:"note"`
		Lines    []line  `json:"lines,omitempty"`
	}
	raw, _ := SchemaOf(in{})
	var schema map[string]any
	_ = json.Unmarshal(raw, &schema)
	var args map[string]any
	_ = json.Unmarshal([]byte(`{"quantity":"2","price":" 1.5 ","express":"TRUE","note":"42","lines":[{"qty":"3","gift":"false"}]}`), &args)
	Coerce(args, schema)
	if err := Validate(args, schema); err != nil {
		t.Fatal(err)
	}
	if args["quantity"] != 2.0 || args["price"] != 1.5 || args["express"] != true || args["note"] != "42" {
		t.Fatalf("%v", args)
	}
	l := args["lines"].([]any)[0].(map[string]any)
	if l["qty"] != 3.0 || l["gift"] != false {
		t.Fatalf("imbriqué : %v", l)
	}
	// Ce qui n'est pas exactement du bon type reste une erreur.
	_ = json.Unmarshal([]byte(`{"quantity":"2 bougies","price":1,"express":"oui","note":"x"}`), &args)
	Coerce(args, schema)
	if err := Validate(args, schema); err == nil {
		t.Fatal("erreur attendue")
	}
}
