package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/embeddings" || body.Model != "emb" || len(body.Input) != 2 || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, `{"error":{"message":"mauvaise requête"}}`, 400)
			return
		}
		// Ordre inversé : l'index fait foi.
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	e := NewEmbedder(Config{BaseURL: srv.URL, APIKey: "k"}, "emb")
	vecs, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil || len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Fatal(vecs, err)
	}
	bad := NewEmbedder(Config{BaseURL: srv.URL}, "emb")
	if _, err := bad.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("erreur attendue")
	}
}
