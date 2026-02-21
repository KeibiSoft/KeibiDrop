# Next Session Handoff

## Status
Last completed: linux-fuse-verify — FUSE mounts and file sync verified, fixes committed
Next task: OPTIONAL — investigate SaveAlice/SaveBob stale state cleanup on startup

## What Was Done
- Verified both `MountAlice` and `MountBob` mount correctly as `fuse.keibidrop-rust`
- Verified file sync: `testfile.txt` added via Alice's UI appeared in `SaveBob/` and `MountBob/`
- Verified FUSE read path: `cat MountBob/testfile.txt` returns correct content
- Committed: `fix(linux): nonempty mount option and surface FUSE mount errors`
- Pushed to: main (e887b2d)

## What's Left (ordered)
- [ ] OPTIONAL: investigate `SaveAlice/` / `SaveBob/` stale state cleanup on startup

## Task Specs

### OPTIONAL — Stale Save directory cleanup

On startup (or before mounting), the `SaveAlice/` and `SaveBob/` directories may contain files
from a previous session. Currently the `nonempty` flag handles the mount directory, but `Save*`
dirs accumulate state indefinitely.

Consider: should `Save*` be cleared on startup? Or should old files be served to the peer as
still-available? This is a product decision before implementing anything.

Questions to answer before coding:
1. What is the intended semantics — is the share ephemeral (cleared on restart) or persistent?
2. If persistent, do we want deduplication / content-addressed storage?
3. If ephemeral, clear `Save*` dirs at startup before mounting.

## Instructions for the Next Session

1. Read this file completely before doing anything.
2. Decide with the user whether stale Save directory cleanup is desired and what semantics to use.
3. If implementing: invoke `superpowers:subagent-driven-development` skill.
4. Follow the subagent-driven-development flow: implement → spec review → code quality review.
5. After task passes both reviews: commit and push.
6. Update this handoff file and commit: `chore(handoff): update next-session after <task-id>`
7. Push.
8. Tell the user the task is done and suggest running /clear.

> After completing your task, update docs/next-session.md following the Session Handoff skill pattern (skill name: session-handoff). Mark your task done, advance the next task, copy in the next task spec, commit with `chore(handoff): update next-session after <task-id>`, and push.
