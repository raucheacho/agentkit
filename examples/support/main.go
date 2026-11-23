// Commande support : un agent d'entreprise qui répond à de nombreux clients,
// à la manière de persibot. Chaque client a sa mémoire ; l'agent consulte le
// catalogue et suit les commandes ; une commande passe par la validation du
// responsable ; l'agent peut poser une question au responsable ; un membre
// de l'équipe peut reprendre une conversation à la main. Pas d'ordinateur,
// pas de planification par l'agent : seulement des outils métier.
//
//	AGENTKIT_MODEL=llama3.1:8b go run ./examples/support
//	WHATSAPP_URL=… WHATSAPP_KEY=… WHATSAPP_SECRET=…   # facultatif : WhatsApp
//
// Web : POST /web/messages {"chat_id":"client1","text":"…"}, GET /web/events?chat_id=client1.
// Responsable : GET /admin/requests, POST /admin/requests/{id}/approve… (ADMIN_TOKEN).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/agent"
	"github.com/raucheacho/agentkit/channel"
	"github.com/raucheacho/agentkit/channel/web"
	"github.com/raucheacho/agentkit/channel/whatsapp"
	"github.com/raucheacho/agentkit/examples/internal/setup"
	"github.com/raucheacho/agentkit/kit"
	"github.com/raucheacho/agentkit/session"
	"github.com/raucheacho/agentkit/tool"
	"github.com/raucheacho/agentkit/trust"
)

func main() { setup.Main(run) }

func run(ctx context.Context) error {
	dir := setup.Env("AGENTKIT_DIR", filepath.Join(os.TempDir(), "agentkit-support"))
	db, err := setup.Open(dir)
	if err != nil {
		return err
	}
	defer db.Close()
	provider, _ := setup.Model()
	shop := newShop()

	webCh := web.New(web.Config{})
	channels := []channel.Adapter{webCh}
	var wa *whatsapp.WhatsApp
	if u := os.Getenv("WHATSAPP_URL"); u != "" {
		wa = whatsapp.New(whatsapp.Config{BaseURL: u, APIKey: os.Getenv("WHATSAPP_KEY"), WebhookSecret: os.Getenv("WHATSAPP_SECRET")})
		channels = append(channels, wa)
	}

	k, err := kit.New(ctx, kit.Config{
		Provider: provider, DB: db,
		Instructions: "Tu es le conseiller clientèle de la boutique Atelier Lune (bougies et céramiques artisanales). " +
			"Réponds poliment et brièvement, en vouvoyant. Utilise le catalogue pour les produits et les prix : " +
			"n'invente jamais un produit, un prix ou un délai. Pour commander, utilise create_order ; la commande " +
			"sera validée par le responsable. Si tu ne sais pas répondre, pose la question au responsable (ask_human).",
		ContextDir:   dir,
		Channels:     channels,
		Tools:        shop.tools(),
		FailureReply: "Désolé, je rencontre un souci technique. Un conseiller vous répondra rapidement.",
		Trust: kit.Trust{
			AskHuman: true,
			TTL:      48 * time.Hour,
			// Une action sensible n'est jamais lancée sans quelqu'un pour la demander.
			Policy: trust.Rules(trust.Default(),
				trust.Rule{Effects: []tool.Effect{tool.Sensitive}, Sources: []string{"schedule", "event", "task"},
					Decision: trust.Deny, Reason: "action sensible hors d'une demande client"}),
			Summarize: func(c tool.Call) string { return "Commande client : " + string(c.Raw) },
			OnChange: func(_ context.Context, r *trust.Request) {
				if r.Status == trust.Pending {
					// Ici : notifier le responsable (e-mail, Telegram…).
					slog.Info("à valider", "id", r.ID, "kind", r.Kind, "client", r.Conversation.ChatID, "summary", r.Summary)
				}
			},
		},
		// Mémoire par client (défaut) ; la recherche dans l'historique reste
		// limitée à la conversation du client.
		Memory:     &kit.Memory{},
		Compaction: &agent.Compaction{},
		TraceFile:  filepath.Join(dir, "trace.jsonl"),
		Agent:      func(c *agent.Config) { c.ParseInlineTools = true },
	})
	if err != nil {
		return err
	}
	defer k.Close()

	mux := http.NewServeMux()
	mux.Handle("/web/", http.StripPrefix("/web", webCh.Handler()))
	if wa != nil {
		mux.Handle("POST /whatsapp/webhook", wa.Handler())
	}
	mux.Handle("/admin/", http.StripPrefix("/admin", k.Admin(setup.AdminToken())))
	return setup.Serve(ctx, setup.Env("AGENTKIT_ADDR", ":8080"), mux, k.Run)
}

