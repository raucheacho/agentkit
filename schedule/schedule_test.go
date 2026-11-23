package schedule

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/turn"
)

var paris, _ = time.LoadLocation("Europe/Paris")

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, paris)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseAndNext(t *testing.T) {
	now := at("2026-11-23 10:17") // lundi
	cases := []struct {
		spec, canon string
		next        []string
	}{
		{"every 2h", "every 2h", []string{"2026-11-23 12:17", "2026-11-23 14:17"}},
		{"EVERY 90m", "every 1h30m", []string{"2026-11-23 11:47"}},
		{"every 1d", "every 1d", []string{"2026-11-24 10:17"}},
		{"in 20m", "at 2026-11-23T10:37:00+01:00", []string{"2026-11-23 10:37", ""}},
		{"2026-12-01 09:00", "at 2026-12-01T09:00:00+01:00", []string{"2026-12-01 09:00", ""}},
		{"0 9 * * 1-5", "0 9 * * 1-5", []string{"2026-11-24 09:00", "2026-11-25 09:00", "2026-11-26 09:00", "2026-11-27 09:00", "2026-11-30 09:00"}},
		{"*/15 * * * *", "*/15 * * * *", []string{"2026-11-23 10:30", "2026-11-23 10:45"}},
		{"30 8 1 * *", "30 8 1 * *", []string{"2026-12-01 08:30", "2027-01-01 08:30"}},
		{"0 0 * * 7", "0 0 * * 7", []string{"2026-11-29 00:00"}}, // 7 = dimanche
		{"@daily", "@daily", []string{"2026-11-24 00:00"}},
		{"0 12 13 * 5", "0 12 13 * 5", []string{"2026-11-27 12:00", "2026-12-04 12:00"}}, // le 13 OU un vendredi
	}
	for _, c := range cases {
		sp, err := Parse(c.spec, paris, now)
		if err != nil {
			t.Fatalf("%s : %v", c.spec, err)
		}
		if sp.String() != c.canon {
			t.Errorf("%s : forme canonique %q, attendu %q", c.spec, sp.String(), c.canon)
		}
		// La forme canonique se relit à l'identique.
		if again, err := Parse(sp.String(), paris, now); err != nil || again.String() != sp.String() {
			t.Errorf("%s : relecture %v %v", c.spec, again, err)
		}
		cur := now
		for i, want := range c.next {
			cur = sp.Next(cur)
			if want == "" {
				if !cur.IsZero() {
					t.Errorf("%s : échéance %d inattendue %s", c.spec, i, cur)
				}
				break
			}
			if !cur.Equal(at(want)) {
				t.Errorf("%s : échéance %d = %s, attendu %s", c.spec, i, cur.In(paris), want)
			}
		}
	}
	for _, bad := range []string{"", "every", "every -1h", "every 0m", "0 25 * * *", "* * * *", "*/0 * * * *", "demain", "5-1 * * * *"} {
		if _, err := Parse(bad, paris, now); err == nil {
			t.Errorf("%q accepté", bad)
		}
	}
}

// Le passage à l'heure d'hiver ne fait ni sauter ni doubler une échéance quotidienne.
func TestCronAcrossDST(t *testing.T) {
	sp, _ := Parse("30 9 * * *", paris, at("2026-10-24 12:00"))
	n := sp.Next(at("2026-10-24 12:00"))
	n2 := sp.Next(n)
	if !n.Equal(at("2026-10-25 09:30")) || !n2.Equal(at("2026-10-26 09:30")) {
		t.Fatal(n, n2)
	}
}

type submitter struct {
	mu    sync.Mutex
	turns []*turn.Turn
}

