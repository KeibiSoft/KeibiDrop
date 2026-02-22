# Next Session Handoff

## Status
Last completed: fix-prefetch-rename-race — fix zeroed files on Bob when rename arrives before prefetch completes
Next task: OPTIONAL — investigate stale Save dir cleanup on startup
Plan file: none (optional tasks, no formal plan)

## What Was Done
- Diagnosed and fixed prefetch-rename race condition (git lock-then-rename pattern caused zeroed files)
- Root cause: three bugs — `RealPathOfFile` not updated in `AllFileMap`, deferred closure read field without lock (data race), deferred rename fired on partial downloads
- Fix: `bitmap.IsComplete()` guard + `RLock` around path read + atomic `os.Rename` in deferred closure + `AllFileMap` update in service.go
- TDD: failing test committed first, fix committed second; `go test -race ./pkg/filesystem/...` passes clean
- PR #22 opened: https://github.com/KeibiSoft/KeibiDrop/pull/22 (fix/prefetch-rename-race → main)
- Deleted stale `handoff/linux-testing-2026-02-22` branch (locally and origin)
- Reverted unintentional shell script modifications made by subagents during development

## What's Left (ordered)
- [ ] OPTIONAL: investigate `SaveAlice/` / `SaveBob/` stale state cleanup on startup
- [ ] OPTIONAL: split `cmd/internal/checkfuse/fusecheck.go` into per-platform files (low priority)
- [ ] OPTIONAL: add `Log_*.txt`, `MountAlice/`, `MountBob/`, `SaveAlice/`, `SaveBob/`, `libkeibidrop.*` to `.gitignore`

## Task Specs

### OPTIONAL — Stale Save directory cleanup

On startup (or before mounting), `SaveAlice/` and `SaveBob/` may contain files from a previous session. Currently the `nonempty` flag handles the mount directory, but `Save*` dirs accumulate state indefinitely.

Questions to answer before coding:
1. What is the intended semantics — ephemeral (cleared on restart) or persistent?
2. If persistent, do we want deduplication / content-addressed storage?
3. If ephemeral, clear `Save*` dirs at startup before mounting.

### OPTIONAL — Per-platform fusecheck split

`cmd/internal/checkfuse/fusecheck.go` uses `runtime.GOOS` switch. Could be split into `fusecheck_linux.go` and `fusecheck_darwin.go` using Go build tags. Current code works correctly — low priority.

### OPTIONAL — .gitignore cleanup

These files/dirs are always untracked and belong in `.gitignore`:
- `Log_Alice.txt`, `Log_Bob.txt`
- `MountAlice/`, `MountBob/`
- `SaveAlice/`, `SaveBob/`
- `libkeibidrop.a`, `libkeibidrop.h`
- `docs/session-*.md`

## Instructions for the Next Session

1. Read this file completely before doing anything.
2. Merge PR #22 if not already merged (or confirm it's merged into main).
3. Decide with the user which optional task (if any) to tackle next.
4. If proceeding: invoke `superpowers:subagent-driven-development` skill.
5. Follow the subagent-driven-development flow: implement → spec review → code quality review.
6. After task passes both reviews: commit and push.
7. Update this handoff file and commit: `chore(handoff): update next-session after <task-id>`
8. Push.
9. Tell the user the task is done and suggest running /clear.

> After completing your task, update docs/next-session.md following the Session Handoff skill pattern (skill name: session-handoff). Mark your task done, advance the next task, copy in the next task spec, commit with `chore(handoff): update next-session after <task-id>`, and push.
