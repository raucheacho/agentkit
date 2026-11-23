// Package kit assemble un agent complet en un appel : stockage durable,
// file de tours, canaux, porte de confiance, mémoire, skills, sous-agents,
// planification, observation, ordinateur, MCP et observabilité.
//
// C'est le seul endroit qui choisit les implémentations (SQLite si une base
// est fournie, mémoire sinon) ; chaque brique reste utilisable seule. Tout ce
// qui est optionnel est désactivé par défaut, sauf la porte de confiance,
// toujours en place (une seule porte décide).
//
//	k, err := kit.New(ctx, kit.Config{
//		Provider: provider, DB: db,
//		Instructions: "Tu es l'assistant de l'atelier.",
//		Channels: []channel.Adapter{telegram.New(…)},
//		Memory:   &kit.Memory{},
//	})
//	defer k.Close()
//	err = k.Run(ctx)
package kit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/channel"
	channelsql "github.com/raucheacho/agentkit/channel/sqlite"
	"github.com/raucheacho/agentkit/computer"
	"github.com/raucheacho/agentkit/delegate"
	delegatesql "github.com/raucheacho/agentkit/delegate/sqlite"
	"github.com/raucheacho/agentkit/event"
	"github.com/raucheacho/agentkit/llm"
	"github.com/raucheacho/agentkit/mcp"
	"github.com/raucheacho/agentkit/memory"
	"github.com/raucheacho/agentkit/memory/notes"
	notesql "github.com/raucheacho/agentkit/memory/notes/sqlite"
	"github.com/raucheacho/agentkit/observe"
	observesql "github.com/raucheacho/agentkit/observe/sqlite"
	"github.com/raucheacho/agentkit/prompt"
	"github.com/raucheacho/agentkit/runner"
	"github.com/raucheacho/agentkit/schedule"
	schedulesql "github.com/raucheacho/agentkit/schedule/sqlite"
	"github.com/raucheacho/agentkit/session"
	sessionsql "github.com/raucheacho/agentkit/session/sqlite"
	"github.com/raucheacho/agentkit/skills"
	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/trust"
	trustsql "github.com/raucheacho/agentkit/trust/sqlite"
	"github.com/raucheacho/agentkit/turn"
	turnsql "github.com/raucheacho/agentkit/turn/sqlite"
)

// Config décrit l'agent à assembler.
type Config struct {
	// Provider est le modèle (obligatoire) ; Model remplace son modèle par défaut.
	Provider    llm.Provider
	Model       string
	Temperature *float64
	MaxTokens   int

	// DB conserve tout l'état (sessions, tours, validations, mémoire,
	// planifications…) ; nil : tout en mémoire, perdu à l'arrêt.
	DB *sql.DB
	// TablePrefix des tables (défaut "agentkit_").
	TablePrefix string

	// Instructions : identité et consignes, en tête du prompt système.
	Instructions string
	// ContextDir : dossier des fichiers de contexte lus à la création de
	// chaque session (ContextFiles, défaut AGENTS.md et SOUL.md).
	ContextDir   string
	ContextFiles []string
	// Prompt : sections supplémentaires du prompt système.
	Prompt []prompt.Section

	// Tools et Toolsets : les outils métier.
	Tools    []tool.Tool
	Toolsets []tool.Toolset

	// Channels : messageries ; Allow filtre les messages reçus, Session
	// choisit la clé de session d'un message (défaut canal:conversation).
	Channels []channel.Adapter
	Allow    func(m channel.Message) bool
	Session  func(m channel.Message) string
	// FailureReply est envoyé quand un message n'a pas pu être traité.
	FailureReply string

	// Trust règle la porte de confiance (toujours active).
	Trust Trust
	// Memory active la mémoire (nil : aucune).
	Memory *Memory
	// Skills active les skills (nil : aucun).
	Skills *Skills
	// Compaction active la compression de l'historique (nil : historique borné).
	Compaction *agent.Compaction
	// Delegate active les sous-agents (nil : aucun).
	Delegate *Delegate
	// Schedule active le planificateur (nil : aucun).
	Schedule *Schedule
	// Observe active l'observation d'événements (nil : aucune). Submit et
	// Seen sont fournis par le kit.
	Observe *observe.Config
	// Computer donne un ordinateur à l'agent (nil : aucun).
	Computer      computer.Workspace
	ComputerTools computer.ToolsConfig
	// MCP : serveurs dont les outils sont ajoutés.
	MCP []mcp.Config

	// Prices : tarifs pour le compteur de coûts ; TraceFile : trace JSONL.
	Prices    map[string]event.Price
	TraceFile string

	// Location : fuseau de la note de date et des planifications.
	Location *time.Location
	Logger   *slog.Logger
	// Agent a le dernier mot sur la configuration de l'agent.
	Agent func(cfg *agent.Config)
}

