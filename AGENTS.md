# AGENTS.md

**The working notes for this repository live in [`CLAUDE.md`](CLAUDE.md). Read
that file before doing anything here.** This file exists only because two
vendors chose different names for the same thing.

There is deliberately no second copy of the notes. This file used to be one,
and it drifted: it still described a shell benchmark harness that issue #190
deleted, told readers to run seven scripts that no longer exist, and pointed at
a `jq` pipeline the project stopped using. An agent that read it produced
confident work against a repository layout from three weeks earlier, which is
worse than having no file at all (issue #231).

So the rule is: **one file, one copy.** Anything worth writing down for an
automated session goes in `CLAUDE.md`. Nothing goes here. `ci.yml` fails if
this file grows back into a duplicate.
