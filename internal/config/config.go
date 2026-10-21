// Package config reads the gateway's configuration: a JSON file with the
// secrets, plus a couple of environment variables for where to listen.
//
// Everything that grants access lives in the file, not in the environment,
// because it is a list (bots, clients, chats) and a list in env vars turns
// into a parsing project of its own.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type Config struct {
	Addr string // ":8080"
	// BaseURL is the public address, https://fyutt.com, no trailing slash.
	// Only needed to register Telegram webhooks.
	BaseURL string

	// GatePath is a secret path segment in front of every working endpoint.
	// Without it every request is a 404, so the gateway cannot be found by
	// scanning, and a wrong client key looks exactly like a wrong address.
	GatePath string `json:"gate_path"`

	Bots    map[string]*Bot `json:"bots"`
	Clients []*Client       `json:"clients"`
}

// Bot is one Telegram bot the gateway can speak as.
type Bot struct {
	Token string `json:"token"`
	// WebhookSecret is what Telegram must echo in
	// X-Telegram-Bot-Api-Secret-Token. Required for a webhook to be accepted.
	WebhookSecret string `json:"webhook_secret"`
}

// Client is one caller allowed to send through the gateway.
type Client struct {
	ID  string `json:"id"`
	Key string `json:"key"`
	Bot string `json:"bot"`
	// Chats is the allow-list. A client may send only here, whatever chat
	// it names in the URL.
	Chats []int64 `json:"chats"`
	// DefaultChat is used when the request names none. Must be in Chats.
	DefaultChat int64 `json:"default_chat"`
	// Signed clients must also send ts and sig (HMAC-SHA256 over the
	// request, keyed with Key) and the key itself is never put in the URL.
	// Use it when the URL may end up somewhere it can be read back, such as
	// a proxy log or a browser history.
	Signed bool `json:"signed"`
	// PerMinute caps sends; 0 means the default of 30.
	PerMinute int `json:"per_minute"`
	// ParseMode is passed to Telegram as is: "", "HTML" or "MarkdownV2".
	ParseMode string `json:"parse_mode"`
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func Load() (*Config, error) {
	path := env("CONFIG_FILE", "/app/config/fyutt.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	cfg.Addr = env("ADDR", ":8080")
	cfg.BaseURL = strings.TrimRight(env("BASE_URL", ""), "/")
	return cfg, nil
}

// Parse reads and checks the file. Every mistake is reported at start-up, so
// a broken config stops the container instead of quietly refusing requests.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var problems []string
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	if len(cfg.GatePath) < 16 || strings.ContainsAny(cfg.GatePath, "/?#% ") {
		add("gate_path must be at least 16 characters, letters and digits only (openssl rand -hex 16)")
	}
	for name, b := range cfg.Bots {
		if !idRe.MatchString(name) {
			add("bot name %q: use lower-case letters, digits, - and _", name)
		}
		if b == nil || b.Token == "" {
			add("bot %q has no token", name)
		}
	}
	seen := map[string]bool{}
	for i, c := range cfg.Clients {
		if c == nil {
			add("client #%d is empty", i+1)
			continue
		}
		if !idRe.MatchString(c.ID) {
			add("client #%d: id %q: use lower-case letters, digits, - and _", i+1, c.ID)
		}
		if seen[c.ID] {
			add("client %q is listed twice", c.ID)
		}
		seen[c.ID] = true
		if len(c.Key) < 32 {
			add("client %q: key must be at least 32 characters (openssl rand -hex 32)", c.ID)
		}
		if cfg.Bots[c.Bot] == nil {
			add("client %q: unknown bot %q", c.ID, c.Bot)
		}
		if len(c.Chats) == 0 {
			add("client %q: chats is empty, it could send nowhere", c.ID)
		}
		if c.DefaultChat != 0 && !c.Allows(c.DefaultChat) {
			add("client %q: default_chat is not in chats", c.ID)
		}
		switch c.ParseMode {
		case "", "HTML", "MarkdownV2":
		default:
			add("client %q: parse_mode must be empty, HTML or MarkdownV2", c.ID)
		}
		if c.PerMinute <= 0 {
			c.PerMinute = 30
		}
	}
	if len(problems) > 0 {
		return nil, errors.New("config:\n  " + strings.Join(problems, "\n  "))
	}
	return &cfg, nil
}

func (c *Client) Allows(chat int64) bool {
	for _, x := range c.Chats {
		if x == chat {
			return true
		}
	}
	return false
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
