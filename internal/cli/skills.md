---
name: piggery
description: Use when you are an agent in a piggery team (or a solo session) and need to message others, manage workers, or write a team template the Human asked for.
---

# piggery for agents

piggery lets agents message each other and work in teams with roles; every message and spawn
passes the team's rules (the gate). In pi you use the `piggery_*` tools, in Claude Code and Codex the
`mcp__piggery__*` tools (new mail is announced in Claude by a message that looks like it comes
from another Claude session, in Codex by a "[piggery] wake #N" prompt: both are piggery); the
shell commands below are the same thing. Messages
are `#N`; people and teams are names.

## Messages

```sh
piggery who                                   # your team, other teams (name, gate), solos
piggery send <to> "text" [--reply-to #N] [--expects-reply] [--kind K]
piggery inbox                                 # new mail (read only)
piggery inbox --view board                    # the team board
piggery completion --batch N                  # ack a batch you pulled with inbox --batch N
piggery tool <name> key=value ...             # a declarative tool of your role (e.g. done summary=...)
```

- `to`: a teammate's name; another team's name (reaches its gate; only gates write between
  teams); a solo's name; `board` (pins: `--op replace|remove --target #N`); `notify` (one way,
  to the Human; nothing comes back).
- Reply with `--reply-to` the `#N` you answer; `--expects-reply` only when you need an answer.
  `kind` is a free label for the receiver; piggery never reads it.
- After sending, end your turn: mail wakes you. Do not poll.

## Workers and teams

```sh
piggery agent spawn --role R --name N "task"  # if your role may spawn R; its reply comes as mail
                                              # --cwd DIR: run it there (your role needs can_set_cwd;
                                              # inside the team root or a git worktree of its repo)
piggery agent resume <worker> ["task"]        # a task comes to it as at spawn
piggery agent stop|resume|tail <worker>       # tail: read its log before nudging or resuming it
piggery agent templates                       # the templates a team can be founded from
```

Only when the Human asks: `found` (start a team from a template, rooted at your directory; you
become its gate), `admit` (take a solo session at your team's root into a role you may spawn),
`close` (the gate closes its own team; you become solo), `reopen` (a solo at a closed team's root
opens it again and becomes its gate). In pi these are `piggery_agent` actions.

## Writing a template (when the Human asks)

A template is `~/.piggery/templates/<name>/manifest.yaml` plus the prompt files it names,
relative to that directory. To start from a built-in, copy its directory under a new name and
set `model:` to that name. You do not bring it up: the Human does, or asks a session to found it.

**Manifest fields** (only these exist):

- `model` (required): the template's name, also the default team name. `summary`: one line, when
  to use it. `auto_join_role`: the founder's role (else the only role).
- `roles.<role>` (at least one): `description` (one line); `instructions_file` or `instructions`;
  `tools` (built-in `send`, `inbox`, `who`, `agent`, plus declared ones; without `send` a role
  writes only through its declared tools); `can_spawn: [role]`; `can_pin: true` (board, needs
  `send`); `can_set_cwd: true` (may spawn a worker in another directory); `spawn: {model:
  provider/id, thinking: level, harness: pi|claude|codex}` (omit to inherit the founding session's).
- `routing`: `{from: role, to: role, allow: true|false, cc: [role]}`; the first rule matching
  (sender's role, recipient's role) decides, none means denied; `to: notify` lets a role notify
  the Human; `cc` copies other members of those roles. Mail between teams ignores routing.
- `tools.<name>`: `{description, params: {field: string}, send: {kind: K, to: spawned_by|reports_to|<role>}}`,
  one message of a fixed kind; params are required strings; it goes through routing.
- `limits`: `depth` and `concurrency` (required once a role can spawn),
  `messages_per_participant_per_minute`, `messages_per_thread`, `max_hops`, `max_respawn_per_hour`.
- `timers`: `{on: role, silent_for: 20m, notify: reports_to|notify|<role>}`: one notice when that
  role works with no turn end for that long.

**Prompts.** Name tools only as `{tool:send}`, `{tool:agent}`, `{tool:done}`… (each harness names them
differently: `piggery_send` in pi, `mcp__piggery__send` in Claude and Codex); a placeholder that
names no tool is refused. A role is a responsibility, what it may do and where it escalates, not a
persona. Stay neutral about the kind of work: a task is a result, its bounds, and its check. Spell
out the lifecycle: give the next task to a free worker instead of spawning; end the turn after
sending; when a worker goes silent, read its tail, then nudge or resume it; stop workers when done.

**A small complete template** (`~/.piggery/templates/brief/`):

```yaml
# manifest.yaml
model: brief
summary: A lead splits a question into parts; helpers each answer one part and report back.
auto_join_role: lead
roles:
  lead:
    description: Splits the question, gives each part to a helper, and writes the answer.
    instructions_file: prompts/lead.md
    tools: [send, inbox, who, agent]
    can_spawn: [helper]
  helper:
    description: Answers one part at a time, with sources, and reports to the lead.
    instructions_file: prompts/helper.md
    tools: [inbox, who, report]          # no free send: it answers through report
routing:
  - {from: lead, to: helper, allow: true}
  - {from: helper, to: lead, allow: true}
  - {from: lead, to: notify, allow: true}
tools:
  report:
    description: Send your answer to the lead who spawned you.
    params: {answer: string}
    send: {kind: report, to: spawned_by}
limits: {depth: 2, concurrency: 3}
timers:
  - {on: helper, silent_for: 30m, notify: reports_to}
```

`prompts/lead.md`:

```
You are the lead. Split the Human's question into parts that can be answered separately.
Give each part to a helper: {tool:send} kind `task` to a free helper, or {tool:agent} action
`spawn` (role helper) when none is free and the part can run at the same time. Then end your
turn: each answer comes as mail (kind `report`). Check each answer against its sources; send
back what is missing. When every part is answered, stop the helpers and give the Human the answer.
```

`prompts/helper.md`:

```
You are a helper. The lead gives you one part at a time by mail. Answer it with sources, say
what you could not check, and send it with {tool:report}. Then end your turn and wait for the
next part. Do not start or message other agents.
```
