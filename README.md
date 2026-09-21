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
| Automatic background probing on an interval, so a ranking cannot go stale | **Working** |
| `daemon.json` + containerd `hosts.toml` generation, merging into what you already have | **Working** |
| GitHub Actions → Alibaba Cloud ACR relocation (workflow generation + dispatch) | **Working** |
| Opt-in relay mode (`ddn-k8s` / `crproxy` style address rewriting) | **Working** |

The panel is the foundation everything else hangs off, so it came first. Nothing here pretends to do more than it does: the dashboard does not show empty charts for features that do not exist yet.

## How the measurement works

Each mirror is measured in four steps, and the speed test page reports them separately. Knowing *which* step failed is the difference between a useful list and a wall of red:

1. **Connect** — `GET /v2/`. Is the host up and speaking the registry protocol?
2. **Token** — how long the anonymous pull token takes. Where it comes from is the registry's decision, not ours: the `401` on `/v2/` carries a `WWW-Authenticate` challenge, and the token is fetched from the realm named in it — often a host that is not the mirror itself. A mirror can answer instantly and still be slow here.
3. **Manifest** — time to the first byte, and a digest computed from what actually arrived.
4. **Throughput** — pulling a real blob, capped at 8 MiB. This is the number that matters, because a mirror can answer instantly and still be useless.

Four rules shape the verdicts, and they are the reason to trust this over a ping:

- **Rate limiting is not failure.** A 429 means the mirror is up and will not talk to us right now. It gets its own amber result, because folding it into "down" would make healthy mirrors look broken.
- **A digest is never taken on trust.** The blob digest is computed locally from the bytes received, never read from a response header. And a read capped by the 8 MiB limit is reported as *unverifiable* rather than as a mismatch — accusing a mirror of serving bad content when the shortage was ours is exactly the wrong answer.
- **A dash is not a zero.** A layer that produced no number shows a dash. Zero would claim the step finished faster than the clock could resolve, which is a different statement from "it never ran".
- **Anonymous means anonymous, and redirects are followed the way a real pull follows them.** The probe never fills in a username or password: if a registry refuses an anonymous token, that is reported honestly. A `401` that advertises a token realm is not a refusal — it is how every anonymous pull begins, so it counts as a working connection, while a `401` or `403` with no realm at all is the real thing. Because a cross-host redirect drops the `Authorization` header, a mirror that answers with a `302` gets re-authorised once against the final URL — otherwise an alias such as `hub.rat.dev` would be written off as needing credentials while `docker pull` succeeds against it.

Every run records the digest the manifest resolved to, so two mirrors measured in the same batch can be compared honestly — a tag can be answered from a cache, and the same tag on two mirrors is not necessarily the same bytes.

None of it depends on you remembering to press the button. Every `probe.interval` the panel repeats this whole measurement for every enabled mirror on its own, which is what keeps the ranking on the config page from quietly becoming a ranking of last month. Two things worth knowing: the sweep needs no credentials, so it keeps running while the panel is locked; and `interval: 0s` turns it off, because these measurements come from wherever this process runs and not everyone wants that traffic against someone else's mirror.

## Getting images you cannot reach

Measurement answers "which mirror is fastest". It does not answer "how do I actually pull this image". Three pages do that, and they exist because the obstacle changes shape depending on where the image lives:

**Config (`/config`)** — for images on Docker Hub. It ranks the mirrors you have enabled and measured, then writes the `daemon.json` (or containerd `hosts.toml`) those rankings imply. Two details are worth knowing:

- The order *is* the feature. Docker tries each mirror in turn and stops at the first that answers, so a fast mirror listed second is worth no more than a slow one first.
- Paste your existing `daemon.json` and it merges rather than replaces: `registry-mirrors` is overwritten, `insecure-registries` is merged, and anything it does not manage is carried through untouched. A document with comments or a trailing comma is **refused**, not silently corrected — Docker rejects those too, and pretending otherwise would hand you a config that fails later, somewhere less obvious.

It also tells you what it *cannot* do: `registry-mirrors` only applies to Docker Hub, so enabled mirrors proxying other registries are counted and set aside rather than written in to occupy a slot that does nothing.

