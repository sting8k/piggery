# Changelog

## v0.12.0 - 2026-10-27

`send` takes an optional `cc`, and the `inbox` tool can look back at your own mail.

Upgrade: `piggery update`, then restart open pi, omp, dsh, opencode, Claude and Codex sessions (the
tool list is read at start). The integrations move to pi 24, omp 23, dsh 24 and opencode 19.

### New

- **`inbox` looks back** (the tool only, no CLI): `view: recent` lists your last 10 messages, received
  and sent, newest first, each with `#N`, when, who, kind, `re #N`, whether it is unread and a short
  preview; `before: #N` pages back. `view: message` with `id: #N` shows one in full and whether it is
  unread. Read-only: it records no delivery and acks nothing, so mail you have not handled is still
  given as new. Mail held for you is not shown; your own held mail is, marked.
- **`cc` on `send`** (`piggery send --cc a,b`). Each name is checked as a send to it would be
  (routing, visibility). A name that would be refused, is in another team, or is unknown gets no
  copy and is named in the result; the send itself still goes. Copies carry no rate cost. The
  recipient of the original and of each copy now sees who else got a copy: `cc="lookout (lookout),
  kim (peer)"`, routing `cc` copies included.

## v0.11.2 - 2026-10-25

A pi, omp, dsh or opencode session that changes participant in the middle of a run now shows as working.

Upgrade: `piggery update`, then restart open pi, omp, dsh and opencode sessions. Their integrations
move to pi 23, omp 22, dsh 23 and opencode 18. Claude and Codex are unchanged.

### Fixed

- A session that founded a team, reopened one, or closed its own team (and so became solo) in the
  middle of a run showed the new participant idle in `top` and `ps`, with `last turn -`, until its
  next run. The run now goes on as the new participant: the daemon sees its turn start, its tool
  boundaries and its end.

## v0.11.1 - 2026-10-25

`piggery skills` now teaches the `every` timer, and the guide and reference cover what v0.11.0 left out.

Upgrade: `piggery update`. Nothing else changes: the binary's behaviour, the database and every
integration are as in v0.11.0, so no session needs a restart.

### Changed

- **`piggery skills` describes both timer kinds.** An agent that writes a template now learns
  `{on: <role>, every: <duration>}` (a role told who is working, once a period, while others work)
  next to `working_for`, so it can give a role the job of looking at the team on a schedule.
- **The guide shows an `every` timer** in its example template.
- **The reference lists two more `piggery check` warnings:** `can_pin: true` (the key is ignored
  since the board was removed) and a timer that still says `silent_for`.
- **The v0.11.0 notes below are laid out by kind** (upgrade steps, New, Changed, Removed, Fixed);
  what they say is unchanged.

## v0.11.0 - 2026-10-25

Upgrading is two commands, model choices live in `config.yaml`, and an idle daemon stays idle.

### Upgrading from v0.10

1. `piggery update`
2. `piggery setup --outdated` (the v0.10 `update` does not run this step; from v0.11 on, `update`
   runs it for you)

Then restart open pi, omp, dsh and opencode sessions: their integrations move to pi 22, omp 21,
dsh 22 and opencode 17. Claude and Codex are unchanged. On first start the database moves to
schema v30, after a backup. Templates in your home that set `spawn.harness|model|thinking` have
those keys moved into `roles:` in `config.yaml`, so every role keeps the model it had (copies in
`templates/.backup/migrate-*`).

### New

- **`piggery setup --outdated`**: the second step of an upgrade. It updates every outdated
  integration, then lists the built-in templates and shared prompts you edited and asks which to
  replace (`d` shows the diff; without a terminal, `--replace <names>|all|none`). A replaced file
  is backed up first.
- **`piggery template update [<name>] [--force]`**: brings a built-in template in
  `~/.piggery/templates` to this version. One you edited is left alone unless `--force`, which
  backs it up first. `template list` marks it `built-in, edited`.
- **`roles:` in `config.yaml`**: harness, model and thinking per role, for example
  `"lead-peer/peer": {harness: codex, thinking: high}`. Keys are `<template>/<role>`,
  `<template>/*`, `<role>` or `*`; for each field the most specific entry wins. An entry piggery
  cannot use is skipped with a warning.
- **`inbox` waits for mail**: `piggery inbox --wait N`, or the tool's `wait`, waits up to 50 s and
  returns as soon as mail comes.
- **`every` timer**: `{on: <role>, every: <duration>}` (at least `1m`) wakes that role once per
  period with `Working now: <names>` while other members work. An idle team gets nothing.
- **`pig`**: a short name for `piggery`, made by the installers only when nothing else is called
  `pig`.
- **`piggery gc` trims worker logs** in open teams: for a worker gone over 7 days, older runs are
  deleted and the last keeps its final 1000 records (at most 5 MB).

### Changed

- **Built-in templates update as a whole.** One you never touched is replaced by the new version;
  one you edited in any way is kept as it is. A template is never half old, half new.
- **`spawn.harness|model|thinking` in a manifest is deprecated** in favour of `roles:`. It is still
  read, below `roles:`, and built-in templates no longer set it.
- **The timer `silent_for` is now `working_for`** (working without ending a turn, not idle).
  Templates in your home are renamed for you; `silent_for` still loads.
- **Built-in prompts.** In `lead-peer` and `slp` a Peer works with the Lead, not under it, and
  accepting a handback needs no reply of its own. `slp`'s Supervisor keeps a journal and advises.
  The taskforce card says when calling one pays. `general-policy` adds three rules: a number that
  gates or steers the work is a proxy (name its source, measure a baseline, freeze the criteria,
  pair it with an opposing check); report finished work before starting more; answer whoever waits
  on you. A copy you edited is kept, and `setup --outdated` offers the new one.
- **pi: mail is acked sooner.** Mail steered into a running turn counts as read when the model reply
  that read it ends, not when the whole turn ends, so a long tool call no longer holds it.
