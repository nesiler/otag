# Setup

How otag gets from `brew install` to a working configuration, and how to change it later.

## First run

Run `otag` in a terminal. On a machine without a configuration it starts the setup
wizard (the same as `otag init`); once otag is set up, `otag` opens the TUI. Without a
terminal (a script, a pipe) or with `--json`, `otag` prints the usage and a hint instead
and never opens a UI.

The wizard (keyboard and mouse; Esc or ctrl+c cancels at any step, and nothing is
written before the last one):

1. **Welcome.** Finds `claude`, `codex` and `opencode` on your PATH (with their versions
   and default config dirs), `tmux`, `git`, and the git repository of the current
   directory. Without tmux or git it says how to install them (`brew install tmux`) and
   stops.
2. **Agents.** One checkbox per agent CLI found. Each becomes an account that uses your
   normal login: no `config_dir`, so Claude Code uses `~/.claude`, Codex `~/.codex` and
   OpenCode its usual `~/.config/opencode`. If `$CLAUDE_CONFIG_DIR` or `$CODEX_HOME`
   names another directory when setup runs, that directory becomes the account's
   `config_dir` (runs do not inherit those variables, so it would be lost otherwise).
3. **Profiles**, one page per agent, each preset on/off with its model and effort:

   | Profile | Model | Effort | Used for |
   |---|---|---|---|
   | `claude-opus` | `opus` | high | the coordinator, hard tasks |
   | `claude-sonnet` | `sonnet` | default | the default worker |
   | `claude-haiku` | `haiku` | default | fast, simple tasks |
   | `codex` | Codex's own (`~/.codex/config.toml`) | high | hard tasks, reviews |
   | `codex-medium` | Codex's own | medium | everyday tasks |
   | `opencode` | OpenCode's own | — | everyday tasks |

   The Claude models are Claude Code's aliases (`claude --model opus`); an empty model
   passes no `--model` flag, so the harness uses its configured default. Fallback lists
   are wired for you across the agents you chose (for example `claude-opus` →
   `codex` → `claude-sonnet`): when an account hits a quota limit or a launch fails, the
   daemon retries the task on the next profile.
4. **Project.** If the current directory is in a git repository, it is offered as a
   project: name (the directory name), a short unique task id prefix, `workspace =
   "worktree"`, `guard = "light"`, `deny = [".env*", "*.pem"]`, the coordinator
   (`claude-opus`), the default profile (`claude-sonnet`) and a review profile on another
   agent when there is one. The coordinator runs on Claude Code only; without a Claude
   profile there is no coordinator, but tasks still run on the other agents. The same
   page asks for the branch settings: the task branch prefix (`feature/`), the base new
   task branches start from (blank: the checkout's HEAD; the repository's remote default
   branch, such as `origin/main`, is shown as the suggestion) and who may push task
   branches (`coordinator`). See "Branches, base and push" below.
5. **Background service.** Installs the daemon (a launchd agent,
   `io.github.nesiler.otag`; on by default), the same as `otag daemon install`.
6. **Agent skill.** Installs the otag skill for Claude Code, Codex and OpenCode, the same
   as `otag skill install`. Off by default: it writes global agent config
   (`~/.agents/skills/otag`, linked from `~/.claude/skills/otag`).
7. **Review.** Lists exactly which files will be written (and what runs after), then
   **Write** writes them atomically (a temporary file and a rename), runs `otag sync`, the
   daemon and skill installs you chose, and offers to open the TUI.

Until **Write**, nothing is written: the database is only read (read-only, without even
a journal file), the config dir is not touched. On **Write** otag takes a lock in the
config dir (`.otag-config.lock`; `otag init`, `otag project` and `otag sync` take it, so
they never interleave), reads the database again (the project's automatic name and
prefix are chosen again from it; a removed project gets its old ones back, a prefix you
typed is kept, and one that clashes is refused), and loads the result the way `otag
sync` would: a configuration that would not load is never written.

