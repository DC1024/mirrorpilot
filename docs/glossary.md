# Glossary

Agreed renderings for the terms that appear in the UI. Consistency matters more than any
individual word choice, so if you think a term is wrong, change it here first — in the same pull
request — rather than in one locale file only.

| English | 简体中文 | Notes |
|---|---|---|
| registry | 镜像仓库 | Not 注册表 — in this context that means the Windows registry, which is a different thing entirely. |
| mirror | 镜像源 / 加速源 | 镜像源 when naming the thing; 加速源 when describing what it is for. Not 镜像 alone, which reads as "image". |
| image | 镜像 | |
| manifest | manifest | Left in English. It has no widely accepted Chinese rendering, and inventing one creates more confusion than it removes. |
| blob | blob | Same reasoning as manifest. |
| digest | digest | Same reasoning. Where a Chinese sentence needs it to read naturally, use "digest（摘要哈希）" once and "digest" thereafter. |
| tag | tag / 标签 | 标签 is fine in prose; prefer `tag` when it appears next to a literal value. |
| pull / push | 拉取 / 推送 | |
| upstream | 上游 | |
| throttle / rate limit | 限流 | Keep "429" visible in any message that mentions it — the status code is the actionable part. |
| probe / speed test | 测速 | |
| egress | 出口 | As in "measurement egress" — 测速出口. |
| trust level | 可信度 | |
| relocate | 搬运 | Not 迁移, which is what you do to a database. |
| credential | 凭据 | Not 凭证 — pick one and stay with it; 凭据 is the more common rendering in Chinese developer documentation. |
| session | 会话 | |
| lock / unlock | 锁定 / 解锁 | This refers specifically to the master key being absent from memory, not to a locked account. |
| master password | 主密码 | |
| panel | 面板 | |
| relay | 中转 / 中转站 | 中转站 names the endpoint; 中转 describes the mode. Not 代理 — a relay is not a proxy in the pull path, and calling it one reintroduces exactly the confusion the README works to remove. |
| relay endpoint | 中转地址 | The host (plus optional path prefix) that gets prepended to an image address. |
| workflow | 工作流 | Refers to the GitHub Actions file. Left as 工作流 in prose; the filename itself is never translated. |
| dispatch | 触发运行 | The act of asking GitHub to run a workflow. Not 触发, which is too bare; not 部署, which is a different claim. |
| run / job | 运行 / 任务 | A run is one execution of the workflow; the jobs inside it are 任务. Keep them distinct — "run failed" and "job failed" are different diagnoses. |
| secret | 仓库密钥 | In the GitHub sense. Kept as 仓库密钥 so it reads as the GitHub feature, not as anything the panel stores. |

## Left in English, deliberately

These appear verbatim in both locales. They are filenames, config keys, or protocol terms where a
Chinese rendering would be invented rather than translated:

| Term | What it is |
|---|---|
| `daemon.json` | Docker Engine configuration file. `registry-mirrors` only ever applies to Docker Hub — this constraint is the reason the config page counts the mirrors it cannot use. |
| `registry-mirrors` / `insecure-registries` | Keys inside `daemon.json`. The first is replaced wholesale, the second merged. |
| `hosts.toml` | The per-registry file containerd 1.7+ reads, at `/etc/containerd/certs.d/<host>/hosts.toml`. |
| `ddn-k8s` | The address shape of the Huawei Cloud SWR public relay (`swr.cn-north-4.myhuaweicloud.com/ddn-k8s/<full upstream address>`). Used as the worked example for a relay endpoint. |
| `workflow_dispatch` | The GitHub Actions event a dispatch targets. |

## Tone

- Chinese UI text uses full-width punctuation and no space between Chinese and Latin text, except
  where a space genuinely aids reading (for example around a literal value like `daemon.json`).
- English UI text is sentence case, not Title Case, for anything longer than a button label.
- Buttons are verbs or short verb phrases: *Save*, *Unlock*, *Log out* — not *Submit*.
- Error messages say what happened and what to do about it. "Session expired" is a fact;
  "Your session expired. Please log in again." is a message.