// Trust règle la porte de confiance.
type Trust struct {
	// Policy (défaut trust.Default : lectures et écritures libres, actions
	// sensibles validées par un humain).
	Policy trust.Policy
	// TTL d'une demande (0 : n'expire pas).
	TTL time.Duration
	// AskHuman ajoute l'outil ask_human (question à un humain).
	AskHuman bool
	// Summarize décrit une action pour la personne qui valide.
	Summarize func(call tool.Call) string
	// OnChange est appelé à chaque demande créée ou décidée (notifier le
	// responsable…), en plus du flux d'événements.
	OnChange func(ctx context.Context, r *trust.Request)
}

// Memory règle la mémoire.
type Memory struct {
	// Embedder active la recherche hybride dans les notes.
	Embedder llm.Embedder
	// Scope : propriétaire des notes d'une conversation (défaut : la
	// conversation). Une constante partage la mémoire entre conversations.
	Scope func(conv tool.Conversation) string
	// NoNotes, NoHistory : retirer les notes ou l'outil history_search.
	NoNotes, NoHistory bool
	// SharedHistory : history_search cherche dans toutes les conversations
	// (assistant personnel seulement).
	SharedHistory bool
	// Providers : fournisseurs de mémoire supplémentaires.
	Providers []memory.Provider
}

// Skills règle les skills.
type Skills struct {
	Dirs []string
	// Writable : dossier où l'agent peut écrire ses skills (skill_save) ; "" : lecture seule.
	Writable string
}

// Delegate règle les sous-agents.
type Delegate struct {
	// Provider et Model des sous-agents (défaut : ceux de l'agent).
	Provider llm.Provider
	Model    string
	MaxSteps int
	// Background active les tâches de fond.
	Background bool
}

// Schedule règle le planificateur.
type Schedule struct {
	// AgentTools donne à l'agent schedule_create, schedule_list et schedule_cancel.
	AgentTools bool
	Tick       time.Duration
}

// Kit est un agent assemblé. Ses briques restent accessibles.
type Kit struct {
	Agent     *agent.Agent
	Runner    *runner.Runner
	Gateway   *channel.Gateway
	Gate      *trust.Manager
	Team      *delegate.Delegator // nil sans Delegate
	Scheduler *schedule.Scheduler // nil sans Schedule
	Observer  *observe.Observer   // nil sans Observe
	Memory    *memory.Set         // nil sans Memory
	Skills    *skills.Library     // nil sans Skills
	Bus       *event.Bus
	Meter     *event.Meter
	Tools     *tool.Registry
	Sessions  session.Store
	Queue     turn.Queue
	Requests  trust.Store
	Jobs      schedule.Store // nil sans Schedule
	Tasks     delegate.Store // nil sans tâches de fond

	log     *slog.Logger
	closers []func() error
	runs    []func(ctx context.Context) error
}

