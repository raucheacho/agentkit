package llm

import "testing"

func tc(id string) ToolCall {
	c := ToolCall{ID: id, Type: "function"}
	c.Function.Name = "x"
	return c
}

func roles(ms []Message) string {
	s := ""
	for _, m := range ms {
		s += m.Role[:1]
	}
	return s
}

func TestTrimHistoryCutsOnUser(t *testing.T) {
	h := []Message{
		{Role: "user"}, {Role: "assistant", ToolCalls: []ToolCall{tc("1")}}, {Role: "tool", ToolCallID: "1"}, {Role: "assistant"},
		{Role: "user"}, {Role: "assistant"},
	}
	got := TrimHistory(h, 3)
	if roles(got) != "ua" {
		t.Fatalf("obtenu %s", roles(got))
	}
	if roles(TrimHistory(h, 10)) != "uatau"+"a" {
		t.Fatal("rien ne doit être coupé sous la limite")
	}
}

func TestSanitizeHistory(t *testing.T) {
	h := []Message{
		{Role: "tool", ToolCallID: "0"}, // début orphelin
		{Role: "user"},
		{Role: "assistant", ToolCalls: []ToolCall{tc("1"), tc("2")}},
		{Role: "tool", ToolCallID: "1"},
		{Role: "tool", ToolCallID: "9"}, // orphelin
		{Role: "user"},
	}
	got := SanitizeHistory(h)
	if roles(got) != "uattu" {
		t.Fatalf("obtenu %s", roles(got))
	}
	if got[3].ToolCallID != "2" || got[3].Content != MissingToolResult {
		t.Fatalf("résultat manquant non complété : %+v", got[3])
	}
	if len(h) != 6 {
		t.Fatal("l'original a été modifié")
	}
}
