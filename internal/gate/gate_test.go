package gate

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"fyutt/internal/config"
	"fyutt/internal/web"
)

const (
	gatePath = "0123456789abcdef0123"
	key      = "k0k1k2k3k4k5k6k7k8k9kakbkckdkekf"
	hookSec  = "s0s1s2s3s4s5s6s7s8s9"
)

type fakeBot struct{ sent []string }

func (f *fakeBot) Send(_ context.Context, chat int64, text, _ string, _ bool) (int64, error) {
	f.sent = append(f.sent, strconv.FormatInt(chat, 10)+":"+text)
	return int64(len(f.sent)), nil
}

type fakeUpdates struct{ got chan string }

func (f fakeUpdates) HandleUpdate(_ context.Context, bot string, u json.RawMessage) {
	f.got <- bot + " " + string(u)
}

func setup(t *testing.T, signed bool) (http.Handler, *fakeBot, *Gate, fakeUpdates) {
	t.Helper()
	cfg, err := config.Parse([]byte(`{
		"gate_path": "` + gatePath + `",
		"bots": {"main": {"token": "x", "webhook_secret": "` + hookSec + `"}},
		"clients": [{"id": "shop", "key": "` + key + `", "bot": "main",
			"chats": [111, -100222], "default_chat": 111, "per_minute": 3,
			"signed": ` + strconv.FormatBool(signed) + `}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	bot := &fakeBot{}
	up := fakeUpdates{got: make(chan string, 1)}
	g := New(cfg, map[string]Sender{"main": bot}, up, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &web.Router{GatePath: gatePath, Gate: g}, bot, g, up
}

func do(h http.Handler, method, target string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, body)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func sendURL(v url.Values) string { return "/" + gatePath + "/v1/send?" + v.Encode() }

func TestRootShowsLogoAndEverythingElseIs404(t *testing.T) {
	h, _, _, _ := setup(t, false)
	w := do(h, "GET", "/", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<svg") {
		t.Fatalf("root: %d", w.Code)
	}
	for _, p := range []string{"/index.html", "/robots.txt", "/" + gatePath, "/" + gatePath + "/", "/" + gatePath + "/v2/send", "/admin"} {
		if w := do(h, "GET", p, nil); w.Code != 404 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := do(h, "POST", "/", nil); w.Code != 404 {
		t.Errorf("POST /: %d", w.Code)
	}
}

func TestBadCredentialsLookLikeAMissingPage(t *testing.T) {
	h, bot, _, _ := setup(t, false)
	want := do(h, "GET", "/nothing", nil)
	for _, v := range []url.Values{
		{"client": {"shop"}, "key": {"wrong"}, "text": {"hi"}},
		{"client": {"nobody"}, "key": {key}, "text": {"hi"}},
		{"client": {"shop"}, "text": {"hi"}},
	} {
		w := do(h, "GET", sendURL(v), nil)
		if w.Code != 404 || w.Body.String() != want.Body.String() {
			t.Errorf("%v: %d %q", v, w.Code, w.Body.String())
		}
	}
	// Right key, wrong gate path.
	v := url.Values{"client": {"shop"}, "key": {key}, "text": {"hi"}}
	if w := do(h, "GET", "/wrong-gate-path-xxxx/v1/send?"+v.Encode(), nil); w.Code != 404 {
		t.Errorf("wrong gate: %d", w.Code)
	}
	if len(bot.sent) != 0 {
		t.Fatal("something was sent")
	}
}

func TestSendByQueryFormJSONAndPlainText(t *testing.T) {
	h, bot, _, _ := setup(t, false)

	if w := do(h, "GET", sendURL(url.Values{"client": {"shop"}, "key": {key}, "text": {"привет"}}), nil); w.Code != 200 {
		t.Fatalf("query: %d %s", w.Code, w.Body)
	}
	form := url.Values{"client": {"shop"}, "key": {key}, "chat": {"-100222"}, "text": {"form"}}
	if w := do(h, "POST", "/"+gatePath+"/v1/send", strings.NewReader(form.Encode()), "Content-Type", "application/x-www-form-urlencoded"); w.Code != 200 {
		t.Fatalf("form: %d %s", w.Code, w.Body)
	}
	js := `{"client":"shop","key":"` + key + `","chat":-100222,"text":"json"}`
	if w := do(h, "POST", "/"+gatePath+"/v1/send", strings.NewReader(js), "Content-Type", "application/json"); w.Code != 200 {
		t.Fatalf("json: %d %s", w.Code, w.Body)
	}

	want := []string{"111:привет", "-100222:form", "-100222:json"}
	if strings.Join(bot.sent, "|") != strings.Join(want, "|") {
		t.Fatalf("sent %v", bot.sent)
	}
}

func TestChatOutsideTheAllowListIsRefused(t *testing.T) {
	h, bot, _, _ := setup(t, false)
	w := do(h, "GET", sendURL(url.Values{"client": {"shop"}, "key": {key}, "chat": {"999"}, "text": {"x"}}), nil)
	if w.Code != 403 || len(bot.sent) != 0 {
		t.Fatalf("%d %v", w.Code, bot.sent)
	}
}

func TestRateLimit(t *testing.T) {
	h, _, g, _ := setup(t, false)
	now := time.Unix(1_800_000_000, 0)
	g.now = func() time.Time { return now }
	u := sendURL(url.Values{"client": {"shop"}, "key": {key}, "text": {"x"}})
	for i := 0; i < 3; i++ {
		if w := do(h, "GET", u, nil); w.Code != 200 {
			t.Fatalf("#%d: %d", i, w.Code)
		}
	}
	if w := do(h, "GET", u, nil); w.Code != 429 {
		t.Fatalf("4th: %d", w.Code)
	}
	now = now.Add(20 * time.Second) // one token back at 3/min
	if w := do(h, "GET", u, nil); w.Code != 200 {
		t.Fatalf("after refill: %d", w.Code)
	}
}

func TestSignedRequestsWorkOnceAndExpire(t *testing.T) {
	h, bot, g, _ := setup(t, true)
	now := time.Unix(1_800_000_000, 0)
	g.now = func() time.Time { return now }
	ts := strconv.FormatInt(now.Unix(), 10)
	v := url.Values{"client": {"shop"}, "ts": {ts}, "text": {"signed"}, "sig": {Signature(key, "shop", "", ts, "signed")}}

	if w := do(h, "GET", sendURL(v), nil); w.Code != 200 {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", sendURL(v), nil); w.Code != 404 {
		t.Fatalf("replay: %d", w.Code)
	}

	// Tampered text.
	v2 := url.Values{"client": {"shop"}, "ts": {ts}, "text": {"other"}, "sig": v["sig"]}
	if w := do(h, "GET", sendURL(v2), nil); w.Code != 404 {
		t.Fatalf("tampered: %d", w.Code)
	}
	// Plain key is not accepted for a signed client.
	if w := do(h, "GET", sendURL(url.Values{"client": {"shop"}, "key": {key}, "text": {"k"}}), nil); w.Code != 404 {
		t.Fatalf("key on signed client: %d", w.Code)
	}
	// Old timestamp.
	old := strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)
	v3 := url.Values{"client": {"shop"}, "ts": {old}, "text": {"late"}, "sig": {Signature(key, "shop", "", old, "late")}}
	if w := do(h, "GET", sendURL(v3), nil); w.Code != 404 {
		t.Fatalf("expired: %d", w.Code)
	}
	if len(bot.sent) != 1 {
		t.Fatalf("sent %v", bot.sent)
	}
}

func TestWebhookNeedsTheSecret(t *testing.T) {
	h, _, _, up := setup(t, false)
	p := "/" + gatePath + "/tg/main"
	if w := do(h, "POST", p, strings.NewReader(`{"update_id":1}`)); w.Code != 404 {
		t.Fatalf("no secret: %d", w.Code)
	}
	if w := do(h, "POST", "/"+gatePath+"/tg/other", strings.NewReader(`{}`), "X-Telegram-Bot-Api-Secret-Token", hookSec); w.Code != 404 {
		t.Fatalf("unknown bot: %d", w.Code)
	}
	if w := do(h, "POST", p, strings.NewReader(`{"update_id":1}`), "X-Telegram-Bot-Api-Secret-Token", hookSec); w.Code != 200 {
		t.Fatalf("good: %d", w.Code)
	}
	select {
	case got := <-up.got:
		if got != `main {"update_id":1}` {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("update not delivered")
	}
}
