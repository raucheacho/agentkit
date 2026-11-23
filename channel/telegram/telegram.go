// Package telegram est un canal Telegram (API Bot) écrit avec la seule
// bibliothèque standard : réception par attente longue (getUpdates), envoi
// par sendMessage.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/raucheacho/agentkit/channel"
)

// Config configure le canal.
type Config struct {
	Token string
	// AllowFrom restreint les expéditeurs (identifiants numériques ou noms
	// d'utilisateur sans @). Vide : tout le monde.
	AllowFrom []string
	// Name du canal (défaut "telegram").
	Name string
	// BaseURL de l'API (défaut https://api.telegram.org).
	BaseURL string
	// PollTimeout de l'attente longue (défaut 50 s).
	PollTimeout time.Duration
	HTTPClient  *http.Client
}

// Telegram est le canal.
type Telegram struct {
	cfg   Config
	http  *http.Client
	allow map[string]bool
}

// New crée le canal.
func New(cfg Config) *Telegram {
	if cfg.Name == "" {
		cfg.Name = "telegram"
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.telegram.org"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 50 * time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: cfg.PollTimeout + 20*time.Second}
	}
	allow := map[string]bool{}
	for _, a := range cfg.AllowFrom {
		if a = strings.TrimPrefix(strings.TrimSpace(a), "@"); a != "" {
			allow[strings.ToLower(a)] = true
		}
	}
	return &Telegram{cfg: cfg, http: hc, allow: allow}
}

func (t *Telegram) Name() string { return t.cfg.Name }

// MaxLength : 4096 caractères par message, avec une marge pour l'échappement HTML.
func (t *Telegram) MaxLength() int { return 3800 }

// APIError est une erreur renvoyée par Telegram.
type APIError struct {
	Method      string
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s : %d %s", e.Method, e.Code, e.Description)
}

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call appelle une méthode de l'API. Le jeton n'apparaît jamais dans une
// erreur : les erreurs réseau de Go contiennent l'URL, qui le contient.
func (t *Telegram) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.BaseURL+"/bot"+t.cfg.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return t.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("telegram %s : %w", method, t.redact(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("telegram %s : %w", method, err)
	}
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return fmt.Errorf("telegram %s : statut %d, réponse illisible", method, resp.StatusCode)
	}
	if !ar.OK {
		return &APIError{Method: method, Code: ar.ErrorCode, Description: ar.Description}
	}
	if out != nil {
		return json.Unmarshal(ar.Result, out)
	}
	return nil
}

func (t *Telegram) redact(err error) error {
	if t.cfg.Token == "" || !strings.Contains(err.Error(), t.cfg.Token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), t.cfg.Token, "<jeton>"))
}

type user struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
	First    string `json:"first_name"`
	Last     string `json:"last_name"`
}

type message struct {
	MessageID int64 `json:"message_id"`
	Date      int64 `json:"date"`
	From      *user `json:"from"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text    string `json:"text"`
	Caption string `json:"caption"`
	Photo   []struct {
		FileID string `json:"file_id"`
	} `json:"photo"`
	Voice *struct {
		FileID   string `json:"file_id"`
		MimeType string `json:"mime_type"`
	} `json:"voice"`
	Audio *struct {
		FileID   string `json:"file_id"`
		MimeType string `json:"mime_type"`
		FileName string `json:"file_name"`
	} `json:"audio"`
	Document *struct {
		FileID   string `json:"file_id"`
		MimeType string `json:"mime_type"`
		FileName string `json:"file_name"`
	} `json:"document"`
	Location *struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"location"`
}

type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

// Run reçoit les messages par attente longue jusqu'à l'annulation de ctx.
// Un jeton refusé (401) ou une autre instance qui lit déjà ce bot (409)
// arrêtent le canal ; les autres erreurs sont réessayées avec un délai croissant.
func (t *Telegram) Run(ctx context.Context, sink channel.Sink) error {
	if t.cfg.Token == "" {
		return errors.New("telegram : jeton manquant")
	}
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		var ups []update
		err := t.call(ctx, "getUpdates", map[string]any{
			"offset": offset, "timeout": int(t.cfg.PollTimeout.Seconds()), "allowed_updates": []string{"message"},
		}, &ups)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			var ae *APIError
			if errors.As(err, &ae) && (ae.Code == 401 || ae.Code == 409) {
				return err
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range ups {
			offset = u.UpdateID + 1
			if m, ok := t.translate(u.Message); ok {
				sink(ctx, m)
			}
		}
	}
	return nil
}

