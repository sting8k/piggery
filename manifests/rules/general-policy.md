General policy for every role. A project's own rules win where they are stricter.

Ownership
- Own the outcome and the completion conditions given to your role, whether you work alone,
  coordinate others, execute a scope, advise, or review. Use your full intelligence in every role.
- Question what deserves questioning, decide what is yours to decide, and bring a recommendation
  when another authority must act. Keep the Human focused on the decisions that need their
  direction.

Change the right thing
- Before you fix or tune an existing mechanism, restate what it must achieve and check that it is
  still the right way to achieve it, in the right place. Keep the required outcome and the
  commitments others rely on, not a mechanism only because it exists.
- Before choosing how, answer:
  1. What outcome is actually needed?
  2. What was the current mechanism made to serve?
  3. Which constraints come from the real need and which only from today's implementation, and
     do they still hold?
  4. Is redundant or wrong behaviour being kept? Is the fault inside a sound mechanism, or is the
     mechanism the fault?
  5. Would patching, replacing or removing it give the right and smallest result?
- Before asking "how do I make this faster", ask "what should it not do at all". Patch only once
  the mechanism is shown to fit.

Tests
- A test proves something someone depends on: one test per invariant or real risk, one per
  acceptance criterion, and for every bug fix a repro that fails on the old code first.
- No tests for wording, formatting, layout, colours, exact output strings, getters or wiring. No
  tables of permutations along the same code path. Do not tighten an older test just to pin a new
  look.
- Before adding a test, look for one that already covers the path; extend it instead.
- A visual or UI change is verified by running it (render, screenshot, measure), not by freezing
  its text in a test.
- Report the test lines you added and removed with your work.

Docs
- Write only what a reader needs to use or change the thing. Change the doc in place; one home
  per fact.
- No run diaries, handoff essays or copies of rules that live elsewhere. Delete what your change
  made stale; git keeps history.

Briefing others
- Do not ask for tests or docs these rules forbid. "One test for each" display detail is a
  formatting test.
