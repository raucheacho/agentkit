package computer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/raucheacho/agentkit/computer"
	"github.com/raucheacho/agentkit/computer/local"
	"github.com/raucheacho/agentkit/tool"
)

func TestTools(t *testing.T) {
	ws, err := local.New(local.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var recs []computer.Record
	tools := map[string]tool.Tool{}
	for _, tl := range computer.Tools(ws, computer.ToolsConfig{
		Redact: computer.Redactor("hunter22"),
		OnExec: func(_ context.Context, r computer.Record) { recs = append(recs, r) },
	}) {
		tools[tl.Spec().Name] = tl
	}
	if tools["bash"].Spec().Effect != tool.Write || tools["read_file"].Spec().Effect != tool.Read {
		t.Fatal("effets")
	}
	ctx := tool.WithConversation(context.Background(), tool.Conversation{Session: "web:c1"})
	run := func(name, args string) tool.Result {
		t.Helper()
		res, err := tools[name].Run(ctx, tool.Call{Raw: []byte(args)})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := run("write_file", `{"path":"src/app.py","content":"print('bonjour')\nprint('bonjour')\n"}`); !strings.Contains(r.Content, "écrit") {
		t.Fatal(r.Content)
	}
	if r := run("edit_file", `{"path":"src/app.py","old_string":"bonjour","new_string":"salut"}`); !strings.Contains(r.Content, "2 fois") {
		t.Fatalf("occurrence ambiguë : %s", r.Content)
	}
	run("edit_file", `{"path":"src/app.py","old_string":"bonjour","new_string":"salut","replace_all":true}`)
	if r := run("read_file", `{"path":"src/app.py","offset":2}`); !r.Untrusted || r.Content != "     2\tprint('salut')\n     3\t" {
		t.Fatalf("lecture : %q", r.Content)
	}
	r := run("bash", `{"command":"echo mot de passe hunter22; ls src; exit 2"}`)
	if !r.Untrusted || !strings.HasPrefix(r.Content, "Code de sortie : 2") || !strings.Contains(r.Content, "app.py") {
		t.Fatalf("bash : %q", r.Content)
	}
	if len(recs) != 1 || strings.Contains(recs[0].Command+recs[0].Output, "hunter22") || recs[0].ExitCode != 2 {
		t.Fatalf("enregistrement : %+v", recs)
	}
	if r := run("bash", `{"command":"sleep 5","timeout_seconds":1}`); !strings.Contains(r.Content, "délai") {
		t.Fatalf("délai : %q", r.Content)
	}
	// Un sous-agent travaille dans le dossier de son parent.
	child := tool.WithConversation(context.Background(), tool.Conversation{Session: "delegate:x", Parent: "web:c1"})
	res, _ := tools["read_file"].Run(child, tool.Call{Raw: []byte(`{"path":"src/app.py"}`)})
	if !strings.Contains(res.Content, "salut") {
		t.Fatalf("sous-agent : %q", res.Content)
	}
	if r := run("read_file", `{"path":"../../etc/passwd"}`); !strings.HasPrefix(r.Content, "Erreur") {
		t.Fatalf("évasion : %q", r.Content)
	}
}

func TestOutputBuffer(t *testing.T) {
	o := computer.NewOutput(10)
	for range 100 {
		o.Write([]byte("0123456789"))
	}
	o.Write([]byte("FIN"))
	b, cut := o.Bytes()
	if !cut || !strings.HasPrefix(string(b), "01234") || !strings.HasSuffix(string(b), "89FIN") || !strings.Contains(string(b), "993 octets omis") {
		t.Fatalf("%q", b)
	}
}