func (t *Telegram) allowed(u *user) bool {
	if len(t.allow) == 0 {
		return true
	}
	return u != nil && (t.allow[strconv.FormatInt(u.ID, 10)] || (u.Username != "" && t.allow[strings.ToLower(u.Username)]))
}

func (t *Telegram) translate(m *message) (channel.Message, bool) {
	if m == nil || m.From == nil || m.From.IsBot || !t.allowed(m.From) {
		return channel.Message{}, false
	}
	out := channel.Message{
		Channel:    t.cfg.Name,
		ChatID:     strconv.FormatInt(m.Chat.ID, 10),
		Sender:     strconv.FormatInt(m.From.ID, 10),
		SenderName: strings.TrimSpace(m.From.First + " " + m.From.Last),
		Text:       m.Text,
		ID:         strconv.FormatInt(m.Chat.ID, 10) + ":" + strconv.FormatInt(m.MessageID, 10),
		Group:      m.Chat.Type == "group" || m.Chat.Type == "supergroup",
		At:         time.Unix(m.Date, 0),
	}
	if out.Text == "" {
		out.Text = m.Caption
	}
	if n := len(m.Photo); n > 0 {
		out.Media = append(out.Media, channel.Media{Kind: "image", Ref: m.Photo[n-1].FileID})
	}
	if m.Voice != nil {
		out.Media = append(out.Media, channel.Media{Kind: "audio", Ref: m.Voice.FileID, MIME: m.Voice.MimeType, Name: "message vocal"})
	}
	if m.Audio != nil {
		out.Media = append(out.Media, channel.Media{Kind: "audio", Ref: m.Audio.FileID, MIME: m.Audio.MimeType, Name: m.Audio.FileName})
	}
	if m.Document != nil {
		out.Media = append(out.Media, channel.Media{Kind: "file", Ref: m.Document.FileID, MIME: m.Document.MimeType, Name: m.Document.FileName})
	}
	if m.Location != nil {
		out.Media = append(out.Media, channel.Media{Kind: "location", Name: fmt.Sprintf("%.6f, %.6f", m.Location.Latitude, m.Location.Longitude)})
	}
	return out, out.Text != "" || len(out.Media) > 0
}

// Send envoie un texte, mis en forme en HTML (**gras**, `code`). Si Telegram
// refuse la mise en forme, le texte brut est renvoyé.
func (t *Telegram) Send(ctx context.Context, out channel.Outgoing) error {
	chatID, err := strconv.ParseInt(out.ChatID, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram : conversation invalide %q", out.ChatID)
	}
	params := map[string]any{"chat_id": chatID, "text": FormatHTML(out.Text), "parse_mode": "HTML"}
	err = t.sendWithRetry(ctx, params)
	var ae *APIError
	if errors.As(err, &ae) && ae.Code == 400 && strings.Contains(strings.ToLower(ae.Description), "parse") {
		return t.sendWithRetry(ctx, map[string]any{"chat_id": chatID, "text": out.Text})
	}
	return err
}

// sendWithRetry réessaie une fois une erreur réseau ; un refus de Telegram
// (bot bloqué, message invalide) n'est pas réessayé.
func (t *Telegram) sendWithRetry(ctx context.Context, params map[string]any) error {
	err := t.call(ctx, "sendMessage", params, nil)
	var ae *APIError
	if err == nil || errors.As(err, &ae) || ctx.Err() != nil {
		return err
	}
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}
	return t.call(ctx, "sendMessage", params, nil)
}

// Typing affiche « en train d'écrire ».
func (t *Telegram) Typing(ctx context.Context, chatID string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return err
	}
	return t.call(ctx, "sendChatAction", map[string]any{"chat_id": id, "action": "typing"}, nil)
}

// FileURL renvoie l'URL de téléchargement d'un fichier (Media.Ref). Elle
// contient le jeton du bot : ne la montrez ni au modèle ni dans un journal.
func (t *Telegram) FileURL(ctx context.Context, fileID string) (string, error) {
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := t.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return "", err
	}
	return t.cfg.BaseURL + "/file/bot" + t.cfg.Token + "/" + f.FilePath, nil
}

var (
	boldRe = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	codeRe = regexp.MustCompile("`([^`\n]+)`")
)

// FormatHTML échappe un texte pour le mode HTML de Telegram et convertit
// **gras** et `code`.
func FormatHTML(s string) string {
	s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
	s = boldRe.ReplaceAllString(s, "<b>$1</b>")
	s = codeRe.ReplaceAllString(s, "<code>$1</code>")
	return strings.ReplaceAll(s, "**", "")
}

var (
	_ channel.Adapter = (*Telegram)(nil)
	_ channel.Typer   = (*Telegram)(nil)
	_ channel.Limiter = (*Telegram)(nil)
)
