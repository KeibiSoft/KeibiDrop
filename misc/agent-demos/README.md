# Agent demos

Runnable demos for the agent-facing surfaces: the `kd` CLI and the `kdmcp`
MCP server. Each script starts two peers on this machine, pairs them, and
checks its claims as it goes, so it doubles as a smoke test. No setup beyond
the build; each run creates its own temporary directories and cleans up.

Build first, from the repository root:

```bash
make build-kd build-kdmcp
```

## The demos

| Script | Shows |
|---|---|
| `demo-two-machines.sh` | The `kd` CLI pairing two peers and moving a file with no prompts, no TTY, and usable JSON results |
| `demo-mcp.py` | The same through MCP tool calls; with `FUSE=1` it reads a 2,184-file tree straight off the mount |
| `demo-reconnect.py` | Saved peers, reconnecting with no code exchange, and a download resuming after a mid-transfer disconnect |
| `measure-on-demand.py` | How many bytes cross the wire when 1 MB is read out of a 256 MB file |
| `demo-fast-folder.py` | A timed run of the in-place folder offer, for reading numbers off a terminal |
| `two-agents.sh` | The side-by-side demo: one pane offers a dataset, the other lists it and reads from the mount. Needs tmux |

## Run them

```bash
KD=./kd        bash misc/agent-demos/demo-two-machines.sh
KDMCP=./kdmcp  uv run --no-project misc/agent-demos/demo-mcp.py
FUSE=1 KDMCP=./kdmcp uv run --no-project misc/agent-demos/demo-mcp.py
KDMCP=./kdmcp  uv run --no-project misc/agent-demos/demo-reconnect.py
KDMCP=./kdmcp  uv run --no-project misc/agent-demos/measure-on-demand.py
```

The Python scripts need only the standard library, so plain `python3` works
too. `FUSE=1` needs a FUSE provider (macFUSE, WinFsp, or libfuse3); without
one the scripts exercise copy mode.

Both peers run on one machine, and the session between them goes over the
relay, so a run exercises the real network path. Wall-clock times therefore
include real round trips on your uplink; the byte counts are the numbers to
compare.

## Record the GIF

`make-gif.sh` records `two-agents.sh` with vhs and stamps the wordmark in
the corner. It needs vhs (with ttyd), rsvg-convert, ffmpeg, magick and tmux.
`agent_pane.py`, `demo_timer.py` and `demo.tape` are its supporting pieces.

```bash
KDMCP=$(pwd)/kdmcp misc/agent-demos/make-gif.sh
```
