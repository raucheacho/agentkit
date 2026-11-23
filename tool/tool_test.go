package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type slot struct {
	Day  string `json:"day" enum:"lun,mar"`
	Hour int    `json:"hour"`
}

type bookIn struct {
	Name  string            `json:"name" desc:"nom du client"`
	Slots []slot            `json:"slots"`
	Note  string            `json:"note,omitempty"`
	Tags  map[string]string `json:"tags,omitempty"`
	Ptr   *int              `json:"ptr"`
	skip  string
	Drop  string `json:"-"`
}

func TestSchemaOfNested(t *testing.T) {
	raw, err := SchemaOf(bookIn{})
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	_ = json.Unmarshal(raw, &s)
	props := s["properties"].(map[string]any)
	if _, ok := props["Drop"]; ok || props["skip"] != nil {
		t.Fatal("champs ignorés présents")
	}
	items := props["slots"].(map[string]any)["items"].(map[string]any)
	if items["properties"].(map[string]any)["day"].(map[string]any)["enum"] == nil {
		t.Fatalf("enum imbriqué absent : %s", raw)
	}
	if props["name"].(map[string]any)["description"] != "nom du client" {
		t.Fatal("description absente")
	}
	req := s["required"].([]any)
	if len(req) != 2 || req[0] != "name" || req[1] != "slots" {
		t.Fatalf("required : %v", req)
	}
}

func TestValidate(t *testing.T) {
	raw, _ := SchemaOf(bookIn{})
	var schema map[string]any
	_ = json.Unmarshal(raw, &schema)
	cases := map[string]string{
		`{"slots":[]}`: "obligatoire manquant : name",
		`{"name":"a","slots":[{"day":"dim","hour":9}]}`:   "slots[0].day",
		`{"name":"a","slots":[{"day":"lun","hour":9.5}]}`: "integer",
		`{"name":"a","slots":[{"day":"lun","hour":9}]}`:   "",
	}
	for in, want := range cases {
		var args map[string]any
		_ = json.Unmarshal([]byte(in), &args)
		err := Validate(args, schema)
		if want == "" {
			if err != nil {
				t.Errorf("%s : %v", in, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s : erreur %v, attendu %q", in, err, want)
		}
	}
}

func TestRegistryOrderAvailabilityAndResolve(t *testing.T) {
	r := NewRegistry()
	noop := func(ctx context.Context, c Call) (Result, error) { return Text("ok"), nil }
	on := false
	if err := r.Add(Func("b_tool", "", nil, noop), Func("a_tool", "", nil, noop)); err != nil {
		t.Fatal(err)
	}
	if err := r.AddSet(Toolset{Name: "web", Tools: []Tool{Func("web_search", "", nil, noop)},
		Available: func(context.Context) bool { return on }}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(Func("a_tool", "", nil, noop)); err == nil {
		t.Fatal("doublon accepté")
	}
	names := func() string {
		var n []string
		for _, s := range r.Specs(context.Background(), nil) {
			n = append(n, s.Name)
		}
		return strings.Join(n, ",")
	}
	if got := names(); got != "b_tool,a_tool" {
		t.Fatalf("specs : %s", got)
	}
	on = true
	if got := names(); got != "b_tool,a_tool,web_search" {
		t.Fatalf("specs : %s", got)
	}
	specs := r.Specs(context.Background(), nil)
	if Resolve("functions.Web-Search<|channel|>x", specs) != "web_search" || Resolve("nope", specs) != "" {
		t.Fatal("résolution de nom")
	}
}

func TestNewDecodesTypedArgs(t *testing.T) {
	var got bookIn
	tl := New("book", "d", func(ctx context.Context, in bookIn) (Result, error) {
		got = in
		return Text("ok"), nil
	}, WithEffect(Sensitive))
	if tl.Spec().Effect != Sensitive {
		t.Fatal("effet")
	}
	_, err := tl.Run(context.Background(), Call{Raw: json.RawMessage(`{"name":"Ana","slots":[{"day":"mar","hour":10}]}`)})
	if err != nil || got.Name != "Ana" || got.Slots[0].Hour != 10 {
		t.Fatalf("%v %+v", err, got)
	}
}