New files are put in place only if nothing is there yet (an exclusive rename,
`RENAME_EXCL`, so it also works on file systems without hard links), a changed
`registry.toml` keeps its file mode, and a backup never replaces an earlier one (`-2`,
`-3`, ... are added).

### Editing the files while setup writes them

Editors do not take otag's lock, so the change to an existing `registry.toml` (an
append, or `--force`) is made so that an edit saved at any moment is never lost:

1. The new content is written to a temporary file.
2. The current `registry.toml` is moved aside (renamed to a hidden
   `.registry.toml.otag-old`), and the moved file is hashed.
3. If it is not the file setup read (you saved in between), it is moved back and
   nothing is written: run the command again.
4. Else the new file is renamed into place without replacing anything. If you saved
   after step 2, your save created `registry.toml` again: otag leaves it there, keeps
   the version before it aside (the error names it) and writes nothing.
5. Only then is the aside copy removed (or, with `--force`, renamed to the backup).

For an instant between steps 2 and 4 there is no `registry.toml`; `otag sync` waits
for the lock and never sees that. An editor that keeps the file open and writes into
it in place (not by saving a new file) during those milliseconds writes into the
moved-aside copy, which is then kept only as a `--force` backup: save, then run
setup.

### An interrupted change

If otag is stopped in the middle of a change (a crash, kill -9), a file can be left
aside: `.registry.toml.otag-old`, or `projects/.<name>.toml.otag-removing` from `otag
project remove`. Every command that changes or syncs the config (`otag init`, `otag
project`, `otag sync`, also through the TUI) first takes the lock and checks for them:

- the original file is missing: it is put back (a no-replace rename) and the command
  says so, so a registry is never taken for a fresh machine and a removal is either
  complete or not done at all (run `otag project remove` again);
- the original exists too (it was saved again, or the change completed except for
  the cleanup): otag touches neither, refuses to change the config and names both
  files. Keep the version you want under the original name and delete the other.

`otag doctor` reports either case as a failure with its fix. Temporary files of an
unfinished write (`.<file>.otag-tmp-*`) never hold the only copy of anything and are
removed.

### `otag init`

`otag init` runs the wizard explicitly.

| Flag | Meaning |
|---|---|
| `--yes` | No questions: accept what was detected, register the current directory's repository, install the daemon (no skill). Needed without a terminal and with `--json`. |
| `--no-daemon` | Do not install (or offer) the daemon. |
| `--force` | Start over: the current `registry.toml` is copied to `registry.toml.bak-<timestamp>`, then replaced. Project files stay, and must still load with the new profiles. |

On a machine that is already set up, `otag init` only offers what is missing: agents
installed since (their account and profiles are appended to `registry.toml`, your own
text above stays byte for byte), the current repository if it is not a project yet, and
the daemon if it is not installed. With nothing to add it says so; running it twice
changes nothing.

## Projects

```sh
otag project add [path] [--name N] [--prefix P] [--guard light|strict] [--workspace worktree|same]
                 [--branch-prefix B] [--base REF] [--push none|coordinator|child] [--remote R] [--yes]
otag project list
otag project remove <name>
```

`project add` registers the git repository of `path` (default: the current directory;
a subdirectory registers its top level), with the same defaults as the wizard. On a
terminal it shows the wizard's project page to confirm and edit; without one it needs
`--yes`. It writes `projects/<name>.toml` and runs `otag sync`.

A repository that was a project before (`otag project remove`) gets its old name back,
and with it its prefix and tasks.

`project remove` renames the project's file to `<name>.toml.removed-<timestamp>` (kept,
no longer loaded; never over an earlier one) and runs `otag sync`, which retires the
project. Sync never deletes anything: the project's tasks, runs, messages and history
stay in the database, and its worktrees and branches stay on disk. A project with runs
that have not ended is refused (stop them first); the check is repeated in the
transaction that retires the project, so a run started meanwhile is seen and the file
is put back. `otag sync` waits for the config lock, so it never syncs the moment the
file is moved aside. `otag project add` brings it back, with its old prefix and tasks.