// New assemble l'agent. Close libère ce qui a été ouvert (serveurs MCP, trace).
func New(ctx context.Context, cfg Config) (k *Kit, err error) {
	if cfg.Provider == nil {
		return nil, errors.New("kit : Config.Provider est obligatoire")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	k = &Kit{log: log, Tools: tool.NewRegistry()}
	defer func() {
		if err != nil {
			_ = k.Close()
		}
	}()
	st, err := openStores(ctx, cfg)
	if err != nil {
		return nil, err
	}
	k.Sessions, k.Queue, k.Requests = st.sessions, st.queue, st.requests

	// Observabilité.
	k.Bus = event.NewBus(event.Config{})
	k.Meter = event.NewMeter(cfg.Prices)
	k.Bus.Attach(k.Meter.Observe)
	if cfg.TraceFile != "" {
		f, err := os.OpenFile(cfg.TraceFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("kit : trace : %w", err)
		}
		k.closers = append(k.closers, f.Close)
		k.Bus.Attach(event.JSONL(f))
	}

	// Outils métier.
	if err := k.Tools.Add(cfg.Tools...); err != nil {
		return nil, err
	}
	for _, set := range cfg.Toolsets {
		if err := k.Tools.AddSet(set); err != nil {
			return nil, err
		}
	}

	// La porte : toujours là.
	onChange := k.Bus.Trust()
	if cfg.Trust.OnChange != nil {
		bus, extra := onChange, cfg.Trust.OnChange
		onChange = func(ctx context.Context, r *trust.Request) { bus(ctx, r); extra(ctx, r) }
	}
	k.Gate = trust.New(trust.Config{Store: st.requests, Tools: k.Tools, Policy: cfg.Trust.Policy, TTL: cfg.Trust.TTL,
		Summarize: cfg.Trust.Summarize, OnChange: onChange, Logger: log})
	k.runs = append(k.runs, k.Gate.Run)
	if cfg.Trust.AskHuman {
		if err := k.Tools.Add(k.Gate.AskTool()); err != nil {
			return nil, err
		}
	}

	sections := []prompt.Section{}
	if cfg.Instructions != "" {
		sections = append(sections, prompt.Text(cfg.Instructions))
	}
	if cfg.ContextDir != "" {
		files := cfg.ContextFiles
		if len(files) == 0 {
			files = []string{"AGENTS.md", "SOUL.md"}
		}
		sections = append(sections, prompt.Heading("Contexte", prompt.Files(cfg.ContextDir, 0, files...)))
	}
	sections = append(sections, cfg.Prompt...)

	// Mémoire.
	if m := cfg.Memory; m != nil {
		providers := append([]memory.Provider{}, m.Providers...)
		if !m.NoNotes {
			providers = append([]memory.Provider{notes.New(notes.Config{Store: st.notes, Embedder: m.Embedder, Scope: m.Scope, Logger: log})}, providers...)
		}
		k.Memory = memory.New(memory.Config{Logger: log}, providers...)
		if err := k.Tools.Add(k.Memory.Tools()...); err != nil {
			return nil, err
		}
		if searcher, ok := st.sessions.(session.Searcher); ok && !m.NoHistory {
			hc := memory.HistoryConfig{Searcher: searcher}
			if m.SharedHistory {
				hc.Keys = func(tool.Conversation) []string { return nil }
			}
			if err := k.Tools.Add(memory.HistoryTool(hc)); err != nil {
				return nil, err
			}
		}
		sections = append(sections, k.Memory.Section())
	}

	// Skills.
	if s := cfg.Skills; s != nil {
		dirs := append([]string{}, s.Dirs...)
		if s.Writable != "" {
			dirs = append(dirs, s.Writable)
		}
		if k.Skills, err = skills.Load(dirs...); err != nil {
			return nil, err
		}
		for _, e := range k.Skills.Errors() {
			log.Warn("kit : skill ignoré", "err", e)
		}
		if err := k.Tools.Add(k.Skills.ViewTool()); err != nil {
			return nil, err
		}
		if s.Writable != "" {
			if err := k.Tools.Add(k.Skills.SaveTool(s.Writable)); err != nil {
				return nil, err
			}
		}
		sections = append(sections, k.Skills.Section)
	}

	// Planificateur.
	if s := cfg.Schedule; s != nil {
		k.Jobs = st.jobs
		k.Scheduler = schedule.New(schedule.Config{Store: st.jobs, Location: cfg.Location, Tick: s.Tick, OnRun: k.Bus.Schedule(), Logger: log})
		k.runs = append(k.runs, k.Scheduler.Run)
		if s.AgentTools {
			if err := k.Tools.Add(k.Scheduler.Tools()...); err != nil {
				return nil, err
			}
		}
	}

	// Observation.
	if o := cfg.Observe; o != nil {
		oc := *o
		if oc.Seen == nil {
			oc.Seen = st.seen
		}
		if oc.Logger == nil {
			oc.Logger = log
		}
		k.Observer = observe.New(oc)
		k.runs = append(k.runs, k.Observer.Run)
	}

	// Ordinateur.
	if cfg.Computer != nil {
		tc := cfg.ComputerTools
		if tc.OnExec == nil {
			tc.OnExec = k.Bus.Computer()
		}
		if err := k.Tools.Add(computer.Tools(cfg.Computer, tc)...); err != nil {
			return nil, err
		}
	}

	// Serveurs MCP.
	for _, mc := range cfg.MCP {
		if mc.Logger == nil {
			mc.Logger = log
		}
		client, err := mcp.Connect(ctx, mc)
		if err != nil {
			return nil, err
		}
		k.closers = append(k.closers, client.Close)
		if err := k.Tools.AddSet(client.Toolset()); err != nil {
			return nil, err
		}
	}

	base := agent.Config{
		Provider: cfg.Provider, Model: cfg.Model, Temperature: cfg.Temperature, MaxTokens: cfg.MaxTokens,
		Tools: k.Tools, Sessions: st.sessions, Location: cfg.Location, Logger: log,
		Middleware: []agent.ToolMiddleware{k.Gate.Middleware()},
		OnEvent:    k.Bus.Agent(),
	}

	// Sous-agents : ils partagent le registre ; ce qui leur est interdit
	// (MainOnly, Sensitive) est filtré par delegate.
	if d := cfg.Delegate; d != nil {
		child := base
		child.Sessions = nil // sessions éphémères
		if d.Provider != nil {
			child.Provider = d.Provider
		}
		if d.Model != "" {
			child.Model = d.Model
		}
		child.MaxSteps = d.MaxSteps
		dc := delegate.Config{Child: child, OnFinish: k.Bus.Delegate(), Logger: log}
		if d.Background {
			dc.Store, k.Tasks = st.tasks, st.tasks
		}
		k.Team = delegate.New(dc)
		k.runs = append(k.runs, k.Team.Run)
		if err := k.Tools.Add(k.Team.Tools()...); err != nil {
			return nil, err
		}
	}

	ac := base
	ac.System = prompt.New(sections...).Build
	ac.Compaction = cfg.Compaction
	if k.Memory != nil {
		ac.Recall, ac.AfterTurn = k.Memory.Recall, k.Memory.AfterTurn
		if ac.Compaction != nil && ac.Compaction.OnCompact == nil {
			c := *ac.Compaction
			c.OnCompact = k.Memory.OnCompact
			ac.Compaction = &c
		}
	}
	if cfg.Agent != nil {
		cfg.Agent(&ac)
	}
	k.Agent = agent.New(ac)

	k.Gateway = channel.NewGateway(channel.Config{Allow: cfg.Allow, Session: cfg.Session, Holds: st.holds, Logger: log}, cfg.Channels...)
	k.Runner = runner.New(runner.Config{Agent: k.Agent, Queue: st.queue, Outbox: k.Gateway, Holds: k.Gateway,
		FailureReply: cfg.FailureReply, OnFinish: k.Bus.Runner(), Logger: log})
	k.Gateway.Attach(k.Runner, k.Agent)
	k.Gate.Attach(k.Runner)
	if k.Team != nil {
		k.Team.Attach(k.Runner)
	}
	if k.Scheduler != nil {
		k.Scheduler.Attach(k.Runner)
	}
	if k.Observer != nil {
		k.Observer.Attach(k.Runner)
	}
	k.runs = append([]func(context.Context) error{k.Runner.Run, k.Gateway.Run}, k.runs...)
	return k, nil
}

// Submit soumet un tour (message d'une source maison, tâche ponctuelle…).
func (k *Kit) Submit(ctx context.Context, t *turn.Turn) error { return k.Runner.Submit(ctx, t) }

// Run fait tourner toutes les briques jusqu'à l'arrêt de ctx ; la première
// erreur arrête les autres.
func (k *Kit) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, len(k.runs))
	for i, run := range k.runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := run(ctx); err != nil && ctx.Err() == nil {
				errs[i] = err
				cancel()
			}
		}()
	}
	wg.Wait()
	if k.Memory != nil {
		k.Memory.Wait()
	}
	return errors.Join(errs...)
}

