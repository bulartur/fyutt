// Package gate is the closed part of the gateway: relaying a caller's text to
// a Telegram chat, and receiving Telegram's webhooks.
//
// Routes, relative to the secret gate path:
//
//	GET|POST /v1/send   relay text to a chat, see Send
//	POST     /tg/{bot}  Telegram webhook for one bot, see Webhook
//
// Anything that fails authentication gets the site's ordinary 404.
// Only a caller who has already proven its key sees real error messages.
package gate

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"fyutt/internal/config"
	"fyutt/internal/telegram"
	"fyutt/internal/web"
)

// MaxText is Telegram's limit for one message, in characters.
const MaxText = 4096

// SignatureWindow is how far a signed request's ts may be from our clock.
const SignatureWindow = 5 * time.Minute

// Sender is what the gate needs from a bot; telegram.Bot satisfies it, and a
// test can substitute a fake.
type Sender interface {
	Send(ctx context.Context, chat int64, text, parseMode string, silent bool) (int64, error)
}

// UpdateHandler receives every webhook update that passed the secret check.
// The update is passed raw: each future feature decodes the part it needs.
type UpdateHandler interface {
	HandleUpdate(ctx context.Context, bot string, update json.RawMessage)
}

type Gate struct {
	cfg     *config.Config
	bots    map[string]Sender
	clients map[string]*config.Client
	updates UpdateHandler
	log     *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	seen    map[string]time.Time // signatures already used, for replay
}

func New(cfg *config.Config, bots map[string]Sender, updates UpdateHandler, log *slog.Logger) *Gate {
	g := &Gate{
		cfg:     cfg,
		bots:    bots,
		clients: map[string]*config.Client{},
		updates: updates,
		log:     log,
		now:     time.Now,
		buckets: map[string]*bucket{},
		seen:    map[string]time.Time{},
	}
	for _, c := range cfg.Clients {
		g.clients[c.ID] = c
	}
	return g
}

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/send":
		g.Send(w, r)
	case strings.HasPrefix(r.URL.Path, "/tg/"):
		g.Webhook(w, r, strings.TrimPrefix(r.URL.Path, "/tg/"))
	default:
		web.NotFound(w)
	}
}

// --------------------------------------------------------------- sending ---

type sendRequest struct {
	Client string
	Key    string
	Chat   string
	Text   string
	TS     string
	Sig    string
	Silent bool
}

// Send relays one message. Parameters may come in the query string, in a
// form body, or in a JSON body; the query string wins where both are given.
//
//	client  client id from the config                      required
//	key     the client's key                               unsigned clients
//	ts,sig  unix seconds and HMAC, see Signature           signed clients
//	chat    target chat id; must be in the client's list   optional with default_chat
//	text    the message, up to 4096 characters             required
//	silent  1 to deliver without a notification sound      optional
//
// A text/plain body is taken as the text.
func (g *Gate) Send(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		web.NotFound(w)
		return
	}
	req, err := parseSend(r)
	if err != nil {
		web.NotFound(w)
		return
	}

	c := g.authenticate(req)
	if c == nil {
		// Not even the client id is logged: it came from a stranger.
		web.NotFound(w)
		return
	}
	log := g.log.With("client", c.ID)

	chat := c.DefaultChat
	if req.Chat != "" {
		n, err := strconv.ParseInt(req.Chat, 10, 64)
		if err != nil {
			reply(w, http.StatusBadRequest, "chat must be a number")
			return
		}
		chat = n
	}
	switch {
	case chat == 0:
		reply(w, http.StatusBadRequest, "chat is required: this client has no default_chat")
		return
	case !c.Allows(chat):
		log.Warn("chat not allowed", "chat", chat)
		reply(w, http.StatusForbidden, "this client may not send to that chat")
		return
	case strings.TrimSpace(req.Text) == "":
		reply(w, http.StatusBadRequest, "text is required")
		return
	case !utf8.ValidString(req.Text):
		reply(w, http.StatusBadRequest, "text must be UTF-8")
		return
	case utf8.RuneCountInString(req.Text) > MaxText:
		reply(w, http.StatusRequestEntityTooLarge, "text is longer than 4096 characters")
		return
	}
	if !g.allow(c) {
		log.Warn("rate limited")
		w.Header().Set("Retry-After", "60")
		reply(w, http.StatusTooManyRequests, "too many messages, slow down")
		return
	}

	id, err := g.bots[c.Bot].Send(r.Context(), chat, req.Text, c.ParseMode, req.Silent)
	if err != nil {
		var te *telegram.Error
		if errors.As(err, &te) {
			log.Warn("telegram refused", "chat", chat, "code", te.Code, "desc", te.Description)
			if te.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(te.RetryAfter))
			}
			reply(w, http.StatusBadGateway, "telegram: "+te.Description)
			return
		}
		log.Error("send failed", "chat", chat, "err", err)
		reply(w, http.StatusBadGateway, "telegram is unreachable, try again")
		return
	}
	log.Info("sent", "chat", chat, "message_id", id, "chars", utf8.RuneCountInString(req.Text))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "chat": chat, "message_id": id})
}