In the TUI, a tree without projects says what to do; if `otag tui` was started inside a
git repository that is not a project yet, `a` (or the context menu) adds it.

## The generated files

`~/.config/otag/registry.toml` (or `$OTAG_CONFIG_DIR/registry.toml`) holds the accounts
and profiles; `~/.config/otag/projects/<name>.toml` one project each. Both are plain,
commented TOML: edit them freely, then run `otag sync` (`otag doctor` tells you when the
database and the files differ).

Account keys: `harness` (claude | codex | opencode), `config_dir`, `command`, `env`,
`quota`. Profile keys: `account`, `model`, `effort`, `mode` (interactive | headless),
`tags`, `fallback`, `env`, `env_passthrough`, `args`, `description`. Project keys:
`path`, `prefix`, `coordinator_profile`, `default_profile`, `review_profile`,
`workspace`, `guard`, `deny`, `branch_prefix`, `base`, `remote`, `push` (and any extra
key, stored as is).

### Branches, base and push

All four are optional; a project without them works as before, except that new task
branches are named `feature/<task>` (it was `otag/<task>`).

| Key | Default | Meaning |
|---|---|---|
| `branch_prefix` | `"feature/"` | A task's branch is `<branch_prefix><task>`, the same name locally and on the remote. `""` is the task id alone. It must make a valid branch name (`git check-ref-format`). A task keeps the branch it got when its worktree was first created, so changing the prefix only affects new tasks (existing ones keep `otag/<task>`). |
| `base` | unset: the checkout's HEAD | The ref new task branches start from. A remote-tracking ref (`origin/dev`, where `origin` is a configured remote) is fetched first (`git fetch origin dev`); a failed fetch fails the run's start with the error (otag never falls back to a stale ref). A local branch or a commit is used as it is and must exist. `otag task add/edit --base` overrides it for one task until its worktree exists. |
| `remote` | the remote of `base` when `base` is a remote-tracking ref, else `origin` | Where `otag task push` pushes. |
| `push` | `"coordinator"` | Who may run `otag task push`: `none` (nobody; otag never pushes), `coordinator` (the project's coordinator and you), `child` (also the task's own child run). An agent's own `git push` stays refused by the guard in every case. |

With `push = "child"` the push runs inside the child's sandbox: a strict-guard run of a
harness whose sandbox has no network or protects the remote-tracking refs (Codex's,
Claude Code's) may not be able to push. It is meant for light-guard projects; otherwise
let the coordinator push.

## Manual configuration

For setups the wizard does not make, edit the TOML; the annotated examples list every key:

- **Several logins of one agent:** a second account with its own `config_dir` (log in once
  with `CLAUDE_CONFIG_DIR=~/.claude-work claude` or `CODEX_HOME=~/.codex-work codex
  login`). For OpenCode, `config_dir` is an account *root* (`<root>/config/opencode/...`),
  not the OpenCode config dir. See [examples/registry.toml](../examples/registry.toml).
- **Custom models:** any model id the harness accepts (`model = "gpt-5.5"`,
  `model = "provider/model"` for OpenCode), with `env` / `env_passthrough` for API keys
  ([QUOTA.md](QUOTA.md) for LiteLLM).
- **Strict guard:** `guard = "strict"` in a project ([ADAPTERS.md](ADAPTERS.md), "Threat
  model"); see [examples/projects/myapp.toml](../examples/projects/myapp.toml).

## Checking the setup

`otag doctor` checks the registry, the database, tmux, git, the agent CLIs, the accounts,
the daemon and its environment. Every warning and failure comes with a `fix:` line (in
`--json`, a `fix` field) naming the command to run, for example `otag init` on a fresh
machine, `otag project add` without projects, `otag daemon install` when the daemon is
not running, or how to install a missing agent CLI.