func (s *submitter) Submit(_ context.Context, t *turn.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns = append(s.turns, t)
	return nil
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Set(t time.Time)     { c.mu.Lock(); c.now = t; c.mu.Unlock() }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

var conv = tool.Conversation{Session: "tg:1", Channel: "tg", ChatID: "1", Sender: "ana", Source: "user"}

func TestSchedulerFiresOncePerSlot(t *testing.T) {
	ck := &clock{now: at("2026-11-23 10:00")}
	sub := &submitter{}
	store := NewMemoryStore()
	s := New(Config{Store: store, Submit: sub, Location: paris, Now: ck.Now})
	ctx := context.Background()
	j, err := s.Add(ctx, conv, "", "every 1h", "Fais le point sur la boîte mail", true)
	if err != nil {
		t.Fatal(err)
	}
	if !j.NextRun.Equal(at("2026-11-23 11:00")) || j.Name != "Fais le point sur la boîte mail" {
		t.Fatalf("tâche : %+v", j)
	}
	_ = s.Tick(ctx) // rien d'échu
	ck.Set(at("2026-11-23 11:00"))
	_ = s.Tick(ctx)
	_ = s.Tick(ctx) // même échéance : rien de plus
	if len(sub.turns) != 1 {
		t.Fatalf("%d tours", len(sub.turns))
	}
	tr := sub.turns[0]
	if tr.Source != turn.SourceSchedule || tr.Session != "tg:1" || tr.Channel != "tg" || tr.Meta["job_id"] != j.ID ||
		tr.Meta[turn.MetaSilence] != "[SILENCE]" || !strings.Contains(tr.Content, "boîte mail") || !strings.Contains(tr.Content, "[SILENCE]") {
		t.Fatalf("tour : %+v", tr)
	}
	got, _ := store.Get(ctx, j.ID)
	if !got.NextRun.Equal(at("2026-11-23 12:00")) || !got.LastRun.Equal(at("2026-11-23 11:00")) {
		t.Fatalf("après : %+v", got)
	}
}

// Après une longue coupure, les échéances manquées sont sautées, pas
// rattrapées en rafale ; un retard court est rattrapé une fois.
func TestMissedSlots(t *testing.T) {
	ck := &clock{now: at("2026-11-23 10:00")}
	sub := &submitter{}
	store := NewMemoryStore()
	s := New(Config{Store: store, Submit: sub, Location: paris, Now: ck.Now, Grace: 30 * time.Minute})
	ctx := context.Background()
	j, _ := s.Add(ctx, conv, "x", "every 1h", "t", false)

	ck.Set(at("2026-11-23 11:10")) // 10 min de retard : rattrapée
	_ = s.Tick(ctx)
	ck.Set(at("2026-11-23 17:45")) // arrêt de 6 h : sautée
	_ = s.Tick(ctx)
	if len(sub.turns) != 1 {
		t.Fatalf("%d tours", len(sub.turns))
	}
	got, _ := store.Get(ctx, j.ID)
	if !got.NextRun.Equal(at("2026-11-23 18:00")) || !got.LastRun.Equal(at("2026-11-23 11:00")) {
		t.Fatalf("après : %+v", got)
	}
}

func TestOneShotDisables(t *testing.T) {
	ck := &clock{now: at("2026-11-23 10:00")}
	sub := &submitter{}
	store := NewMemoryStore()
	s := New(Config{Store: store, Submit: sub, Location: paris, Now: ck.Now})
	ctx := context.Background()
	j, _ := s.Add(ctx, conv, "rappel", "in 20m", "Rappelle le dentiste", false)
	ck.Add(25 * time.Minute)
	_ = s.Tick(ctx)
	_ = s.Tick(ctx)
	got, _ := store.Get(ctx, j.ID)
	if len(sub.turns) != 1 || got.Enabled || !got.NextRun.IsZero() || sub.turns[0].Meta[turn.MetaSilence] != "" {
		t.Fatalf("tours %d, tâche %+v", len(sub.turns), got)
	}
	if err := s.Resume(ctx, j.ID); err == nil {
		t.Fatal("reprise d'une tâche ponctuelle passée")
	}
	if _, err := s.Add(ctx, conv, "", "2020-01-01 10:00", "t", false); err == nil {
		t.Fatal("date passée acceptée")
	}
}

func run(t *testing.T, tl tool.Tool, c tool.Conversation, args string) string {
	t.Helper()
	res, err := tl.Run(tool.WithConversation(context.Background(), c), tool.Call{Raw: []byte(args)})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content
}

func TestTools(t *testing.T) {
	ck := &clock{now: at("2026-11-23 10:00")}
	s := New(Config{Store: NewMemoryStore(), Location: paris, Now: ck.Now, MaxPerSession: 2})
	tools := map[string]tool.Tool{}
	for _, tl := range s.Tools() {
		if !tl.Spec().MainOnly {
			t.Errorf("%s n'est pas réservé à l'agent principal", tl.Spec().Name)
		}
		tools[tl.Spec().Name] = tl
	}
	out := run(t, tools["schedule_create"], conv, `{"when":"0 8 * * 1-5","task":"Résume mes rendez-vous du jour","name":"agenda"}`)
	if !strings.Contains(out, "planifiée") || !strings.Contains(out, "2026-11-24 08:00") {
		t.Fatalf("création : %s", out)
	}
	if out := run(t, tools["schedule_create"], conv, `{"when":"every 1m","task":"x"}`); !strings.Contains(out, "trop fréquent") {
		t.Fatalf("intervalle minimal : %s", out)
	}
	sched := conv
	sched.Source = "schedule"
	if out := run(t, tools["schedule_create"], sched, `{"when":"every 1h","task":"x"}`); !strings.HasPrefix(out, "Refusé") {
		t.Fatalf("planification depuis un tour planifié : %s", out)
	}
	_ = run(t, tools["schedule_create"], conv, `{"when":"every 1h","task":"b"}`)
	if out := run(t, tools["schedule_create"], conv, `{"when":"every 2h","task":"c"}`); !strings.HasPrefix(out, "Refusé") {
		t.Fatalf("limite par conversation : %s", out)
	}
	list := run(t, tools["schedule_list"], conv, `{}`)
	if !strings.Contains(list, "« agenda » (0 8 * * 1-5") {
		t.Fatalf("liste : %s", list)
	}
	id := strings.Fields(list)[1]
	other := tool.Conversation{Session: "tg:2"}
	if out := run(t, tools["schedule_cancel"], other, `{"id":"`+id+`"}`); !strings.HasPrefix(out, "Erreur") {
		t.Fatalf("annulation depuis une autre conversation : %s", out)
	}
	if out := run(t, tools["schedule_cancel"], conv, `{"id":"`+id+`"}`); !strings.Contains(out, "supprimée") {
		t.Fatal(out)
	}
}
