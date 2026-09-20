# Contributing to MirrorPilot

Thanks for helping. This document covers what you need to get a change merged.

## Prerequisites

| Tool | Version |
|---|---|
| Go | as declared in `go.mod` |
| Docker | 20.10+ with Buildx (for multi-arch image testing) |
| Git | any recent version |

## Where things live

```
cmd/mirrorpilot/       Entry point: flags, config, wiring, shutdown
internal/config/       Runtime configuration (non-sensitive only)
internal/secret/       Password hashing, key derivation, AEAD sealing
internal/store/        SQLite persistence and migrations
internal/auth/         Account, sessions, the in-memory master key
internal/i18n/         Locale catalogues and language negotiation
internal/web/          HTTP surface, middleware, templates, static assets
```

The dependency direction is one-way: `web` depends on `auth` and `i18n`; `auth` depends on `secret` and `store`; `secret` and `store` depend on nothing in this project. Keep it that way — it is what makes the security-critical packages reviewable in isolation.

## Getting started

```bash
git clone https://github.com/DC1024/mirrorpilot.git
cd mirrorpilot

# Run locally (creates ./data on first start)
go run ./cmd/mirrorpilot -data ./data -listen 127.0.0.1:8080

# Or with the container
docker compose up -d --build
```

## Before opening a pull request

Run the same checks CI runs:

```bash
gofmt -l .          # must print nothing
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build ./...
```

For anything touching the image or the build chain, also verify both architectures:

```bash
docker buildx build --platform linux/amd64,linux/arm64 .
```

## Commit messages

We use [Conventional Commits](https://www.conventionalcommits.org/):

```
feat(web): add the mirror source list page
fix(store): keep the rekey transaction from half-committing
docs: clarify relay mode limitations
chore(ci): cache go modules
```

## Translation

The UI is bilingual. **Any change to user-facing text must update both locale files** in the
same pull request:

```
internal/i18n/locales/en.json
internal/i18n/locales/zh-CN.json
```

Two tests keep the catalogues honest, and both will fail if you are careless:

- `TestKeyParity` — the two files must have identical key sets, and matching `%d`/`%s` placeholders.
- `TestCatalogKeysAreAllReferenced` — every key must actually be used by a template or handler.

That second one is the interesting one. It means you cannot land a translation "for later": add
the string in the same change that renders it. Delete the last user of a key and the test tells
you to delete the key. Dead strings are indistinguishable from live ones in review, so the
compiler-adjacent check is what actually prevents the rot.

Before inventing a translation for a technical term, check [docs/glossary.md](docs/glossary.md).
Terms like *registry*, *mirror*, *manifest*, *blob*, and *digest* have agreed renderings —
consistency matters more than individual word choice.

## Working on the panel

A few things that are easy to get wrong:

- **Templates**: `layout.html` is the root; every page supplies a `{{define "content"}}`. They are
  parsed explicitly rather than with `ParseFS`, because `ParseFS` associates the receiver with
  whichever file it reads first and it reads in sorted order. Don't "simplify" it back.
- **Translation in templates** is `{{ .T "key" }}`. `T` is a *method* on the page data, not a func
  field, because `text/template` will only pass arguments to a method.
- **No inline `<style>` or `<script>`.** The Content-Security-Policy has no `'unsafe-inline'`, and
  that is worth more than the convenience. Theme switching is a `data-theme` attribute plus a
  media query; the client-side behaviour is `static/app.js`.
- **Adding a page**: add the name to `pages` in `internal/web/web.go`. Startup parses and validates
  every template, so a missing `content` block fails immediately rather than at request time.

## Security invariants

These are not negotiable, and a change that breaks one will not be merged:

- **Never commit real credentials**, not even in test fixtures. Use obviously fake values.
- **The master key never touches disk.** If you find yourself wanting to persist it, the design has
  been misread — see the README section on how the password works.
- **Anything written to the `credentials` table must go through `secret.Sealer`**, with the
  credential's name as associated data. That binding is what stops a ciphertext being moved from
  one slot to another and decrypting.
- **`users` and `vault` are single-row tables.** Don't add a code path that could create a second
  row, and don't remove the `CHECK (id = 1)` constraints.
- **Rekeying is one transaction.** A password change re-encrypts every credential and swaps the
  salt, fingerprint and password hash together. Splitting it across transactions can leave
  credentials sealed under a key nobody can derive, which is unrecoverable data loss.
- **New network calls to third-party services** must respect the rate limits described in the
  README; we do not hammer community mirrors.

If you find a security issue, please **do not open a public issue**. Report it privately to the
maintainers first.

## Adding a mirror to the built-in list

Not available yet: the mirror list and its trust grading are still to be built. When they land,
this is the process — and it is written down here so the bar is set before the first entry, not
after:

1. Open a [New mirror proposal](https://github.com/DC1024/mirrorpilot/issues/new?template=new_mirror.yml)
   issue with real test output from your own network.
2. Once it looks reasonable, add the entry to the built-in list.
3. Grade it honestly:

| Value | Meaning |
|---|---|
| `verified` | Run by a cloud vendor or established institution, with a public operator page |
| `known` | Publicly known project with a track record, but no institutional backing |
| `unknown` | Little or no public information about who operates it |

An `unknown` mirror being listed is not an accusation — it just means users deserve to know
before trusting their pull traffic to it.

## Scope

MirrorPilot measures mirrors, generates configuration, and relocates images through GitHub
Actions. It is intentionally **not** a registry proxy. Relay mode will be an opt-in module with
its own prerequisites; do not extend it into the default path.

## License

By contributing, you agree your work is released under the [MIT License](LICENSE).
