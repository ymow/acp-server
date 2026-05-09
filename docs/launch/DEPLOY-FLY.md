# Deploy acp-server on Fly.io

> Same durability concerns as Railway (SQLite DB + keyring directory
> must persist), different mechanics. Fly's volume model is more
> explicit, which is actually a feature for ACP.

---

## TL;DR

```
1. fly launch (uses included Dockerfile / let Fly generate one)
2. fly volumes create acp_data --size 1 --region <your-region>
3. Set ACP_DB_PATH=/data/acp.db, ACP_KEY_FILE=/data/keys/v1.key as secrets
4. Generate master key with `openssl rand -hex 32`, save it
5. After first deploy: fly ssh console; place the key under /data/keys/
6. fly status — should be passing /health
```

## Why Fly is a good fit for ACP

- **Volumes are first-class.** No surprise filesystem ephemerality.
- **Single-region by default.** ACP is single-instance + SQLite; multi-
  region clustering doesn't help us yet.
- **`fly ssh console` for keyring placement** — beats Railway's
  exec-into-container UX.
- **No idle sleep.** Container stays warm; no 12h restart cycles
  trashing in-memory state.

## Prerequisite

Fly CLI installed (`brew install flyctl` or curl install per Fly docs).
`fly auth login`.

## Step-by-step

### 1. Initialise the Fly app

From the cloned repo root:

```bash
fly launch --no-deploy
```

Pick:
- App name: `acp-server-<your-handle>` (must be globally unique)
- Region: closest to you (single region is fine)
- Postgres: **No**
- Redis: **No**
- Deploy now: **No** — we need the volume first

Fly will generate `fly.toml`. Edit it:

```toml
app = "acp-server-<your-handle>"
primary_region = "<your-region>"

[build]
  builder = "paketobuildpacks/builder:base"

[env]
  ACP_ADDR = ":8080"
  ACP_DB_PATH = "/data/acp.db"
  ACP_KEY_FILE = "/data/keys/v1.key"

[http_service]
  internal_port = 8080
  force_https = true
  auto_stop_machines = false
  auto_start_machines = true
  min_machines_running = 1
  processes = ["app"]

  [[http_service.checks]]
    grace_period = "10s"
    interval = "30s"
    method = "GET"
    timeout = "5s"
    path = "/health"

[mounts]
  source = "acp_data"
  destination = "/data"

[[vm]]
  size = "shared-cpu-1x"
  memory = "512mb"
```

`auto_stop_machines = false` is intentional — ACP must stay warm. The
default Fly behaviour stops idle machines, which is fine for a stateless
app and catastrophic for ACP's hash chain integrity assumptions.

### 2. Create the volume

```bash
fly volumes create acp_data --size 1 --region <your-region>
```

1 GB is plenty for early Phase 4.2 traffic. Fly volumes can be expanded
later but not shrunk; start small.

### 3. Generate the master key

**Locally:**

```bash
openssl rand -hex 32
```

Save it in a password manager. This key is the only way to decrypt
ACR-700-sealed columns.

### 4. First deploy

```bash
fly deploy
```

The first deploy will start, fail to find the master key, and exit.
That's expected — let it create the machine + volume mount.

### 5. Place the key

```bash
fly ssh console
```

Inside the machine:

```bash
mkdir -p /data/keys
cat > /data/keys/v1.key <<'EOF'
<paste the hex from openssl rand>
EOF
chmod 600 /data/keys/v1.key
exit
```

Restart:

```bash
fly machine restart
```

Logs should now show:

```bash
fly logs
```

```
acp-server listening on :8080
loaded keyring v1 (fingerprint: <hash>)
```

### 6. Verify

```bash
curl https://acp-server-<your-handle>.fly.dev/health
# {"status":"ok"}
```

## Volume snapshots = backups

Fly takes daily snapshots of volumes by default. Verify:

```bash
fly volumes snapshots list
```

For ACP's risk profile, **enable explicit weekly off-Fly snapshots
too** for the keyring:

```bash
fly ssh console
tar czf /tmp/keys-backup-$(date +%F).tar.gz /data/keys
exit

fly ssh sftp shell
get /tmp/keys-backup-2026-05-09.tar.gz
exit
```

Then encrypt and store off-platform. Don't trust a single provider for
the only copy of your keyring.

## Rotating the key on Fly

```bash
fly ssh console
./acp-server rotate-key
./acp-server reencrypt
exit
```

Same semantics as Railway: `rotate-key` is O(1), `reencrypt` is
O(rows). Old keyring versions stay on disk for one rotation cycle so
backups remain decryptable.

## Common Fly gotchas

| Symptom | Cause | Fix |
|---|---|---|
| Volume not attached | `[mounts]` block missing or wrong volume name | Edit `fly.toml`, `fly deploy` |
| Healthcheck fails | port mismatch (Fly internal vs Go bind) | Confirm `internal_port = 8080` matches `ACP_ADDR` |
| Container won't start | Master key missing | `fly ssh console`, place key, restart |
| Random restarts | `auto_stop_machines = true` | Force `false` for ACP |
| Build slow | Buildpack re-downloads Go modules | Switch to a Dockerfile, cache modules explicitly |

## Custom Dockerfile (if buildpack is too slow)

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /out/acp-server ./cmd/acp-server

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/acp-server /usr/local/bin/acp-server
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/acp-server"]
```

Then in `fly.toml` change `[build]` to:

```toml
[build]
  dockerfile = "Dockerfile"
```

## Cost

Fly's pricing: shared-cpu-1x with 512MB + 1GB volume + minimal egress
≈ $4–6 USD/month. Cheaper than Railway for steady-state, more flexible
for moving to a dedicated CPU later if traffic grows.

## When NOT to use Fly

- If you need multi-region failover. Phase 4.2 single-instance
  assumption breaks under multi-region; defer this to Phase 8 or beyond.
- If you've never used flyctl. Railway is simpler for first-time
  deployers.
- If your team prefers click-ops dashboards. Fly is heavily CLI-based.

## Next steps

- File an Observation issue tagged `deploy` if anything broke or
  surprised you.
- Once you're comfortable, the next thing to try is connecting an MCP
  client (`docs/mcp` on the docs site) — that's where ACP starts paying
  off.
