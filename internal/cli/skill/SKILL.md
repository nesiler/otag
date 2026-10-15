---
name: otag
description: Coordinate AI coding agents and tasks across projects with the otag CLI. Use when the user asks to plan, hand out, start, watch, review or deliver tasks to child agents (Claude, Codex, OpenCode), or when you run inside a otag run (OTAG_RUN_ID is set).
metadata:
  managed-by: otag
---

# otag

otag coordinates AI coding agents: tasks go to child agents that each work in their own
git worktree; a coordinator watches, answers their questions and reviews the results.

Run `otag prime` first: it prints your role (child, review, coordinator or none), your
task or project, the otag commands open to you and your unread messages.

Core commands:
- `otag status`, `otag needs`: project overview; what waits for you
- `otag task add "title" --body "..."`, `otag task list`, `otag task show <id>`
- `otag run start <task> --no-attach`, `otag run stop <run>`
- `otag inbox`, `otag reply <id> "text"`, `otag send <run|task|coord|human> "text"`
- `otag review <task> --accept|--reject "reason"`

Inside a run you are a child: finish with `otag done "summary"` (or `otag fail "reason"`),
ask the coordinator with `otag ask "question"`, and never push or merge.
