# Deploying fyutt.com on srv608216

The fourth stack, next to `/opt/palker`, `/opt/krutt_budget` and
`/opt/qr_memorial`. The same pattern: no port faces the outside, and the
`docker_palker-network` network is attached as an external network.

| What | Where |
|---|---|
| Project | `/opt/fyutt` |
| Compose file | `/opt/fyutt/deploy/docker-compose.yml` |
| Config with secrets | `/opt/fyutt/deploy/fyutt.json` (not in git) |
| nginx block | `/opt/palker/docker/nginx/conf.d/fyutt.com.conf` |
| Certificate | `/etc/letsencrypt/live/fyutt.com/` |

Container: `fyutt-app`. No volumes or database.

> If the domain isn't `fyutt.com`, replace it in
> `deploy/nginx/fyutt.com.conf` and in `BASE_URL` (compose).

## 0. DNS

```
A    fyutt.com       191.101.81.144
A    www.fyutt.com   191.101.81.144
```

Wait until `dig +short fyutt.com` returns that address.

## 1. Code

```bash
sudo mkdir -p /opt/fyutt && sudo chown jordan:jordan /opt/fyutt
git clone <repository-url> /opt/fyutt
```

## 2. Config

```bash
cd /opt/fyutt/deploy
cp fyutt.example.json fyutt.json
openssl rand -hex 16     # → gate_path
openssl rand -hex 32     # → key for each client
nano fyutt.json
# The container runs as uid 10001; only it and root may read the file.
sudo chown 10001 fyutt.json && sudo chmod 400 fyutt.json
```

Find a chat ID: send the bot any message and open
`https://api.telegram.org/bot<token>/getUpdates`; the ID is in
`message.chat.id`. A group's ID starts with `-100`. Add the bot to the group
first. Do this **before** you set up a webhook: once a webhook exists,
`getUpdates` stops working.

To edit the file later: `sudo nano fyutt.json`, then
`sudo docker compose restart`.

## 3. Start

```bash
cd /opt/fyutt/deploy
sudo docker compose up -d --build
sudo docker compose logs app --tail=20       # expect "listening"
sudo docker compose exec app /app/fyutt check   # every bot ok
```

## 4. Certificate

First a temporary block just for the check, then certbot:

```bash
sudo tee /opt/palker/docker/nginx/conf.d/fyutt.com.conf >/dev/null <<'EOF'
server {
    listen 80;
    server_name fyutt.com www.fyutt.com;
    location /.well-known/acme-challenge/ { root /var/www/certbot; }
    location / { return 404; }
}
EOF
sudo docker exec palker-nginx nginx -t && sudo docker exec palker-nginx nginx -s reload

sudo certbot certonly --webroot -w /opt/palker/docker/certbot \
  -d fyutt.com -d www.fyutt.com
```

## 5. The real nginx block

```bash
sudo cp /opt/fyutt/deploy/nginx/fyutt.com.conf /opt/palker/docker/nginx/conf.d/fyutt.com.conf
sudo docker exec palker-nginx nginx -t        # ALWAYS first
sudo docker exec palker-nginx nginx -s reload
```

If `nginx -t` complains, don't reload: the neighboring sites stay on the old config.

## 6. Check

```bash
curl -sI https://fyutt.com/ | head -1              # 200
curl -sI https://fyutt.com/anything | head -1      # 404
curl -sI https://www.fyutt.com/ | head -1          # 301

# a real send
URL=$(sudo docker compose exec -T app /app/fyutt url <client> 'Fyutt is up')
curl -s "$URL"                                     # {"ok":true,...}
```

## Update

```bash
cd /opt/fyutt && git pull
cd deploy && sudo docker compose up -d --build
```

## Rollback

```bash
cd /opt/fyutt && git checkout <good-commit>
cd deploy && sudo docker compose up -d --build
```

## What this stack doesn't do

- doesn't take ports 80/443 and publishes none;
- doesn't create or delete `docker_palker-network`;
- doesn't touch other containers, volumes or files outside `/opt/fyutt`
  and its single nginx block.