**Sync (`/sync`)** — for images on `ghcr.io`, `quay.io`, `gcr.io` and friends, where no mirror helps. This is the "borrow someone else's network" route: a GitHub Actions runner can reach registries your machines cannot. The page generates the workflow, stores the credentials it needs as repository secrets, and can dispatch a run with a list of image addresses. "Dispatch accepted" and "copy succeeded" are different statements — GitHub says nothing after accepting the request — so the page shows the run log rather than claiming success on the button press.

**Relay (`/config`, lower half)** — opt-in, and deliberately off until you set an endpoint. A relay is not a mirror, and the difference is the shape of the address: a mirror speaks the registry protocol at the same path as upstream, while a relay takes the *whole original address as a path* and decides for itself where to fetch from. The worked example is Huawei Cloud SWR's public `ddn-k8s` relay. Set the endpoint once and the page turns any image address into a pull command.

None of these three touches your system. They generate text, and you decide what to do with it.

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

What you *can* do is start over. Running the panel with `-reset` deletes the account, the sessions and every stored credential in one transaction, then exits — it recovers nothing, it just clears the way to a fresh first-run setup. Your mirror sources, settings and probe history are left alone, since none of them were secret. This is the difference between "I lost my password" and "I lost my data": the first one is annoying, the second one is what the encryption is for.

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

probe:
  concurrency: 5         # mirrors measured at once, 1..16
  timeout: 15s           # per-request budget
  interval: 30m          # background sweep: minimum 1m, 0s turns it off
```

| Flag | Environment | Meaning |
|---|---|---|
| `-config` | `MIRRORPILOT_CONFIG` | Config file path (default `<data dir>/config.yaml`) |
| `-data` | `MIRRORPILOT_DATA` | Persistent data directory (default `/data`) |
| `-listen` | `MIRRORPILOT_BIND` | Listen address (default `0.0.0.0:8080`) |
| `-reset` | — | Delete the account, sessions and stored credentials, then exit (start over; recovers nothing) |
| — | `MIRRORPILOT_LOG_LEVEL` | Log level |
| — | `MIRRORPILOT_BASE_URL` | Externally visible URL |

## Putting TLS in front of the panel

The panel speaks plain HTTP on a single port and does not terminate TLS itself. If you need to reach it from anywhere other than your own LAN, put a reverse proxy in front of it. Caddy is the shortest path, because it handles certificates on its own.

Three things change when you do:

1. **Stop publishing the panel's port.** Let the proxy reach it over a Docker network instead, so there is no way to reach the panel except through TLS.
2. **Set `MIRRORPILOT_BASE_URL` to the external HTTPS URL.** That is what makes the panel mark its session cookie `Secure`. Leave it unset and your browser silently drops the cookie on every request, so logging in appears to do nothing.
3. **Keep `/data` mounted exactly as it was.** None of this touches the database.

Two certificate details matter if, like most home and small-office setups, you have **no domain name** — just an address:

- **Let's Encrypt needs a name to validate.** On a bare IP it cannot succeed: the HTTP-01 challenge is answered by whoever parks the address rather than by your server, and TLS-ALPN-01 is reset in transit. `tls internal` tells Caddy to issue from its own CA instead. The connection is still encrypted; the browser simply warns once that it cannot vouch for who is on the other end. That warning is the honest answer here, not a misconfiguration to hide.
- **`default_sni` is not optional.** RFC 6066 forbids clients from sending SNI for an IP literal, so a browser reaching `https://203.0.113.10/` arrives with an empty server name — and Caddy matches site blocks on SNI. Without a default, that handshake dies with `tlsv1 alert internal error`: the certificate is sitting right there, nothing claims it. You can recognise this because `curl -k https://203.0.113.10/healthz` fails while `openssl s_client -connect 203.0.113.10:443 -servername 203.0.113.10` happily shows the certificate.

A Caddyfile for an address with no name, verified on Caddy 2.11:

```
{
	default_sni 118.89.25.55
}

118.89.25.55 {
	tls internal
	encode gzip
	reverse_proxy mirrorpilot:8080
}

# Everything arriving in the clear is sent to the encrypted address. 8080 is
# kept answering for exactly this reason: it was the panel's address, and a
# redirect means old bookmarks fail loudly rather than silently.
:80 {
	redir https://118.89.25.55{uri} permanent
}

:8080 {
	redir https://118.89.25.55{uri} permanent
}
```

Mount it read-only into `caddy:2`, publish 80/443/8080 from the proxy rather than from the panel, and give Caddy a volume for `/data` so its CA survives a restart. Substitute your own address for `118.89.25.55` — all four places.

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