// --- la boutique (données de démonstration) ---------------------------

type product struct {
	Ref   string
	Name  string
	Price float64
	Stock int
}

type shop struct {
	catalog []product
	orders  map[string]string
}

func newShop() *shop {
	return &shop{
		catalog: []product{
			{"B-01", "Bougie Lune (cire de soja, 200 g, figue)", 24, 12},
			{"B-02", "Bougie Marée (cire de soja, 200 g, iode)", 24, 0},
			{"C-10", "Bol en grès émaillé bleu nuit", 32, 5},
			{"C-11", "Tasse en grès blanc cassé", 18, 20},
		},
		orders: map[string]string{"A-1042": "expédiée le 20 novembre, livraison prévue sous 48 h"},
	}
}

type searchIn struct {
	Query string `json:"query" desc:"produit recherché (mot-clé)"`
}

type orderStatusIn struct {
	Order string `json:"order" desc:"numéro de commande, par exemple A-1042"`
}

type orderIn struct {
	Ref      string `json:"ref" desc:"référence du produit"`
	Quantity int    `json:"quantity" desc:"quantité"`
	Address  string `json:"address" desc:"adresse de livraison complète"`
}

func (s *shop) tools() []tool.Tool {
	return []tool.Tool{
		tool.New("catalog_search", "Cherche dans le catalogue (produits, prix, stock).",
			func(_ context.Context, in searchIn) (tool.Result, error) {
				out := ""
				for _, p := range s.catalog {
					if in.Query == "" || containsFold(p.Name+" "+p.Ref, in.Query) {
						out += fmt.Sprintf("- %s %s : %.2f €, %s\n", p.Ref, p.Name, p.Price, stock(p.Stock))
					}
				}
				if out == "" {
					return tool.Text("Aucun produit ne correspond. Catalogue complet : catalog_search sans mot-clé."), nil
				}
				return tool.Text(out), nil
			}),
		tool.New("order_status", "Donne l'état d'une commande.",
			func(_ context.Context, in orderStatusIn) (tool.Result, error) {
				if st, ok := s.orders[in.Order]; ok {
					return tool.Text("Commande " + in.Order + " : " + st + "."), nil
				}
				return tool.Text("Commande " + in.Order + " introuvable : demandez au client de vérifier le numéro."), nil
			}),
		// Sensible : la porte demande la validation du responsable, puis
		// exécute la commande telle qu'elle a été validée, une seule fois.
		tool.New("create_order", "Passe une commande pour le client (validée ensuite par le responsable).",
			func(ctx context.Context, in orderIn) (tool.Result, error) {
				for _, p := range s.catalog {
					if p.Ref == in.Ref {
						if p.Stock < in.Quantity {
							return tool.Text("Stock insuffisant pour " + p.Name + "."), nil
						}
						c, _ := tool.ConversationFrom(ctx)
						id := fmt.Sprintf("A-%d", 1100+len(s.orders))
						s.orders[id] = "validée, en préparation"
						return tool.Text(fmt.Sprintf("Commande %s créée pour %s : %d × %s, total %.2f €, livraison à %s.",
							id, c.ChatID, in.Quantity, p.Name, float64(in.Quantity)*p.Price, in.Address)), nil
					}
				}
				return tool.Text("Référence " + in.Ref + " inconnue."), nil
			}, tool.WithEffect(tool.Sensitive)),
	}
}

func stock(n int) string {
	if n == 0 {
		return "en rupture"
	}
	return fmt.Sprintf("%d en stock", n)
}

func containsFold(s, sub string) bool {
	return strings.Contains(session.Fold(s), session.Fold(sub))
}