- **pi: a session that stops driving a participant says why** (the daemon refused it, another
  session owns it, the team closed) and how to get back.
- **`config.yaml` errors name the line**: an unknown key names its line, where it sits and the
  keys known there.
- **`piggery ps` is cheaper**: a handful of queries instead of several per team and member.

### Removed

- **The team board (pins)**: `send` to `board`, `--op replace|remove`, `--target`, `inbox --view`,
  `piggery board` and `can_pin`. No built-in template used it. A template with `can_pin: true`
  still loads, and `piggery check` says the key is ignored. Pinned mail stays in the database;
  schema v30 drops the `pins` view.

### Fixed

- An idle daemon woke the CPU about 10 times a second per open team. It now reads only teams where
  someone works.
- The `settled` notice fired while a taskforce the gate called was still working.
- `piggery tail` of a worker that went on as an interactive session showed its old driver log
  instead of the session's transcript.
- pi: two piggery tools called at once on a fresh session left it unable to join.

## v0.10.4 - 2026-10-19

`top`'s sidebar is now the selected row's Tail, drawn as turns, and its width follows a drag.

- Change: `top`'s sidebar has no Overview tab any more (it repeated the row) and `t` is gone. It is
  the selected row's Tail; on a team's line, its gate's (`Tail · lark (gate)`). Above a rule, a head
  says what the row does not: the task and a newer mail, the model where it can be changed (a click
  on it, or `M`, opens the picker as before), who spawned it and whom it reports to, and when a
  closed team closed. The log below is drawn as turns, a blank line between two: what came in at the
  left edge (a prompt `›`, mail `from lark …`), what the participant did two cells in. A run of tool
  calls is one line (`• 4 tools · bash ×3, edit`); the latest run, still going, is a tree of its
  last calls; a result shows only when it failed; mail sent is one line (`to lark …`), at the left
  edge like mail in; an older turn keeps what came in, its calls and what it gave back. This works
  for interactive sessions' transcripts as well as workers' logs. `piggery tail` still prints every
  line.
