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

## Tone

- Chinese UI text uses full-width punctuation and no space between Chinese and Latin text, except
  where a space genuinely aids reading (for example around a literal value like `daemon.json`).
- English UI text is sentence case, not Title Case, for anything longer than a button label.
- Buttons are verbs or short verb phrases: *Save*, *Unlock*, *Log out* — not *Submit*.
- Error messages say what happened and what to do about it. "Session expired" is a fact;
  "Your session expired. Please log in again." is a message.
