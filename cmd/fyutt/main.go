// Command fyutt runs the gateway, and a few chores around it.
//
//	fyutt                          serve
//	fyutt check                    validate the config and every bot token
//	fyutt webhook set <bot>        point Telegram at this gateway
//	fyutt webhook delete <bot>
//	fyutt webhook info <bot>
//	fyutt url <client> <text>      print a ready-to-use send URL
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"fyutt/internal/config"
	"fyutt/internal/gate"
	"fyutt/internal/telegram"
	"fyutt/internal/web"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch {
	case args[0] == "serve":
		err = serve(cfg, log)
	case args[0] == "check":
		err = check(cfg)
	case args[0] == "webhook" && len(args) == 3:
		err = webhook(cfg, args[1], args[2])
	case args[0] == "url" && len(args) == 3:
		err = sendURL(cfg, args[1], args[2])
	default:
		err = errors.New("usage: fyutt [serve | check | webhook set|delete|info <bot> | url <client> <text>]")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serve(cfg *config.Config, log *slog.Logger) error {
	bots := map[string]gate.Sender{}
	for name, b := range cfg.Bots {
		bots[name] = telegram.New(b.Token)
	}
	g := gate.New(cfg, bots, gate.LogUpdates{Log: log}, log)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           &web.Router{GatePath: cfg.GatePath, Gate: g},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		// No request is ever logged by the server itself: a send URL holds
		// a client key in its query string.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("listening", "addr", cfg.Addr, "bots", len(cfg.Bots), "clients", len(cfg.Clients))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func check(cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	failed := false
	for name, b := range cfg.Bots {
		user, err := telegram.New(b.Token).Me(ctx)
		if err != nil {
			fmt.Printf("bot %-12s FAIL  %v\n", name, err)
			failed = true
			continue
		}
		fmt.Printf("bot %-12s ok    @%s\n", name, user)
	}
	fmt.Printf("config ok: %d bot(s), %d client(s)\n", len(cfg.Bots), len(cfg.Clients))
	if failed {
		return errors.New("some bot tokens do not work")
	}
	return nil
}

func webhook(cfg *config.Config, action, name string) error {
	b := cfg.Bots[name]
	if b == nil {
		return fmt.Errorf("no bot %q in the config", name)
	}
	bot := telegram.New(b.Token)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	switch action {
	case "set":
		if cfg.BaseURL == "" {
			return errors.New("BASE_URL is not set")
		}
		if len(b.WebhookSecret) < 16 {
			return fmt.Errorf("bot %q needs a webhook_secret of at least 16 characters (openssl rand -hex 32)", name)
		}
		if err := bot.SetWebhook(ctx, cfg.BaseURL+"/"+cfg.GatePath+gate.WebhookPath(name), b.WebhookSecret, nil); err != nil {
			return err
		}
		fmt.Println("webhook set")
		return nil
	case "delete":
		if err := bot.DeleteWebhook(ctx); err != nil {
			return err
		}
		fmt.Println("webhook deleted")
		return nil
	case "info":
		info, err := bot.WebhookInfo(ctx)
		if err != nil {
			return err
		}
		fmt.Println(string(info))
		return nil
	}
	return fmt.Errorf("unknown webhook action %q", action)
}

// sendURL prints the URL a client would call, so it can be pasted into
// whatever is going to send. For a signed client the URL is good for one use
// within five minutes, which is what makes it useful only as a test.
func sendURL(cfg *config.Config, id, text string) error {
	var c *config.Client
	for _, x := range cfg.Clients {
		if x.ID == id {
			c = x
		}
	}
	if c == nil {
		return fmt.Errorf("no client %q in the config", id)
	}
	base := cfg.BaseURL
	if base == "" {
		base = "http://localhost" + cfg.Addr
	}
	q := url.Values{"client": {c.ID}, "text": {text}}
	if c.Signed {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		q.Set("ts", ts)
		q.Set("sig", gate.Signature(c.Key, c.ID, "", ts, text))
	} else {
		q.Set("key", c.Key)
	}
	fmt.Println(base + "/" + cfg.GatePath + "/v1/send?" + q.Encode())
	return nil
}