- New: drag the line between the list and the sidebar in `top` to resize the sidebar; the width
  is kept for the next `top` (#34). `top` also draws faster: its colours are worked out once, not
  on every frame. Thanks to @sting-lon.
- Change: in `piggery tail`, a call to piggery's send tool shows whom it went to and its body
  (`> piggery_send lead #3 done`) instead of cut `body=… to=…`, and `read`, `edit` and `write`
  show their path also when the harness names it `file_path` (Claude) or passes a list of `paths`.

## v0.10.3 - 2026-10-15

On Windows, pi workers started through pi's own launcher no longer go gone about ten seconds after they start.

- Fix (Windows): a pi worker went gone about ten seconds after it was spawned, with its mail
  unread and an empty tail, when pi's `pi.cmd` names its script `"%~dp0pi-launcher.js"`, with no
  backslash after `%~dp0` (pi's launcher writes it so); piggery did not see through such a shim, so the worker ran under
  `cmd.exe`, one process too deep, and the daemon refused it as a harness nested in a worker
  (#32). Thanks to @sting-lon for the report, the diagnosis and the fix.

## v0.10.2 - 2026-10-12

A Codex worker is woken for mail that came as its turn ended, and cleanup removes closed teams it used to skip.

- Fix: a Codex worker could stay idle with unread mail. A mail that came in the moment between
  the end of its turn and Codex reporting that turn over did not wake it, and once a turn had been
  kept going 8 times for mail that came during it, this happened every time; the mail waited for
  a newer one. The worker
  is now woken once the turn is over (#26).
- Fix: cleanup never removed a closed team in which a member had been given mail on an earlier
  run of its process (`piggery gc` reported that the team had changed since the archive was
  written, every time). Such teams are now removed.

## v0.10.1 - 2026-10-12

Mail is no longer lost or held back after an interrupted, failed or crashed turn on any harness, cleanup keeps mail its recipients have not read, a team member can no longer stop a person's session, and Claude Code sessions keep their participant across `/clear`, `/branch` and forks.

Before you upgrade:

- **A template with a typo inside a role, its `spawn` block or a routing rule no longer founds a
  team.** An unknown key there, a role name the manifest does not have, or a limit below 1 is
  refused with the reason (`none` still switches a limit off). An unknown top-level key is ignored
  with a warning, so a template written for a newer piggery still runs. Teams already founded keep
  their manifest as it was. Run `piggery check` to see what a template of yours would hit.

After upgrading, run `piggery restart`: the daemon updates the integrations it installed to pi 14,
omp 14, dsh 15 and opencode 12. Then restart the sessions that were open, or run `/reload` in pi.

Mail:

- Fix: in omp and dsh, mail that arrived at the very last step of a reply was taken as read before
  the model saw it. If the next step or run was stopped (Esc, `piggery abort`, a failure), that mail
  was lost. It is now taken as read only once a model turn that shows it ends, and comes again
  otherwise (#15).
- Fix: a Claude Code turn interrupted with Esc, at a permission prompt or by an SDK client (Paseo)
  held back wakes and mail until the next prompt, and a permission prompt left with Esc kept the
  session shown as awaiting permission. Claude sends no hook for an interrupt; piggery now reads
  Claude's own session status and ends such a turn as interrupted. As after any interrupt, nothing
  starts on its own: the next mail, or your next prompt, brings the waiting mail (#13, #14).
- Fix: a member that went gone in the middle of a turn (its process exited, its session ended, or
  the daemon restarted) was never woken by mail; the mail waited until someone resumed it. Mail now
  wakes it as it wakes any gone member (#18). A headless worker whose process ends on its own (a
  crash, not `agent stop`, `team down` or a daemon shutdown) is also woken at once for the mail it
  had not finished, its task included: one automatic restart per mail.
- Fix: in opencode, a run that failed before the model answered (for example, a model that does not
  resolve) with piggery mail in it was nudged again and again and never ended. It now ends failed:
  the mail is not taken as read and comes again with the next mail (#27).
- Fix: cleanup (`piggery gc`, and the daemon's own) deleted unread mail that a closed team or gone
  solo had sent to someone outside it, and once that person had been given such mail, cleanup of
  the team or solo failed on every run. Mail sent out now stays with its recipient until it is
  read; the team or solo is kept until then (#17).
- Fix: the `settled` notice ("nobody is working and no mail waits") left out mail waiting for a
  member that had gone and could not be woken (a limit refused it), so it could tell you a team was
  done while mail sat unread (#21).
- Fix: `cc` copies and timer notices sent to a role still went to a member that had left the team,
  where nobody reads them (#23).

Teams and sessions:

- Fix: `agent stop` and `agent resume` treated a session admitted into a team as a headless worker.
  A member could stop a session a person runs, and one mail later the daemon ran that conversation
  a second time in the background, while the person's own session was refused. Both now refuse a
  session, as `piggery kill` and `piggery resume` already did (#19).
- Fix: when a session reopened the team it had been the gate of, the taskforce it had called in the
  meantime was closed as orphaned, its workers stopped mid-task. The taskforce now stays with the
  session (#22).
- Fix: after a Claude Code session's row moved (its gate closed its own team, `piggery team down`,
  or an in-process resume), its piggery tools stayed bound to the old one: mail no longer woke it,
  and tool calls could fail with `run.stale`. A gate closing its own team was also told
  `refused (host.unknown)` although the close succeeded (#16).
- Fix: Claude Code's session identity. After `/clear`, a reconnect of piggery's MCP server (a
  daemon restart is enough) put the old session id back, so a wake by mail resumed the conversation
  from before `/clear` (#24). `/branch` turned the session into a new solo, and its participant went
  gone with its mail (#25). A session forked outside its process (Paseo's Rewind) became a new solo
  with an empty inbox (#13).
- Fix: a manifest with a typo inside a role, a `spawn` block or a routing rule founded a team, the
  typo silently became a default, and `piggery check` said "No problems" (#20).
- Fix: a worker whose first start failed before its harness made a session (a model or provider
  that did not resolve) could never be brought back: `agent resume` asked the harness for a session
  that did not exist, although the error said to fix the cause and resume. It now starts a fresh
  session under the same id. This was true of every harness but pi.
- Fix: a session of yours that mail had woken once, and that went gone again, was woken the next
  time into the conversation from before your last `/clear`. It now always resumes the newest one.
- dsh 0.2.0-rc.2 and opencode 1.19.0 are tested. dsh rc.2 no longer reads `~/.dsh/settings.yaml`:
  a custom provider is a row of `$DSH_HOME/cordis.patch.yml` (see the reference's dsh notes).

Thanks to @Patruxs for the reports, each with a test that reproduces it.

## v0.10.0 - 2026-10-12

piggery runs on Windows (experimental), a team or solo can close its gate to other teams, and `lead-peer` replaces `p2p` as the default template.

Before you upgrade:

- **`p2p` is no longer built in; `lead-peer` is the default.** A session asked to set up a team
  without a template now founds a `lead-peer` team and becomes its lead, `piggery template new`
  copies `lead-peer`, and a solo takes its limits from it (the same numbers as before). Teams
  already founded from `p2p` keep running: a team keeps the template it started with. The copy in
  `~/.piggery/templates/p2p` is deleted when the daemon starts, unless you edited it: an edited
  file stays, with its directory, as your own template.
- **piggery's own Paseo plugin is gone, and so is `piggery setup paseo`.** In Paseo, use
  [PigMonitor](https://github.com/Patruxs/PigMonitor), which does more. To remove the old plugin:
  `paseo plugin remove piggery`, then delete `~/.piggery/paseo`. `ps --view`, `ps --json`,
  `tail --view` and `--no-start`, which such a plugin reads, are unchanged.

After upgrading, run `piggery restart`. The daemon updates the integrations it installed: pi to 13,
omp to 13, dsh to 14 and opencode to 10. Then restart the sessions that were open, or run `/reload`
in pi. Claude Code and Codex need nothing. The database moves to schema 28 at the first start (a
copy of the old one is kept in `~/.piggery/backups/`).

Closed gate:

- **A team or a solo session can close its gate to other teams.** Others no longer see it in
  `who`, and a send to it (by id, by name or as a reply) fails like an unknown name; it sees none
  of them and cannot send to them. Unread outside mail is held while the gate is closed, neither
  delivered nor acked nor deleted, and arrives when it opens. Nothing changes inside a team, and a
  caller and its taskforce always reach each other.
- **`gate: open|closed` in `config.yaml`** sets the gate of a new solo (default `open`). A solo,
  or a team's gate, closes or opens it when you ask with `agent action=gate_close` /
  `gate_open`. A team it founds or reopens, a taskforce it calls, and a session left solo when its
  team closes start with the same state.
- **`who`, `ps` and `top` show `[gate closed]`**; `ps --json` has `gate_closed`.

Windows (experimental, amd64 and arm64):

- **The daemon, workers and harnesses run natively.** The daemon listens on a named pipe that only
  your user can open (`piggery.pipe` in `~/.piggery` holds its name). Each worker runs in a Job
  Object, so a worker, the `.cmd` shim it was started through and its children end together, and a
  daemon that restarts after a crash finds and ends the ones it left.
- **Install and update.** Releases carry `piggery-windows-<arch>.exe`. `install.ps1` installs it
  (`irm https://raw.githubusercontent.com/sting8k/piggery/main/install.ps1 | iex`), and `piggery
  update` works there: the binary it replaces, which may be running, is kept beside the new one as
  `piggery.exe.old` until the next update.
- **`piggery setup claude` and `setup codex` write hook commands each one runs on Windows.** Claude
  Code gets piggery's path with forward slashes (it does not run a hook whose path has
  backslashes). Codex gets the path's 8.3 short name when the path has a space, since some Codex
  versions run no hook command with a quote in it.
- **`piggery setup notify add desktop` and `add ntfy:<topic>`** write PowerShell hooks on Windows (a
  toast, a push through ntfy.sh) that need nothing installed. `herdr` has no Windows script. Your
  own hooks in `notify.d` may be `.exe`, `.cmd`, `.bat` or `.ps1`; a `.sh` hook is skipped there.
- **A harness installed with npm or pnpm starts as a worker.** Its `.cmd` shim is seen through,
  and the script runs with the interpreter the shim names (bun for omp), not always node.
- **A worker's agent directory shares yours without Developer Mode.** Where a symlink cannot be
  made, pi's and omp's entries become junctions and hard links, not a copy of the whole directory
  at each spawn: a worker's sessions and a login it refreshed are your own.
- **A Claude worker starts whatever the length of its role card.** Windows limits a command line
  to 32,767 characters, which a card carrying your own `prompts` rules can pass; the card goes to
  Claude Code in a file of the run's directory there (`--append-system-prompt-file`).

Also:

- Built with Go 1.26.9, which fixes GO-2026-6617 in `net/http`.

Thanks to @dinhhung893, whose piggery-winport the Windows port learned from; to @KiraKas-Tr, who
tested it on a real Windows machine and found what CI could not; to @haohao3k, whose report and
reproduction (#11) led to the closed gate; and to @Patruxs for PigMonitor.

## v0.9.1 - 2026-10-07

A pi session no longer misses mail that arrives just as it finishes a reply.

After upgrading, run `piggery restart`: the daemon updates the pi integration it installed to 9
(omp to 9 and dsh to 10, which share its adapter file and behave as before). Then restart the pi
sessions that were open, or run `/reload` in them.

- Fix: mail that reached a pi session at the very end of a reply was shown to the model, and pi
  went on working with it, but piggery took the turn as finished. Mail sent after that waited,
  unannounced, until pi stopped for good or the model checked its inbox. The turn now stays open
  while pi works on that mail, so later mail is steered in as usual and nothing is acked before
  the model has read it.
- Fix: a stopped worker no longer leaves an empty directory under `~/.piggery/run/`. Existing
  empty ones go when `piggery gc` removes their worker.

## v0.9.0 - 2026-10-06

A session can call up a taskforce for one job, a team's gate stays with its founder, and `lead-peer`, `dual-lens` and `advisor` join the built-in templates.

Before you upgrade:

- **`supervisor-executor` is no longer built in: `lead-peer` replaces it** (roles `lead` and
  `peer`). A copy in `~/.piggery/templates` stays as your own template and teams made from it keep
  running, but it gets no updates, and `prompts` entries in `config.yaml` that name its roles apply
  to it only; add entries for `lead-peer/...` to give those rules to the new template. `slp`
  changed too: its Supervisor no longer writes to a Peer directly or pins to the board. An `slp` you
  never edited is refreshed when the daemon restarts; a running team keeps the template it started
  with.
- **A team's gate no longer passes to a peer while its founder is away.** The gate is the member
  that founded the team (or reopened it), stored on the team: closing its session, a crash or a
  daemon restart leave it the gate. Mail for it waits in its inbox until it is back, a send to the
  team from another team is queued instead of refused, and no peer can close the team or take its
  cross-team mail meanwhile. The gate moves only when it leaves the team (the next member that
  joined takes over, `gate_moved` in `piggery log`) or the team is reopened. A worker is never
  the gate. Existing teams get one when the daemon starts: the member that joined first, a session
  before a worker.
- **Mail now wakes a member that is gone.** In an open team, new mail for a member whose session
  you closed starts that session again as a headless worker, which costs a model run; `ps` shows it
  as a worker. The limits of `piggery resume` apply (`max_respawn_per_hour`, `concurrency`,
  directories), the gate is never woken, and a worker that stops with the mail unread is woken
  again only by newer mail. If you open the session yourself, the worker is stopped (about 2.5
  seconds at most, a turn in progress can be cut) and the session is yours again. Before, such mail
  waited for `piggery resume`.
- **The database moves from schema 22 to 27** at the first start. piggery copies it to
  `~/.piggery/backups/pre-v22-27-<date>.db` first (the three newest copies are kept). v0.8.0 does
  not open a newer database: to go back, stop the daemon, install v0.8.0 and put that copy in
  place of `piggery.db`, deleting `piggery.db-wal` and `piggery.db-shm` beside it. Whatever happened
  after the upgrade is lost.
- **Open sessions keep the old extension until you restart them.** The daemon updates the pi, omp,
  dsh and opencode extensions at its first start (versions below), so `piggery setup` is not needed
  for them. Claude Code, Codex and Paseo installs did not change.

In this release:

- Integration versions since v0.8.0: pi 3 to 8, omp 3 to 8, dsh 4 to 9, opencode 1 to 6 (claude 1,
  codex 1 and paseo 3 are the same).
- A solo session or a team's gate can call up a taskforce: `spawn` with a `template` and a task
  builds a temporary team from a template that has a `taskforce:` block, in the caller's directory
  (named after the template unless you give a name), and starts its chair as a headless worker. The
  chair sends the result back as mail and stays for follow-ups; only the caller closes the
  taskforce, from outside (a close from inside it, the chair's included, is refused). If every
  member stays idle for `taskforce.idle_for` (20 minutes in the built-ins) you get one notice for
  that idle stretch. A taskforce closes with its caller, and also when its chair stops (gone, left or
  parked): its other members are stopped as with `team down` and the caller gets one notice, "Taskforce X
  closed: its chair <name> stopped." A closed taskforce is
  never reopened (call its template up again), and a taskforce never calls another. Its caller's
  `limits.concurrency` counts the taskforces it has open; the taskforce's own `limits.depth` counts
  from its chair. `spawn template=` also takes a `cwd`, the
  taskforce's directory (and its chair's): relative to the caller's or absolute, and inside the
  caller's root (a gate's team root, a solo's own directory), a git worktree of that repo, or a
  `spawn.allowed_roots` entry, else it is refused; no `can_set_cwd` is needed, and without it the
  taskforce works in the caller's directory. In a shell:
  `piggery agent spawn --template T [--name N] [--cwd D] <task>` and `piggery agent close [<team>]`. A key
  piggery does not know inside a `taskforce:` block is ignored, and `piggery check` names it.
- `council`, `dual-lens` and `advisor` are taskforce templates. The prompts of `council` and
  `dual-lens` work both ways: founded by you as a long-lived team, or called up by a session (they
  answer the caller, and only read unless the task says they may edit). The leads of `lead-peer`
  and `slp` call a taskforce for a one-off second opinion instead of spawning a Peer. The card of a
  solo and of a gate lists the taskforces it can call and says when to call one: a second opinion, a
  review that matters, a decision or approach it is stuck on, or a short bounded job, not work it can
  do itself; each taskforce's summary says what it does and whether it edits.
- The `agent` tool of pi, omp, dsh, opencode, Claude Code and Codex takes `template` on `spawn` and
  `team` on `close`.
- `piggery template list` has a `taskforce` column that marks the templates a session can call up.
  `ps` and `top` list a taskforce right under its caller, a step in, with a `taskforce` label in
  place of `team` and the caller's name (`caller=` in `ps`, `for <name>` in `top`); `ps --json` has
  `parent` and `parent_id` on such a team and `under` on its unit in `projects`, and `ps --view` has `under` and `depth` on its block and
  `taskforce` and `caller` on its head. A closed taskforce is not in the main list or under its caller
  any more; it stays in the Closed tab until gc.
- `top`'s tabs are projects (directories), not teams: All, then one tab per project, then Closed.
  Projects with someone alive come before all-gone ones. A tab is named by its directory's last
  name, with parent names added until two that clash differ (`a/api`, `b/api`), and a bar too wide
  for the window slides, with `‹ +N` and `+N ›` for the tabs left out. A taskforce has no tab of its
  own; it sits under its caller. All lists only what is alive: no team whose members are all gone, no
  gone solo and no closed team (the one-hour grace for a closed team is gone; closed teams are in
  Closed). A team with some members alive keeps its folded `N members ✗ gone` row. `ps`, `ps --json`
  and `ps --view` are unchanged.
- The built-in `lead-peer` template: the Lead owns the plan, talks to you directly, and judges each
  result. A Peer may know as much as the Lead: it follows the brief by default and speaks up only
  with evidence that the plan is off, and the Lead weighs that evidence, not who said it. Both
  prompts are much shorter.
- The `slp` prompts are much shorter, with the same Lead and Peer rules. The Supervisor writes to
  a Lead as you would, passing on your own words where they carry the request; it talks only to
  Leads and watches a lane through its Lead's log. In both templates a Lead writes to its Peers as
  a person would, and a Peer's prompt speaks only of its work and the one who gives it.
- New template `dual-lens`, written as a taskforce: its chair takes one hard technical question or
  review, sends the same neutral brief to two lenses, accepts where they overlap, hands each lens
  the other's argument where they conflict, decides, and sends the answer to whoever asked. It only
  reads unless the task says it may edit. Set `lens-a` and `lens-b` to two different model
  families; left at `inherit`, both run the same model.
- New template `advisor`, a taskforce of one: it gives read-only advice on one hard decision or a
  stalled approach (a recommendation, the evidence, the strongest downside, and a check that would
  prove it wrong), answers by mail, and stays for follow-ups until you close it. Call it with
  `spawn template=advisor`.
- piggery now ships a shared prompt, `~/.piggery/rules/general-policy.md` (own the outcome,
  question a mechanism before patching it, keep tests and docs minimal), for every role of
  `lead-peer` and `slp`. `setup` or the daemon's start writes the
  file and adds its entry to `prompts` in `config.yaml` once, keeping your entries. Remove the
  entry or the file and it stays removed; a file you edited is never overwritten.
- `top` and `ps` show the template a team was founded from in brackets after its name
  (`piggery [lead-peer]`; not when it is the team's name); `ps --json` has it as `teams[].template`.
  Members are listed in the order they joined the team (`ps`, `who`, `gc`).
- A mail an agent is given again, because it was not acked, says `redelivered="true"` in its
  header, whatever its age.
- When a team closes (`team down`, or the gate closes it), a session that was a member goes on as a
  solo under the name it had, not a new random one. Only if a live solo or an open team already
  has that name does it become `name-2`.
- `piggery template list` lists the templates a team can be founded from, with their summaries.
  `team up` is no longer in `--help` or the public docs (it makes a team nobody can join): found a
  team from a session. It still runs, names its team after the directory, and an unknown template
  name says which ones exist.
- `piggery --help` fits an 80-column window, one line a row (the full usage of each command is in
  its own `--help`). `doctor` prints findings before warnings and "no findings" only when there are
  neither, `log` leaves out empty `participant=`, `run=` and `ref=`, `top` and `ps` do not repeat a
  template that is the team's name, and an empty team in `top` says it can be closed. The install
  script's last line points to `piggery setup` for the full list of harnesses.
- `setup` and `doctor` warn about a harness version only when it is older than the oldest tested
  one; a newer version no longer shows "not tested".
- Fix: the gate was the member created first, so a solo opened before the founder and admitted
  later could become the gate. It is now the member that joined first.
- Fix: when a team reopened, a member whose session had gone on as a newer row (it joined again
  after the team closed) stayed as a second row that mail could wake or `admit` twice. It leaves
  the team now, and `admit` refuses a session that already has a row in it.
- Fix: a first install that ran only `piggery setup pi` had no worker profile, and the first worker
  spawn failed. The daemon writes every missing worker profile when it starts.

## v0.8.0 - 2026-10-05

opencode joins the farm, and two new team templates come built in: `amp-like` and `gastown-like`.

After upgrading, run `piggery restart`: the daemon then knows opencode and refreshes the built-in
templates you never edited. No existing integration changed, so `setup --outdated` has nothing to
do. To use opencode, run `piggery setup opencode`.

- opencode 1.x (tested with 1.18.34): an opencode session you open joins piggery through a plugin,
  and a team can start opencode workers (`opencode serve`, one per worker). Mail wakes an idle
  session and reaches a busy one at its next step; `abort`, `stop`, `resume` and `model` work on
  its workers. `setup opencode` adds one `plugin` entry to your `opencode.json` and refuses an
  `opencode.jsonc` (it prints the line to add by hand). Workers keep your opencode setup, except
  the `question` and `task` tools (`disabled_tools` in `harness/opencode.json`; a role gets one
  back with `spawn.allow_tools`).
- Template `amp-like`, after [Amp](https://ampcode.com)'s oracle and code review: a lead does the
  work and calls an oracle (hard reasoning) or a reviewer (one diff), each answering once. It pays
  off when they run another model family than the lead.
- Template `gastown-like`, after [Gas Town](https://github.com/gastownhall/gastown): a mayor splits
  the work, polecats do each task on their own branch and worktree, and a refinery merges the
  branches one at a time into an integration branch. Your own branch moves only when you say.
- `council`: the chair stops at the verdict and starts agents for other work, such as carrying out
  the decision, only when you ask.
- [manifests/README.md](manifests/README.md) draws every built-in template, with when to pick it.
- Forks get an `AGENTS.md` with the rules that keep the design intact, for people and coding agents.
- `setup <harness>` keeps one copy of each of your config files it is about to change for the first
  time, in `~/.piggery/backups/setup/<harness>/`. `setup remove` still takes out only piggery's part
  and never restores the copy; it is there if something goes wrong.
- Fix: `setup pi --ext` then `setup remove pi` left a `settings.json` of `{}` where there was none,
  and could re-indent other values in it (such as `packages`).

## v0.7.1 - 2026-10-05

Codex threads that share one app-server are no longer mixed up.

After upgrading, run `piggery restart`, then restart Codex (the Desktop app, or the shared
`codex app-server` and its TUIs). No integration changed, so `setup --outdated` has nothing to do.

- Fix: Codex Desktop, and TUIs started with `--remote`, run every thread in one `codex app-server`,
  and piggery made all of them one participant, so mail or a wake could reach the wrong thread or
  project. Each thread is now its own participant; a piggery tool call there that does not say
  which thread it is from is refused. A Codex TUI and Claude still keep one participant across
  `/clear`. Participants already mixed up stay so until they are closed. Reported by @haohao3k
  (#4).
- Fix: `--admin` on an agent's command (such as `who`) now points to `piggery ps` instead of the
  removed `dump`, and `gc`'s usage says `--closed-before DURATION` (such as `168h`).

## v0.7.0 - 2026-10-03

piggery now tells you, by itself, when a team's mail flow needs you, and `piggery check` tests your
files before a restart.

After upgrading, run `piggery setup --outdated`: the pi and omp integrations are now 3 and dsh 4
(Paseo, Claude and Codex are unchanged). Then restart the pi, omp and dsh sessions that were open.

Breaking: only piggery writes to `notify` now. An agent's send to `notify` is refused, and a
`to: notify` routing line or a `notify: notify` timer in a template is ignored with a warning
(`piggery check` lists them). Notify hooks run from `~/.piggery/hooks/notify.d/`; a single
`~/.piggery/hooks/notify` is no longer run, so move it into that directory.

- Notices, decided by the engine at the end of a gate's turn, from the team's mail alone: `reply`
  (a turn on team mail ended and the gate sent nothing), `settled` (the gate sent its last message
  and nobody works or has mail waiting), `failed` (a turn on team mail failed, so that mail waits)
  and `gate_lost`. Your chat with the gate, a gate that is still dispatching work, a headless gate
  and a member that is not the gate never notify. The hook's JSON line adds `gate` and `dir`,
  and `kind` is the notice's kind. The respawn-limit notice goes to the worker's lead only.
- `piggery setup notify add desktop|herdr|ntfy:<topic>` writes a ready hook into `notify.d/`
  (`remove` takes it out; alone, it lists the hooks and what each needs). Every file there runs in
  parallel, each with its own 10-second limit.
- `piggery check`: reads `config.yaml`, the harness profiles, every template and your prompts with
  the daemon's own loaders and prints what they would refuse or ignore; it changes nothing.
- The daemon checks once a day for a newer release; `top`, `setup` and `update --check` say `vX
  available: piggery update`. Nothing is installed on its own; `update.check: false` turns it off.
- `top` shows the latest notices at the bottom left, beside the events; `n` opens or closes them,
  like `e` for the events. `ps --view` carries them as `notices`, and the update notice as
  `daemon.update`.
- The template list an agent reads now also shows a template that `team up` would refuse.
- Fix: a build stamped `dev-<sha>` is treated as a build from source: `piggery update` no longer
  replaces it with an older release unless `--force`.

## v0.6.0 - 2026-10-01

The Paseo plugin is rebuilt on what `piggery top` shows, and a project no longer jumps around the
list when its sessions reconnect.

After upgrading, run `piggery setup --outdated`: the pi and omp integrations are now 2, dsh 3 and
Paseo 3. Then restart the pi, omp and dsh sessions that were open, and reload the Paseo app.

- Paseo plugin, rebuilt: the Overview has `top`'s rows, order, state words, folds and since, one
  line per worker with a ctx column, a header with working/idle/waiting counts, events as short
  lines, and a dialog with a worker's details and tail. A new Board tab gives one band per project
  and a column per status (working, idle, waiting, gone), filtered by team; projects with nothing
  live start folded, and gone workers show on request.
- `piggery ps --view` and `piggery tail --view` print what `top` shows as versioned JSON (rows with
  their actions, header counts, events, Overview; tail lines with their kind). The Paseo plugin reads
  them from the installed binary, so `top`, `ps` and the plugin always agree.
- Projects are listed live first, then sleeping (nothing working and no real turn for over a day),
  all gone, closed. A live project sorts by its latest real turn; a reconnect or a daemon restart
  moves nothing.
- An idle worker's or solo's since counts from its last real turn, not from a reconnect. `ps --json`
  gives a solo's `last_turn_end` too.
- `piggery top`: in a live headless worker's Overview, a click on the model (blue, `▾`) or `M` opens a
  picker of the models its harness offers, with the thinking level; `enter` or a double-click
  applies (a dsh worker lists its models once the dsh integration is updated). The footer always
  lists `M model`, dim where it does nothing.
- Fix: in pi, omp and dsh sessions, a wake or reconnect that finds no mail is not a turn any more (no
  empty turn, last turn kept); needs the updated integration.
- Fix: in `top`'s Overview, the last turn no longer runs into the joined/spawned value.

## v0.5.3 - 2026-09-30

Big teams stay readable in `piggery top`, and a worker's piggery tool call survives a daemon restart.

After upgrading, reload the Paseo app if you use its plugin (its integration is now 2; `piggery setup
--outdated` updates it).

- `piggery top`: Enter opens or closes everywhere: a team (live ones too), the gone line, a member's
  details; `t` only switches Overview/Tail. A collapsed team is one line with its working/idle/gone
  counts and held mail, and top remembers which teams you opened or closed (`<dir>/cache/top.json`).
- Gone members with no live worker under them fold into one dim row at the bottom of their team
  (`▸ N members  ✗ gone  <since>`, on the columns), in `top`, `piggery ps` and the Paseo plugin;
  Enter expands it. A team's members are indented under the team's title, and the list no longer
  has an unacked column (the header, details and a folded team still show it).
- `piggery top` and the Paseo plugin: all-gone and closed teams are rows on the columns
  (`▸ team old  ✗ gone  15h`), a solo row has a team row's shape, and the state column keeps one
  width whatever the states, so the layout depends only on the window.
- `piggery top` and the Paseo plugin: the gate is tagged on its member (`summer-hamster (gate)`)
  instead of on the team line; a requested worker shows as `◌ queued` (ps keeps `requested`); the
  cwd column shows only when someone works outside their group's directory.
- `piggery top` scrolls a long list under a column header that stays put, with `↑ N` / `↓ N` on the
  border for rows out of view; PgUp/PgDn and Home/End (`? all keys`). Opening, closing or scrolling
  never moves a column, and a collapsed team line drops whole parts to fit instead of being cut.
- `piggery top`: the footer says whether the mouse is captured (`m mouse on` / `m mouse off`), and
  events show their time (`14:12:05` today, `09-29 14:12` before) instead of how long ago. Events
  start folded; `e` opens them and top remembers it.
- Paseo plugin, like top: Events fold (folded by default), event times, a folded team shows its
  counts, and folds are remembered on the Paseo host.
- `prompts` in config.yaml takes `*` (every role, every template, and solo sessions) and
  `<template>/*` (every role of that template); a file several entries name is added once.
- `team up` warns when a role pins `spawn.model` or `spawn.thinking` but leaves `harness: inherit`:
  a model name belongs to one harness.
- Fix: a Claude Code or Codex worker's piggery tool call made while the daemon restarts waits up to
  10 seconds for the connection and then runs, instead of failing. A call already sent when the
  connection dropped still fails, so nothing runs twice.
- Fix: a tool call right after the first connect could fail with "piggery is not reachable right
  now".
- Fix: `piggery x <name>` (and abort, model, resume, tail) no longer refuses a name when the other
  matches are gone members of closed teams; the live participant wins.

## v0.5.2 - 2026-09-30

Fix: `x` and `kill` now stop the command a worker is running, not only the worker.

- `x` / `kill` left a worker's running shell command alive when the harness ran it in a process group of
  its own (pi's bash tool does). Kill now asks first (SIGTERM, so the harness and its extensions clean
  up), waits up to 2 seconds, then SIGKILLs the worker and everything left in its process tree; stop
  ends the tree the same way. Something detached before the kill that no tool tracks may survive.

## v0.5.1 - 2026-09-30

An emergency stop in `piggery top` (`x` kills the selected worker) and a tidier bottom of the screen.

- Emergency stop: `x` in `piggery top` kills the selected headless worker (it asks `kill <name>? y/n`
  once; a session you opened gets a reason and nothing else), and `piggery x <worker>` is the short
  form of `piggery kill`.
- `piggery top`: events in a box that `e` folds to one line (`● Events · <latest>`; it starts folded
  under 30 rows), and the keys on two aligned lines under a rule.

## v0.5.0 - 2026-09-30

`piggery setup --outdated` brings what piggery installed for your harnesses up to date, and user docs
are public: `docs/guide.md` and `docs/reference.md`.

After upgrading, run `piggery setup --outdated`; reload the Paseo app if you use its plugin. Each
thing piggery installs for a harness now has an integration version, and whether it is outdated is
decided by that number, not by the build.

- One integer for each of pi, omp, dsh, claude, codex and paseo, bumped only when what is installed
  changes; an install carries it as `PIGGERY_INTEGRATION_VERSION=N`. A rebuild that leaves the
  installed part as it was no longer reads as outdated or rewrites anything. Installs from before
  this read as outdated once. pi, omp and dsh are brought up by the daemon at its start; for claude,
  codex and paseo the daemon logs one warning, `ps` and `top` show `outdated: claude (v1 < v2):
  piggery setup --outdated` (`ps --json` has the list as `outdated`, which the Paseo plugin reads
  instead of running a command), and `setup` and `doctor` show `vN < vM` (a Codex hook missing from
  `hooks.json` counts).
- `piggery setup --outdated` updates every installed integration that is outdated (`setup <harness>`
  for each, one line per update and what to do after); not installed ones are untouched, and it says
  so when all is current. `install.sh` no longer runs anything after an install: it prints
  `piggery setup <harness>` for a new machine and `piggery setup --outdated` for an upgrade.
- Docs: `docs/guide.md` (by task, including how to customize `~/.piggery`) and `docs/reference.md`
  (every command, every key of `config.yaml`, the harness profiles and the template manifest, and
  what each harness can do), linked from the README. The adapters' "piggery binary is not on PATH"
  error links the guide.
- `top` shows the daemon's version at the end of its key footer; when the `piggery` you run is
  another build, it is amber with `(cli <version>: piggery restart)`. `ps --json` has `version`.
- Paseo plugin: a session you opened has a Tail and ctx/turns (from its transcript), a member's
  Overview shows its current task (handed back, newer mail), and the list starts with top's status
  line: daemon age, teams, working and idle, held, unacked, version, and the outdated notice.

## v0.4.0 - 2026-09-29

Shared prompts: your own rules (code style, how you organise a project) go into the role card of
the roles you pick, in every template and harness.

- `config.yaml` `prompts: [{file, roles}]`: a role is `<role>` (every template), `<template>/<role>`
  or `solo`. The file is read each time a card is built, so an edit reaches the next session with no
  restart. A mistake (an unreadable file, an unknown template, a role in no template) is a warning
  in `~/.piggery/serve.log` and that part is skipped; it never stops the daemon or a spawn.
- A template's name key is `template:` (was `model:`, easy to read as the AI model). Your templates
  are rewritten in place at `setup` or daemon start (only that key); teams already made keep
  working, and a file with `model:` is still read.
- Mail headers name the sender's real role and the relation: `ana (supervisor, you report to
  them)`, `bo (executor, reports to you)`; before, any superior read "your lead" and a same-role
  member "your peer". Codex workers are told to ask the member they report to, not "your lead".

## v0.3.0 - 2026-09-29

Two new harnesses, omp (oh-my-pi) and dsh (DeepSeek Harness 0.2), and each member's current task
in `piggery top`.

Breaking: mail threads, `expects_reply` and the limits `max_hops` and `messages_per_thread` are
removed, and a team template that sets either limit to a number is refused; delete the key. Nothing
in piggery acted on them: `reply_to` stays, and `messages_per_participant_per_minute` is the flood
guard. Mail held by a removed limit is released when the daemon upgrades its database. Teams
already up keep working. Run `piggery setup pi` again so pi sessions stop showing `thread=`.

- omp: `piggery setup omp` adds piggery to omp sessions (mail is steered in and shows in the
  session); a role can run omp workers (`harness: omp`), resumed with their context after a stop.
- dsh 0.2.0-rc.1: `piggery setup dsh` adds piggery to `dsh web` sessions; a role can run dsh
  workers (`harness: dsh`). Workers turn off dsh's upload of session logs to DeepSeek.
- `send --op assign` marks a mail as the member's current task (a spawn or resume task is one);
  `top`'s Overview shows it, whether it was handed back, and a newer unmarked mail.
- `reply_to` takes a bare `N` as well as `#N`.
- The daemon backs up its database before a schema upgrade (`~/.piggery/backups/`, the 3 newest
  kept).
- `~/.piggery` is laid out as `plugins/`, `run/` (per-run scratch), `sessions/` (session data
  piggery keeps); gc removes a removed participant's entries there and in `logs/`, and now also
  removes a solo session gone longer than `gc.closed_after` (archived first, like a closed team).
- `send` answers `sent #N`; a mail's header has no `thread=`; `send --expects-reply` is gone.
- `top`: an open team whose members are all gone is one line; a model switched by `piggery model`
  shows at once. The Paseo plugin starts such a team collapsed (every Paseo app needs 0.9.1+).
- The built-in templates allow 10 live workers at once (`limits.concurrency`, was 4 or 5).
- An installed built-in template file you edited to exactly the new built-in is updated again by
  later versions.
- The gate roles' prompts and the solo card point to `piggery skills` for the rest of piggery.
- One-line install for Linux and macOS:
  `curl -fsSL https://raw.githubusercontent.com/sting8k/piggery/main/install.sh | sh` picks the
  build for your OS and CPU, checks it against the release's `checksums.txt`, and installs it in
  `~/.local/bin` (`PIGGERY_INSTALL_DIR`, `PIGGERY_VERSION` to change).
- A release's notes on GitHub are its section of this changelog.
- Tests that wait for the daemon to start allow 10 seconds, so a slow CI runner no longer fails them.

## v0.2.0 - 2026-09-29

Breaking: a team template with a `tools:` section is now refused. Give its roles `send` and say in
their prompt what to send to whom, then delete the section. An installed built-in template you
edited is not updated for you; edit it the same way.

- Every role of the built-in templates talks with `send` (no `done`/`ask`/`answer` tools).
- Declarative tools are removed: a template with a `tools:` section is refused; use `send`.
- A new session's default name is one word.
- Interactive sessions (pi, Claude, Codex; solos too) show ctx, turns and a tail in `top`, `ps --json` and `piggery tail`, read from the harness's own transcript.

## v0.1.0 - 2026-09-28

First release.

- One Go binary with a local daemon (SQLite, unix socket). Nothing runs in the cloud.
- Mail that waits for an agent to be back, and counts as delivered only when the agent's turn
  that read it has finished.
- Team layouts as YAML: roles, who may talk to whom, who may spawn whom. The daemon checks every
  send and spawn. Built-in: `supervisor-executor`, `slp`, `council`, `p2p`; make your own with
  `piggery template new`.
- Harnesses: pi, Claude Code and Codex (`piggery setup`). Headless workers run in their own
  sessions and can be stopped, resumed and switched to another model.
- Watch and step in: `top`, `ps`, `log`, `tail`, `why`, `abort`, `kill`, `resume`, `model`,
  `release`.
- Paseo plugin: `piggery setup paseo` adds a Piggery view (the same as `piggery top`) to Paseo.
- Upkeep: `gc` archives closed teams, `doctor` checks the daemon's state, `update` installs a
  newer release.
