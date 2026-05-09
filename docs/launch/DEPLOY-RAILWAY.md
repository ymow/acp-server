# Deploy acp-server on Railway

> Phase 4.2 observation work flagged "deploy" as the most-asked
> category. This is the canonical Railway recipe. If your deploy target
> differs (Fly, Zeabur, fly-self-hosted, raw VPS), see the sibling
> guides or open an Observation issue.

---

## TL;DR

```
1. New project on Railway → "Deploy from GitHub" → ymow/acp-server
2. Add a Volume mounted at /data
3. Set 5 env vars (table below)
4. Generate the master key with `openssl rand -hex 32` — save it somewhere safe
5. After first deploy, exec into the container and place the key
6. Healthcheck: GET /health returns 200
```

That's it. Detailed walkthrough below.

## Why a volume matters

acp-server's durable state is two things:

1. **SQLite DB** — the audit log, covenants, members, tokens. If you
   lose this, you lose everything; the hash chain is the proof.
2. **Keyring directory** — the AES-256-GCM master key (and any rotated
   versions). If you lose this, you can't decrypt the encrypted
   `platform_id` columns. **Lose the keyring AND the DB and you have
   nothing. Lose just the keyring and the encrypted columns are
   permanently unreadable.**

Both must live on a persistent volume. Railway's container filesystem
is ephemeral; restarts wipe it.

## Environment variables

| Var | Required? | Default | Notes |
|---|---|---|---|
| `ACP_ADDR` | yes | `:8080` | Railway sets this via `PORT` — bind to that |
| `ACP_DB_PATH` | yes | `./acp.db` | **Set to `/data/acp.db`** for volume persistence |
| `ACP_KEY_FILE` | recommended | `$HOME/.acp/keys/v1.key` | **Set to `/data/keys/v1.key`** |
| `ACP_LOG_LEVEL` | optional | `info` | `debug` for first-week observation; `info` after |

For Railway specifically, you'll bind to whatever port Railway gives:

```
ACP_ADDR=:$PORT
ACP_DB_PATH=/data/acp.db
ACP_KEY_FILE=/data/keys/v1.key
```

## Step-by-step

### 1. Create the Railway project

Railway dashboard → New Project → "Deploy from GitHub repo" → select
`ymow/acp-server`. Railway will detect the Go project, run
`go build ./...`, and produce the binary.

If the build doesn't auto-detect, add a `nixpacks.toml` to your fork:

```toml
[phases.setup]
nixPkgs = ["go_1_25"]

[phases.build]
cmds = ["go build -o /app/acp-server ./..."]

[start]
cmd = "/app/acp-server"
```

### 2. Add a Volume

Railway dashboard → your service → Settings → Volumes → New Volume.

- **Mount path:** `/data`
- **Size:** 1 GB is plenty for the first 6 months of observation usage.
  Audit logs grow ~1 KB per action; SQLite stays compact.

### 3. Set environment variables

Railway dashboard → Variables. Paste the four vars from the table above.

### 4. Generate the master key

**On your laptop**, not on Railway:

```bash
openssl rand -hex 32
```

You'll get something like:

```
73871e882ca760bbe9b18468abee8d4ab696a8b75489e1baf151a28f5443e927
```

**Save this somewhere safe.** A password manager works. If your laptop
dies and the volume dies, this is the only way to recover encrypted
data — losing it means permanent decryption failure.

### 5. Deploy and place the key

Railway will deploy on push. Wait for the first deploy to fail or
succeed-but-fail-on-startup (it'll panic because no key exists).

Then connect to the container shell:

```
railway run --service acp-server bash
```

Inside the container:

```bash
mkdir -p /data/keys
cat > /data/keys/v1.key <<'EOF'
73871e882ca760bbe9b18468abee8d4ab696a8b75489e1baf151a28f5443e927
EOF
chmod 600 /data/keys/v1.key
```

Restart the service. Logs should show:

```
acp-server listening on :8080
loaded keyring v1 (fingerprint: <some-hash>)
```

### 6. Verify the deployment

```bash
curl https://<your-app>.up.railway.app/health
# {"status":"ok"}
```

Then run through the Quickstart against the live URL — create a
Covenant, configure tiers, submit a passage, settle.

## Healthcheck for Railway's internal monitor

Railway dashboard → Service settings → Health check.

```
Path:    /health
Port:    $PORT
Interval: 30s
```

Acp-server's `/health` is unauthenticated and returns `{"status":"ok"}`.
No DB query — it's a liveness probe, not a readiness probe.

## Rotating the key on Railway

Per ACR-700, rotate the master key periodically (90-day cadence is
reasonable for a Phase 4.2 deployment).

```bash
railway run --service acp-server bash
./acp-server rotate-key
./acp-server reencrypt
```

`rotate-key` is O(1) (generates `keys/v2.key`, marks v1 as previous).
`reencrypt` is O(rows) and idempotent — safe to re-run if interrupted.
After both complete, encrypted columns are sealed under v2; v1 is kept
on disk for one rotation cycle so old backups still decrypt.

## Backup strategy

The minimum viable backup:

1. **Daily** — `railway run --service acp-server cp /data/acp.db /tmp/acp.db.$(date +%F)` then download.
2. **Weekly** — copy the entire `/data/keys/` directory to encrypted
   off-Railway storage (e.g. password manager attached file, encrypted
   S3 bucket, you-trust-it).
3. **After every key rotation** — full snapshot of `/data/keys/` and
   `/data/acp.db` so the pre-rotation state is recoverable.

## Common Railway-specific gotchas

| Symptom | Cause | Fix |
|---|---|---|
| Container OOM | Default memory tier too small | Bump to 1GB+ in Railway settings |
| Volume mount fails | Volume created on wrong service | Create volume after the service exists, not before |
| Health check fails | `$PORT` not bound | Verify `ACP_ADDR=:$PORT` literally — Railway substitutes |
| Builds slow | Go modules re-download | Add Railway's Go module cache plugin |
| Container restarts every X hours | Railway free-tier sleep | Upgrade to Hobby plan ($5/mo); ACP needs steady-state |

## Cost

Railway Hobby plan: $5/month base + ~$5/month for a small service +
storage = expect $10–15/month for a single-server ACP deployment with
modest traffic. Free tier won't keep the container warm; ACP's
durability assumptions break if it restarts every 12h.

## Don't deploy if...

- You don't have a backup strategy yet for `/data/keys/`. Lose the
  keyring and encrypted columns are dead. Plan backups before you
  trust any platform_id to encryption.
- You're going to use Railway's "rollback to previous deploy" feature
  for routine rollbacks. Each rollback resets the keyring directory
  contents unless they're on the volume — verify the keyring is on
  `/data` before you ever rollback.
- You expect to migrate workspaces frequently. SQLite + a keyring is
  fine but it's not a horizontally-scalable database. For Phase 4.2
  observation, single-instance is correct.

## Next steps

- Run `acp-doctor` against your live DB once a week:
  `railway run --service acp-server ./acp-doctor --db /data/acp.db`
- Subscribe to release notifications on github.com/ymow/acp-server.
- File an Observation issue if anything in this guide didn't match
  reality — those are gold during Phase 4.2.