func parseSend(r *http.Request) (*sendRequest, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 64<<10)
	q := r.URL.Query()
	req := &sendRequest{}

	if r.Method == http.MethodPost {
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		switch ct {
		case "application/json":
			var body struct {
				Client string          `json:"client"`
				Key    string          `json:"key"`
				Chat   json.RawMessage `json:"chat"`
				Text   string          `json:"text"`
				TS     json.RawMessage `json:"ts"`
				Sig    string          `json:"sig"`
				Silent bool            `json:"silent"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, err
			}
			req.Client, req.Key, req.Text, req.Sig, req.Silent = body.Client, body.Key, body.Text, body.Sig, body.Silent
			req.Chat = strings.Trim(string(body.Chat), `"`)
			req.TS = strings.Trim(string(body.TS), `"`)
		case "text/plain":
			b, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			req.Text = string(b)
		case "application/x-www-form-urlencoded", "multipart/form-data":
			if err := r.ParseMultipartForm(64 << 10); err != nil && !errors.Is(err, http.ErrNotMultipart) {
				return nil, err
			}
			for k, dst := range map[string]*string{"client": &req.Client, "key": &req.Key, "chat": &req.Chat, "text": &req.Text, "ts": &req.TS, "sig": &req.Sig} {
				*dst = r.PostForm.Get(k)
			}
			req.Silent = r.PostForm.Get("silent") == "1"
		}
	}
	for k, dst := range map[string]*string{"client": &req.Client, "key": &req.Key, "chat": &req.Chat, "text": &req.Text, "ts": &req.TS, "sig": &req.Sig} {
		if v := q.Get(k); v != "" {
			*dst = v
		}
	}
	if q.Get("silent") == "1" {
		req.Silent = true
	}
	return req, nil
}

// Signature is what a signed client puts in sig: lower-case hex of
// HMAC-SHA256(key, client + "\n" + chat + "\n" + ts + "\n" + text), where
// chat is exactly as sent (empty when relying on default_chat).
func Signature(key, client, chat, ts, text string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(client + "\n" + chat + "\n" + ts + "\n" + text))
	return hex.EncodeToString(m.Sum(nil))
}

func (g *Gate) authenticate(req *sendRequest) *config.Client {
	c := g.clients[req.Client]
	if c == nil {
		return nil
	}
	if !c.Signed {
		if subtle.ConstantTimeCompare([]byte(req.Key), []byte(c.Key)) != 1 {
			return nil
		}
		return c
	}

	// A signed client never sends its key; one that does has leaked it.
	if req.Key != "" {
		g.log.Warn("signed client sent its key in the clear; rotate it", "client", c.ID)
		return nil
	}
	ts, err := strconv.ParseInt(req.TS, 10, 64)
	if err != nil {
		return nil
	}
	now := g.now()
	if d := now.Sub(time.Unix(ts, 0)); d > SignatureWindow || d < -SignatureWindow {
		return nil
	}
	want := Signature(c.Key, c.ID, req.Chat, req.TS, req.Text)
	if !hmac.Equal([]byte(strings.ToLower(req.Sig)), []byte(want)) {
		return nil
	}

	// The same signed URL works only once.
	g.mu.Lock()
	defer g.mu.Unlock()
	for s, t := range g.seen {
		if now.Sub(t) > 2*SignatureWindow {
			delete(g.seen, s)
		}
	}
	if _, dup := g.seen[want]; dup {
		return nil
	}
	g.seen[want] = now
	return c
}

// A token bucket per client: PerMinute tokens, refilled continuously.
type bucket struct {
	tokens float64
	at     time.Time
}

func (g *Gate) allow(c *config.Client) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now, max := g.now(), float64(c.PerMinute)
	b := g.buckets[c.ID]
	if b == nil {
		b = &bucket{tokens: max, at: now}
		g.buckets[c.ID] = b
	}
	b.tokens = min(max, b.tokens+now.Sub(b.at).Minutes()*max)
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func reply(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

// -------------------------------------------------------------- webhooks ---

// WebhookPath is the path, under the gate, that Telegram is told to call.
func WebhookPath(bot string) string { return "/tg/" + bot }

// Webhook accepts an update for one bot. Telegram proves itself with the
// secret token it was given at setWebhook; without a match it is a 404 like
// anything else. A bot with no webhook_secret takes no webhooks at all.
func (g *Gate) Webhook(w http.ResponseWriter, r *http.Request, name string) {
	bot := g.cfg.Bots[name]
	if r.Method != http.MethodPost || bot == nil || bot.WebhookSecret == "" ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(bot.WebhookSecret)) != 1 {
		web.NotFound(w)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || !json.Valid(raw) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Answer Telegram at once and handle the update afterwards: a slow
	// handler would otherwise make Telegram retry and deliver it twice.
	w.WriteHeader(http.StatusOK)
	go g.updates.HandleUpdate(context.WithoutCancel(r.Context()), name, raw)
}

// LogUpdates is the handler until a bot has real work to do: it records that
// an update arrived and what kind it was, never its content.
type LogUpdates struct{ Log *slog.Logger }

func (l LogUpdates) HandleUpdate(_ context.Context, bot string, update json.RawMessage) {
	var u map[string]json.RawMessage
	_ = json.Unmarshal(update, &u)
	kind := "unknown"
	for k := range u {
		if k != "update_id" {
			kind = k
		}
	}
	l.Log.Info("webhook update", "bot", bot, "kind", kind, "update_id", string(u["update_id"]))
}
