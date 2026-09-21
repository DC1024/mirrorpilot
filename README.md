# MirrorPilot

> Find the fastest container registry mirror for your network, and get images that are unreachable from your machines.

[中文文档](README.zh-CN.md)

---

## What is this

MirrorPilot is a **self-hosted, single-binary web panel** that helps you deal with container registry access problems — especially in networks where `docker.io` and `ghcr.io` are slow or unreachable.

It is built for a specific situation: **you have no machine with reliable access to overseas registries.** That single constraint rules out the usual answer (a pull-through cache like Harbor or Zot, which has to reach the upstream itself) and shapes everything else here.

## What this is NOT

Being clear about boundaries saves everyone time:

- **It is not a registry proxy.** It does not sit in your pull path, and it cannot make an unreachable image reachable by itself.
- **It is not a drop-in replacement for KSpeeder.** KSpeeder bundles third-party mirror sources behind its own endpoint. MirrorPilot measures sources and helps you move images into a registry you control.
- **It does not modify your `daemon.json` behind your back.** It will generate configuration and commands for you to review and run.
- **It is not closed-source middleware.** No paid tier, no "free nodes" resold to you.

> A proxy is an amplifier, not a generator. It can only serve what its upstreams can serve.

## Project status

MirrorPilot is being built in milestones. This is what actually works today:

| Area | State |
|---|---|
| Panel: first-run setup, login, logout, sliding sessions | **Working** |
| Master-password encryption of stored credentials (Argon2id + AES-256-GCM) | **Working** |
| Locked/unlocked model with a one-time unlock after restart | **Working** |
| Password change with full credential re-encryption | **Working** |
| Bilingual UI (English / 简体中文) with browser-language negotiation | **Working** |
| Light / dark / follow-system themes | **Working** |
| Multi-arch image (`linux/amd64`, `linux/arm64`) | **Working** |
| Mirror catalogue with trust grading, plus mirrors you add yourself | **Working** |
| Four-layer speed probe with per-mirror history | **Working** |
| `daemon.json` config generation | Planned |
| GitHub Actions → Alibaba Cloud ACR relocation | Planned |
| Opt-in relay mode | Planned |

The panel is the foundation everything else hangs off, so it came first. Nothing here pretends to do more than it does: the dashboard does not show empty charts for features that do not exist yet.

## How the measurement works

Each mirror is measured in four steps, and the speed test page reports them separately. Knowing *which* step failed is the difference between a useful list and a wall of red:

1. **Connect** — `GET /v2/`. Is the host up and speaking the registry protocol?
2. **Token** — how long the anonymous pull token takes. A mirror can answer instantly and still be slow here.
3. **Manifest** — time to the first byte, and a digest computed from what actually arrived.
4. **Throughput** — pulling a real blob, capped at 8 MiB. This is the number that matters, because a mirror can answer instantly and still be useless.

Three rules shape the verdicts, and they are the reason to trust this over a ping:

- **Rate limiting is not failure.** A 429 means the mirror is up and will not talk to us right now. It gets its own amber result, because folding it into "down" would make healthy mirrors look broken.
- **A digest is never taken on trust.** The blob digest is computed locally from the bytes received, never read from a response header. And a read capped by the 8 MiB limit is reported as *unverifiable* rather than as a mismatch — accusing a mirror of serving bad content when the shortage was ours is exactly the wrong answer.
- **A dash is not a zero.** A layer that produced no number shows a dash. Zero would claim the step finished faster than the clock could resolve, which is a different statement from "it never ran".

Every run records the digest the manifest resolved to, so two mirrors measured in the same batch can be compared honestly — a tag can be answered from a cache, and the same tag on two mirrors is not necessarily the same bytes.

## Quick start

```bash
git clone https://github.com/DC1024/mirrorpilot.git
cd mirrorpilot
docker compose up -d
```

Then open `http://<host>:8080`. On first run you are asked to create the panel account — there is no default password, and nothing is written to disk in advance.

> **Do not expose the panel to the public internet.** It is designed to hold credentials for your GitHub and Alibaba Cloud accounts. Keep it on your LAN, or put it behind a reverse proxy that terminates TLS.

## How the password works

This is the one design decision worth reading before you trust the panel with a token.

Your password does two jobs:

1. **It logs you in.** A hash of it (Argon2id) is stored; the password itself never is.
2. **It derives the encryption key** that protects the third-party credentials you save later. A random salt is stored alongside the database, and the key is derived from password + salt.

The derived key lives **in process memory only**. That has a consequence you will notice: after a container restart the panel is still logged in — your session cookie survived because sessions are rows in the database — but it cannot read a single stored credential until you enter the password once. Only the first request after a restart asks for it.

That is deliberate. Tying the key to a browser session would mean background work (probing, scheduled relocation) stops the moment you close a tab. The key lives as long as the process, and the session is a separate, shorter thing.

**If you forget the password, the stored credentials are unrecoverable.** There is no reset link, and no recovery key. That is the point: it is also what makes a stolen `/data` volume useless on its own.

## Configuration

Precedence, lowest to highest: built-in defaults → `config.yaml` → environment variables → command-line flags.

```yaml
# /data/config.yaml
listen: 0.0.0.0:8080
data_dir: /data
log_level: info          # debug | info | warn | error

# Set this to your externally visible HTTPS URL when a reverse proxy terminates
# TLS for you. It is what makes the panel mark its cookies Secure. Leave it
# empty when talking to the panel over plain HTTP on a LAN, or your browser will
# silently drop the session cookie.
base_url: ""

probe:                   # reserved for the probing engine
  concurrency: 5
  timeout: 15s
  interval: 30m
```

| Flag | Environment | Meaning |
|---|---|---|
| `-config` | `MIRRORPILOT_CONFIG` | Config file path (default `<data dir>/config.yaml`) |
| `-data` | `MIRRORPILOT_DATA` | Persistent data directory (default `/data`) |
| `-listen` | `MIRRORPILOT_BIND` | Listen address (default `0.0.0.0:8080`) |
| — | `MIRRORPILOT_LOG_LEVEL` | Log level |
| — | `MIRRORPILOT_BASE_URL` | Externally visible URL |

## Data & backup

Everything persistent lives under `/data`:

```
/data/config.yaml      Non-sensitive configuration
/data/app.db           SQLite database: account, session, encrypted credentials
```

Backing up is copying this one directory. Migrating is dropping it into a new host. The database is useless without the password — see above.

## Development

```bash
go run ./cmd/mirrorpilot -data ./data -listen 127.0.0.1:8080
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the checks CI runs and the rules around user-facing text.

## Disclaimer

Mirror addresses listed in this project are collected from publicly available sources. The project does not operate or endorse any of them. Third-party mirrors may substitute content, retain traffic data, or shut down without notice. Only pull from mirrors you trust. For production or sensitive workloads, run your own registry or use a cloud provider's in-region registry service.

## License

[MIT](LICENSE)