// Close libère les serveurs MCP et la trace.
func (k *Kit) Close() error {
	var errs []error
	for i := len(k.closers) - 1; i >= 0; i-- {
		if err := k.closers[i](); err != nil {
			errs = append(errs, err)
		}
	}
	k.closers = nil
	return errors.Join(errs...)
}

type stores struct {
	sessions session.Store
	queue    turn.Queue
	requests trust.Store
	holds    channel.HoldStore
	notes    notes.Store
	tasks    delegate.Store
	jobs     schedule.Store
	seen     observe.Seen
}

func openStores(ctx context.Context, cfg Config) (*stores, error) {
	if cfg.DB == nil {
		return &stores{sessions: session.NewMemory(), queue: turn.NewMemory(), requests: trust.NewMemoryStore(),
			notes: notes.NewMemory(), tasks: delegate.NewMemoryStore(), jobs: schedule.NewMemoryStore(), seen: observe.NewMemorySeen()}, nil
	}
	db, p := cfg.DB, cfg.TablePrefix
	var s stores
	var err error
	steps := []func() error{
		func() (e error) { s.sessions, e = sessionsql.New(ctx, db, p); return },
		func() (e error) { s.queue, e = turnsql.New(ctx, db, p); return },
		func() (e error) { s.requests, e = trustsql.New(ctx, db, p); return },
		func() (e error) { s.holds, e = channelsql.NewHolds(ctx, db, p); return },
		func() (e error) { s.notes, e = notesql.New(ctx, db, p); return },
		func() (e error) { s.tasks, e = delegatesql.New(ctx, db, p); return },
		func() (e error) { s.jobs, e = schedulesql.New(ctx, db, p); return },
		func() (e error) { s.seen, e = observesql.New(ctx, db, p); return },
	}
	for _, step := range steps {
		if err = step(); err != nil {
			return nil, fmt.Errorf("kit : stockage : %w", err)
		}
	}
	return &s, nil
}

// DataDir crée (si besoin) et renvoie un dossier de données privé.
func DataDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return abs, os.MkdirAll(abs, 0o700)
}
