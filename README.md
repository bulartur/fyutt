# Fyutt

A closed gateway. To the outside world it's one page with the logo on a white
background, and every other address returns `404`. Behind a secret path sits
an endpoint that takes text and relays it to Telegram. It accepts only calls
that bring the right key, and only to the chats allowed for that key.

Go, standard library only, no database. One container next to qr-memorial on
srv608216. Deploy: [`docs/DEPLOY.md`](docs/DEPLOY.md).

```
cmd/fyutt           entry point and service commands
internal/web        the logo page and the shared 404
internal/gate       send and webhooks: the closed part
internal/telegram   Bot API client
internal/config     reading and validating fyutt.json
deploy              compose, nginx block, example config
```

## How it works

| Request | Response |
|---|---|
| `GET /` | the logo page |
| anything else | `404 Not Found` |
| `/<gate_path>/v1/send` with the right key | text sent to Telegram, `200 {"ok":true,...}` |
| same, wrong key or unknown client | the same `404`, byte for byte |
| `POST /<gate_path>/tg/<bot>` from Telegram with the secret | `200`, the update goes to the handler |

A wrong key looks the same as a page that doesn't exist, so a scanner can't
tell the gateway exists at all. Real errors (`400/403/429/502`) are shown only
to a caller that passed the check.

## Configuration: `fyutt.json`

Example: [`deploy/fyutt.example.json`](deploy/fyutt.example.json). The
application checks it at startup, and a single mistake stops the container
with a list of everything wrong.

- `gate_path`: secret path segment (`openssl rand -hex 16`).
- `bots`: name → `token` from BotFather, and `webhook_secret` (empty means
  this bot takes no webhooks).
- `clients`: who is allowed to send:
  - `id`, `key` (`openssl rand -hex 32`), `bot`: which bot sends;
  - `chats`: allowed chat IDs; you can't send anywhere else;
  - `default_chat`: where to send if the request names no chat;
  - `signed`: signature mode instead of a key in the URL (below);
  - `per_minute`: send limit, 30 by default;
  - `parse_mode`: `""`, `"HTML"` or `"MarkdownV2"`.

Add a client, bot or chat by editing the file and running
`docker compose restart`.

## Sending

```
https://fyutt.com/<gate_path>/v1/send?client=<id>&key=<key>&chat=<chat_id>&text=<text>
```

| Parameter | |
|---|---|
| `client` | client id |
| `key` | its key (for `signed: false`) |
| `ts`, `sig` | time and signature (for `signed: true`) |
| `chat` | chat ID; optional if `default_chat` is set |
| `text` | up to 4096 characters, URL-encoded |
| `silent=1` | deliver without a sound |

Works as `GET`, and as `POST` with a form, JSON or a bare `text/plain` body
(then the parameters are in the URL and the text is in the body):

```bash
curl -X POST 'https://fyutt.com/<gate>/v1/send?client=shop&key=…' \
     -H 'Content-Type: text/plain' --data-binary @message.txt
```

Response: `{"ok":true,"chat":123,"message_id":45}`, or
`{"ok":false,"error":"…"}` with a code:
`400` malformed request, `403` chat not allowed, `413` text too long,
`429` limit hit (see `Retry-After`), `502` Telegram refused or is unreachable.

Ready-made URL for testing:

```bash
sudo docker compose exec app /app/fyutt url shop 'Hello'
```

### Signed mode

The key in the URL is convenient, but a URL leaks: browser history, proxy
logs, someone else's screenshot. For a client with `signed: true` the key
never goes into the URL. Instead it gets:

- `ts`: current Unix time in seconds;
- `sig`: hex of `HMAC-SHA256(key, client + "\n" + chat + "\n" + ts + "\n" + text)`,
  where `chat` is exactly as passed (an empty string if not passed).

A signature is valid for ±5 minutes and only once. Even an intercepted URL
can't be replayed or have its text swapped.

## Webhooks

The groundwork is in place. To make a bot accept updates:

1. Set `webhook_secret` in `fyutt.json` (`openssl rand -hex 32`) and restart.
2. `sudo docker compose exec app /app/fyutt webhook set main`
3. Check: `… /app/fyutt webhook info main`.

Telegram will then call `https://fyutt.com/<gate_path>/tg/main` with the
secret in a header. Anything without the secret gets a `404`. For now an
update is only written to the log (type and id, not the content): the
`gate.LogUpdates` handler. New features go in their own implementation of
`gate.UpdateHandler` in `cmd/fyutt/main.go`. It gets the raw update and
decodes only the part it needs.

## Growing it further

- New endpoint: a `case` in `Gate.ServeHTTP`; it's automatically behind
  `gate_path`.
- New public page: `web.Router` (by default everything except `/` is 404).
- If you ever need state (message history, a queue, retries), that's when a
  database gets added, the way qr-memorial has one.

## Service commands

```bash
fyutt check                 # config + getMe for every bot
fyutt webhook set|delete|info <bot>
fyutt url <client> <text>   # print a ready-made send URL
```

## Local run

```bash
CONFIG_FILE=./fyutt.json ADDR=:8080 go run ./cmd/fyutt
go test ./...
```

## Privacy and logs

- nginx keeps no access log for this site: the key travels in the query string.
- The application logs only the client id, chat, message id and length,
  never the text or the key. Requests that fail the check aren't logged at all.
