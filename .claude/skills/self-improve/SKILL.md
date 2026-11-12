---
name: self-improve
description: Persist corrections and learnings so future sessions benefit. Use when the user corrects your approach, says "记住/记一下/以后都这样", when you discover a project convention that isn't documented yet, or when finishing a task reveals a gap in the agent docs.
---

# Self-Improvement: Externalize the Lesson

The model does not change between sessions — only externalized knowledge
compounds. When something in this conversation revealed a better way, persist it
immediately, to exactly one place.

## Step 1 — Classify the learning

| Type of learning | Destination |
|---|---|
| Project fact not derivable from code/git (test cluster quirks, env gotchas, user preferences, workflows the user prefers) | Claude memory dir (`~/.claude/projects/-Users-fengcaiwen-GolandProjects-kubevpn/memory/`, one fact per file, then add a line to `MEMORY.md`) |
| Repo-wide convention or workflow rule that any contributor/agent should follow | This repo's `AGENTS.md` (pick the matching section; add a row to a table if one exists) |
| A correction to how *this workflow skill* should be executed | The relevant `SKILL.md` in `.claude/skills/` |
| Detailed reference material (helper tables, architecture maps) | `CLAUDE.md` |

## Step 2 — Deduplicate before writing

1. Search the destination for an existing entry covering the same fact —
   **update it** rather than adding a near-duplicate.
2. Do NOT record anything the repo already documents (CLAUDE.md, AGENTS.md,
   docs/, code comments) or anything `git log`/code structure already shows.
3. Do NOT record conversation-specific temporary state (a bug's current fix
   status, a one-off command that worked).

## Step 3 — Write it compactly

- One fact per memory file, kebab-case name, with `description:` frontmatter.
- For rules in `AGENTS.md`/`SKILL.md`: one bullet or table row, imperative form,
  placed in the section users of that rule will actually be reading.
- Convert relative dates to absolute ("as of 2026-11").

## Step 4 — Confirm in one line

Tell the user where you saved it, e.g.:
`已记住：写入 memory/xxx.md` or `已更新 AGENTS.md §6`。

## When a correction is disputed or situational

If the user's correction applies only to this task (not a durable preference),
do not persist it — silently adjust and move on. When unsure, ask the user
whether it should be remembered.
