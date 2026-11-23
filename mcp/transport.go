package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// message est un message JSON-RPC 2.0 (requête, réponse ou notification).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError est une erreur JSON-RPC renvoyée par le serveur.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp : erreur %d : %s", e.Code, e.Message) }

// ErrClosed : la connexion au serveur est fermée.
var ErrClosed = errors.New("mcp : connexion fermée")

// transport envoie des requêtes et des notifications à un serveur.
type transport interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	close() error
	alive() bool
}

// reply construit la réponse du client à une requête du serveur : ping est
// accepté, le reste (sampling, elicitation…) n'est pas pris en charge.
func reply(req message) message {
	out := message{JSONRPC: "2.0", ID: req.ID}
	if req.Method == "ping" {
		out.Result = json.RawMessage(`{}`)
	} else {
		out.Error = &RPCError{Code: -32601, Message: "méthode non prise en charge par le client : " + req.Method}
	}
	return out
}

// --- stdio ---------------------------------------------------------------

type stdio struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	log    *slog.Logger
	nextID atomic.Int64

	wmu     sync.Mutex
	mu      sync.Mutex
	pending map[string]chan message
	done    chan struct{}
	err     error
}

func startStdio(ctx context.Context, command string, args, env []string, dir string, log *slog.Logger) (*stdio, error) {
	cmd := exec.Command(command, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp : lancement de %s : %w", command, err)
	}
	t := &stdio{cmd: cmd, stdin: stdin, log: log, pending: map[string]chan message{}, done: make(chan struct{})}
	go t.readLoop(stdout)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 4096), 1<<20)
		for sc.Scan() {
			log.Debug("mcp : stderr du serveur", "line", sc.Text())
		}
	}()
	return t, nil
}

func (t *stdio) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			t.log.Warn("mcp : message illisible", "err", err)
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0: // requête du serveur
			_ = t.write(reply(m))
		case m.Method != "": // notification : ignorée
		default:
			t.mu.Lock()
			ch := t.pending[string(m.ID)]
			delete(t.pending, string(m.ID))
			t.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	t.mu.Lock()
	t.err = err
	t.mu.Unlock()
	close(t.done)
	_ = t.cmd.Wait()
}

func (t *stdio) write(m message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	_, err = t.stdin.Write(append(data, '\n'))
	return err
}

func (t *stdio) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	id := json.RawMessage(fmt.Sprint(t.nextID.Add(1)))
	ch := make(chan message, 1)
	t.mu.Lock()
	if !t.aliveLocked() {
		t.mu.Unlock()
		return nil, ErrClosed
	}
	t.pending[string(id)] = ch
	t.mu.Unlock()
	if err := t.write(message{JSONRPC: "2.0", ID: id, Method: method, Params: raw}); err != nil {
		t.forget(id)
		return nil, fmt.Errorf("mcp : envoi : %w", err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		t.forget(id)
		_ = t.notify(context.Background(), "notifications/cancelled", map[string]any{"requestId": id, "reason": "annulé par le client"})
		return nil, ctx.Err()
	case <-t.done:
		return nil, ErrClosed
	}
}

func (t *stdio) forget(id json.RawMessage) {
	t.mu.Lock()
	delete(t.pending, string(id))
	t.mu.Unlock()
}

func (t *stdio) notify(_ context.Context, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return t.write(message{JSONRPC: "2.0", Method: method, Params: raw})
}

func (t *stdio) aliveLocked() bool {
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

func (t *stdio) alive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.aliveLocked()
}

func (t *stdio) close() error {
	_ = t.stdin.Close()
	select {
	case <-t.done:
	default:
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		<-t.done
	}
	return nil
}

// --- HTTP (Streamable HTTP) ---------------------------------------------

type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client
	nextID  atomic.Int64

	mu       sync.Mutex
	session  string
	protocol string
	closed   bool
}

func (t *httpTransport) post(ctx context.Context, m message) (*http.Response, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	t.mu.Lock()
	if t.session != "" {
		req.Header.Set("Mcp-Session-Id", t.session)
	}
	if t.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", t.protocol)
	}
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp : %w", err)
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.mu.Lock()
		t.session = sid
		t.mu.Unlock()
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("mcp : HTTP %d : %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (t *httpTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	id := json.RawMessage(fmt.Sprint(t.nextID.Add(1)))
	resp, err := t.post(ctx, message{JSONRPC: "2.0", ID: id, Method: method, Params: raw})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var m *message
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		m, err = t.readSSE(ctx, resp.Body, id)
	} else {
		var one message
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&one)
		m = &one
	}
	if err != nil {
		return nil, fmt.Errorf("mcp : réponse illisible : %w", err)
	}
	if m.Error != nil {
		return nil, m.Error
	}
	return m.Result, nil
}

// readSSE lit le flux jusqu'à la réponse id ; il répond aux requêtes du
// serveur et ignore ses notifications.
func (t *httpTransport) readSSE(ctx context.Context, r io.Reader, id json.RawMessage) (*message, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data.WriteString(strings.TrimPrefix(v, " "))
			continue
		}
		if line != "" || data.Len() == 0 {
			continue
		}
		var m message
		err := json.Unmarshal([]byte(data.String()), &m)
		data.Reset()
		if err != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			if resp, err := t.post(ctx, reply(m)); err == nil {
				resp.Body.Close()
			}
		case m.Method != "":
		case string(m.ID) == string(id):
			return &m, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("flux terminé sans réponse")
}

func (t *httpTransport) notify(ctx context.Context, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	resp, err := t.post(ctx, message{JSONRPC: "2.0", Method: method, Params: raw})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (t *httpTransport) alive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closed
}

func (t *httpTransport) close() error {
	t.mu.Lock()
	sid, closed := t.session, t.closed
	t.closed = true
	t.mu.Unlock()
	if sid == "" || closed {
		return nil
	}
	// Fin de session explicite (facultative côté serveur).
	req, err := http.NewRequest(http.MethodDelete, t.url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Mcp-Session-Id", sid)
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if resp, err := t.client.Do(req); err == nil {
		resp.Body.Close()
	}
	return nil
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return json.Marshal(params)
}
