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
piggery send <to> "text" [--reply-to #N] [--kind K]
piggery inbox                                 # new mail (read only)
piggery inbox --view board                    # the team board
piggery completion --batch N                  # ack a batch you pulled with inbox --batch N
```

- `to`: a teammate's name; another team's name (reaches its gate; only gates write between
  teams); a solo's name; `board` (pins: `--op replace|remove --target #N`). `notify` is piggery's own
  channel to the Human; agents cannot send to it.
- `--op assign` (to a member that reports to you): the mail becomes that member's current task,
  shown in `piggery top`; a later assign replaces it. Put a short title on the first line. A task
  given by `agent spawn|resume` is one already.
- Reply with `--reply-to` the `#N` you answer.
  `kind` is a free label for the receiver; piggery never reads it.
- After sending, end your turn: mail wakes you. Do not poll.

## Workers and teams

```sh
piggery agent spawn --role R --name N "task"  # if your role may spawn R; its reply comes as mail
                                              # --cwd DIR: run it there (your role needs can_set_cwd;
                                              # inside the team root or a git worktree of its repo)
piggery agent spawn --template T [--name N] [--cwd DIR] "task"   # a taskforce, see below (no --role)
piggery agent close [<team>]                  # a taskforce you called up; with no team, your own (see below)
piggery agent resume <worker> ["task"]        # a task comes to it as at spawn
piggery agent stop|resume|tail <worker>       # tail: read its log before nudging or resuming it
piggery agent templates                       # the templates a team can be founded from
```

**A taskforce** is a temporary team for a one-off job: a second opinion, a review, a hard question.
You may call one on your own judgment, as a main session calls a subagent, when you are a solo
session or a team's gate (a Peer cannot, and a taskforce never calls another). `spawn --template T`
builds a team from a template that has a `taskforce:` block (`piggery agent templates` marks them)
in your directory (or `--cwd DIR`: relative to yours or absolute, inside your root, a git worktree of its repo, or an allowed root) and starts its chair as a headless worker, your task being its first mail. You stay
where you are: the chair reports to you by mail (write to it by the team name `spawn` printed) and
stays for follow-ups; you close the taskforce when you are done with `agent close <team>` (it never closes itself).
It only reads unless the task says it may edit.

Only when the Human asks: `found` (start a team from a template, rooted at your directory; you
become its gate and stay in it), `admit` (take a solo session at your team's root into a role you may
spawn), `close` with no team (the gate closes its own team; you become solo), `reopen` (a solo at a
closed team's root opens it again and becomes its gate). In pi these are `piggery_agent` actions.

**Changing a worker's model** (only when the Human asks you to; it is an admin command, so it
needs `--admin` in your shell):

```sh
piggery --admin model <worker> <provider/model> [--thinking L]   # model, and optionally the level
piggery --admin model <worker> --thinking L                      # only the thinking level
piggery ps                                                       # check: its model column
```

It works on headless workers only (a session's model is chosen in that session). A running worker
switches from its next turn and keeps its context; a stopped one gets it when resumed. Name the
model as the worker's harness does (pi and omp: `provider/model`); a model or level the harness does
not run is refused and nothing changes. Tell the Human what `ps` shows after.

## Writing a template (when the Human asks)

A template is `~/.piggery/templates/<name>/manifest.yaml` plus the prompt files it names,
relative to that directory. To start from a built-in, copy its directory under a new name and
set `template:` to that name. You do not bring it up: the Human does, or asks a session to found it.

**Manifest fields** (only these exist; each with its default is in
[docs/reference.md](https://github.com/sting8k/piggery/blob/main/docs/reference.md#manifest)):
`template` (required, the name), `summary`, `auto_join_role`, `roles.<role>` (`instructions` or
`instructions_file`, `tools`, `can_spawn`, `can_pin`, `can_set_cwd`, `spawn`), `routing`, `limits`, `timers`, `taskforce` (`idle_for`: a template with this block can also be called up
with `spawn --template`; keep its keys in that block, because `timers` refuses unknown keys in older binaries).
The tools are `send`, `inbox`, `who`, `agent` and no others. The first routing rule matching (sender's
role, recipient's role) decides and none means denied; mail
between teams ignores routing. A role that can spawn needs `limits.depth` and `limits.concurrency`.

**Prompts.** Name tools only as `{tool:send}`, `{tool:agent}`, `{tool:inbox}`, `{tool:who}` (each harness names them
differently: `piggery_send` in pi, `mcp__piggery__send` in Claude and Codex); a placeholder that
names no tool is refused. A handback, a question or a report is a `send` with a `kind` (e.g.
`handback`, `ask`, `report`) to the right name; routing is the fence, the prompt says who sends what. A role is a responsibility, what it may do and where it escalates, not a
persona. Stay neutral about the kind of work: a task is a result, its bounds, and its check. Spell
out the lifecycle: give the next task to a free worker instead of spawning; end the turn after
sending; when a worker goes silent, read its tail, then nudge or resume it; stop workers when done.

**The Human's own rules** are not in a template: `prompts:` in `~/.piggery/config.yaml` (see the reference) appends a file of theirs to the cards of the roles it names, so do not copy such rules into a template's prompts.

**A small complete template** (`~/.piggery/templates/brief/`):

```yaml
# manifest.yaml
template: brief
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
    tools: [send, inbox, who]            # routing lets it write only to the lead
routing:
  - {from: lead, to: helper, allow: true}
  - {from: helper, to: lead, allow: true}
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
what you could not check, and send it to the lead with {tool:send} kind `report`. Then end your turn and wait for the
next part. Do not start or message other agents.
```
