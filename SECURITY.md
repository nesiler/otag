# Security Policy

## Reporting a vulnerability

Please do not open a public issue. Report privately through GitHub Security Advisories:
<https://github.com/nesiler/otag/security/advisories/new>

Include affected version or commit, reproduction steps, and impact. We aim to acknowledge reports within a few days.

## Scope

otag runs AI coding agents locally and enforces guard rules around them. In scope:

- Guard or hook bypass (a child agent performing an action the configured guard level, light or strict, should block).
- Worktree escape (an agent reading or writing outside its task worktree or permitted paths).
- Destructive operations (unintended deletion or modification of repositories, branches, worktrees, or user config).
- Leaking run tokens or credentials, or a child agent acting on another run.

Out of scope: vulnerabilities in the agents themselves (Claude Code, Codex, OpenCode), tmux, or git; and issues requiring an already-compromised local account. The guard is best-effort feedback on top of each harness's own sandbox, not a security boundary on its own.

## Supported versions

Only the latest release receives security fixes.
