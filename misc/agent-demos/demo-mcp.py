#!/usr/bin/env python3
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# Two KeibiDrop peers, driven entirely through MCP tool calls. Two kdmcp
# servers are launched the way an MCP client launches one: a subprocess
# speaking newline-delimited JSON-RPC 2.0 over stdio.
#
# Usage: KDMCP=/path/to/kdmcp uv run demo-mcp.py [workdir]
#        FUSE=1 to keep the mount on and read the peer's files in place.

import hashlib
import json
import os
import random
import shutil
import subprocess
import sys
import time

KDMCP = os.environ.get("KDMCP", "kdmcp")
WORK = sys.argv[1] if len(sys.argv) > 1 else "/tmp/kd-mcp-demo"
CONNECT_TIMEOUT = int(os.environ.get("TIMEOUT", "120"))

# FUSE=1 keeps the mount on, so the peer's files are read where they sit and
# only the bytes read travel. Off by default so this runs on a machine with no
# FUSE provider installed.
USE_FUSE = os.environ.get("FUSE", "") not in ("", "0", "no", "false")

passed = failed = 0


def ok(msg):
    global passed
    passed += 1
    print(f"  ok    {msg}", flush=True)


def bad(msg):
    global failed
    failed += 1
    print(f"  FAIL  {msg}", flush=True)


def step(msg):
    print(f"\n== {msg}", flush=True)


class Server:
    """One kdmcp subprocess, spoken to the way an MCP client speaks to it."""

    def __init__(self, name, env):
        self.name = name
        full = dict(os.environ)
        full.update(env)
        log = os.path.join(env["KEIBIDROP_CONFIG_DIR"], "..", "stderr.log")
        self.proc = subprocess.Popen(
            [KDMCP],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=open(log, "w"),
            text=True,
            bufsize=1,
            env=full,
        )
        self.n = 0

    def call(self, method, params=None):
        self.n += 1
        req = {"jsonrpc": "2.0", "id": self.n, "method": method}
        if params is not None:
            req["params"] = params
        self.proc.stdin.write(json.dumps(req) + "\n")
        self.proc.stdin.flush()
        line = self.proc.stdout.readline()
        if not line:
            raise RuntimeError(f"{self.name}: server closed the stream")
        return json.loads(line)

    def tool(self, name, args=None):
        """Call one tool and return (payload, is_error)."""
        resp = self.call("tools/call", {"name": name, "arguments": args or {}})
        result = resp["result"]
        payload = json.loads(result["content"][0]["text"])
        return payload, result.get("isError", False)

    def close(self):
        try:
            self.proc.stdin.close()
            self.proc.wait(timeout=15)
        except Exception:
            self.proc.kill()


def make_env(root, inbound, outbound, share=None):
    env = {
        "KEIBIDROP_CONFIG_DIR": f"{root}/cfg",
        "KD_SAVE_PATH": f"{root}/recv",
        "KD_MOUNT_PATH": f"{root}/mnt",
        "KD_LOG_FILE": f"{root}/kd.log",
        "KD_INBOUND_PORT": str(inbound),
        "KD_OUTBOUND_PORT": str(outbound),
        "KD_INCOGNITO": "1",
        "KD_SHARE_ROOT": share or "",
    }
    if not USE_FUSE:
        env["KD_NO_FUSE"] = "1"
    return env


def build_tree(root, depth=5, dirs_per_level=3, files_per_dir=6):
    """Write a nested directory tree and return {relative path: sha256}."""
    rng = random.Random(1234)
    digests = {}

    def fill(path, level):
        os.makedirs(path, exist_ok=True)
        for i in range(files_per_dir):
            name = f"file{i}.bin"
            body = bytes(rng.getrandbits(8) for _ in range(rng.randint(200, 4000)))
            full = os.path.join(path, name)
            with open(full, "wb") as fh:
                fh.write(body)
            rel = os.path.relpath(full, root).replace(os.sep, "/")
            digests[rel] = hashlib.sha256(body).hexdigest()
        if level < depth:
            for d in range(dirs_per_level):
                fill(os.path.join(path, f"level{level + 1}_dir{d}"), level + 1)

    fill(root, 0)
    return digests


def main():
    step(f"Workspace {WORK}")
    shutil.rmtree(WORK, ignore_errors=True)
    for sub in ("A/cfg", "A/recv", "A/mnt", "A/share", "B/cfg", "B/recv", "B/mnt"):
        os.makedirs(f"{WORK}/{sub}", exist_ok=True)

    # A folder of many files, nested several levels deep. This is the shape
    # real work has: a build output, a dataset, a case folder.
    tree_root = f"{WORK}/A/share/project"
    digests = build_tree(tree_root)
    total_bytes = sum(os.path.getsize(os.path.join(tree_root, p)) for p in digests)
    depth = max(p.count("/") for p in digests) + 1
    print(f"  built {len(digests)} files in {depth} levels, {total_bytes} bytes", flush=True)

    step("Launch two MCP servers")
    # Each peer gets its own config directory and its own ports. Peers that
    # share a config directory load the same identity and end up with the same
    # code, and a peer connects to a different code than its own.
    a = Server("A", make_env(f"{WORK}/A", 26501, 26502, share=f"{WORK}/A/share"))
    b = Server("B", make_env(f"{WORK}/B", 26511, 26512))

    try:
        for s in (a, b):
            r = s.call("initialize", {
                "protocolVersion": "2025-06-18",
                "capabilities": {},
                "clientInfo": {"name": "kd-demo", "version": "1"},
            })
            info = r["result"]["serverInfo"]
            ok(f"{s.name} started, running {info['name']} {info['version']}")

        tools = a.call("tools/list")["result"]["tools"]
        ok(f"the server offers {len(tools)} tools: {', '.join(t['name'] for t in tools)}")

        step("The server prepares the machine on its own")
        setup_a, err = a.tool("kd_setup")
        if not err and setup_a.get("ready"):
            ok(f"A is ready, mode {setup_a['mode']}, sends from {setup_a['share_root']}")
        else:
            bad(f"A setup returned {setup_a}")

        setup_b, _ = b.tool("kd_setup")
        if setup_b.get("share_root", "").startswith("disabled"):
            ok("B has no share root, so B receives and B sends nothing")
        else:
            bad(f"B reports share_root {setup_b.get('share_root')}")

        step("The limits hold before anything is paired")
        _, err = a.tool("kd_send_file", {"path": "/etc/hosts"})
        ok("a path outside the share root is refused") if err else bad("/etc/hosts was accepted")

        _, err = b.tool("kd_pull_file", {"name": "../../../etc/passwd"})
        ok("a file name that climbs out of the save directory is refused") if err \
            else bad("the climbing name was accepted")

        _, err = b.tool("kd_send_file", {"path": tree_root})
        ok("a peer with no share root is refused when it tries to send") if err \
            else bad("B sent a file with no share root")

        step("Pair the two peers")
        # Each peer reads its own code, the two codes are swapped, and each
        # hands the other's code back. The engine compares the two codes to
        # decide which peer listens, so the order of these calls is free.
        share_a, _ = a.tool("kd_create_share")
        share_b, _ = b.tool("kd_create_share")
        code_a, code_b = share_a.get("code"), share_b.get("code")
        if code_a and code_b and code_a != code_b:
            ok(f"A's code is {code_a[:12]}..., B's code is {code_b[:12]}...")
        else:
            bad(f"codes came back as {code_a} and {code_b}")

        accepted, err = a.tool("kd_accept_peer", {"code": code_b})
        ok("A takes B's code") if not err else bad(f"accept_peer returned {accepted}")

        joined, err = b.tool("kd_join", {"code": code_a, "timeout_s": 60})
        ok(f"B takes A's code, connected is {joined.get('connected')}") if not err \
            else bad(f"join returned {joined}")

        deadline = time.time() + CONNECT_TIMEOUT
        state_a = state_b = {}
        while time.time() < deadline:
            state_a, _ = a.tool("kd_status")
            state_b, _ = b.tool("kd_status")
            if state_a.get("connected") and state_b.get("connected"):
                break
            time.sleep(1)
        if state_a.get("connected") and state_b.get("connected"):
            ok(f"both peers are connected over {state_a.get('mode')}")
        else:
            bad(f"A is {state_a.get('state')} and B is {state_b.get('state')}")

        step("A offers the whole folder in one call")
        sent, err = a.tool("kd_send_file", {"path": "project"})
        if not err:
            ok(f"A offered {sent.get('files')} files, {sent.get('bytes')} bytes, "
               f"in {sent.get('elapsed_s')}s")
            ok("A sent the listing, and the file contents stay on A until B reads them")
        else:
            bad(f"send returned {sent}")

        step("B sees the folder")
        listing = {}
        deadline = time.time() + 120
        while time.time() < deadline:
            listing, _ = b.tool("kd_list_files", {"source": "remote"})
            if len(listing.get("entries", [])) >= len(digests):
                break
            time.sleep(1)
        seen = [e["name"].lstrip("/") for e in listing.get("entries", [])]
        if len(seen) >= len(digests):
            ok(f"B lists {len(seen)} files, the same count A offered")
        else:
            bad(f"B lists {len(seen)} files and A offered {len(digests)}")

        nested = sorted(seen, key=lambda n: n.count("/"))[-1] if seen else ""
        ok(f"the deepest path B sees is {nested}") if nested else bad("B lists no paths")

        step("B reads files out of the folder")
        sample = sorted(digests)[:3] + [sorted(digests, key=lambda p: p.count("/"))[-1]]
        for rel in sample:
            remote = f"project/{rel}"
            pull, err = b.tool("kd_pull_file", {"name": remote})
            tid = pull.get("transfer_id")
            if not tid:
                bad(f"pull of {remote} returned {pull}")
                continue
            st = {}
            deadline = time.time() + 60
            while time.time() < deadline:
                st, _ = b.tool("kd_transfer_status", {"transfer_id": tid})
                if st.get("done"):
                    break
                time.sleep(0.5)
            path = st.get("local_path", "")
            if st.get("done") and os.path.exists(path):
                got = hashlib.sha256(open(path, "rb").read()).hexdigest()
                ok(f"{remote} arrived and matches") if got == digests[rel] \
                    else bad(f"{remote} arrived with a different checksum")
            else:
                bad(f"{remote} did not arrive: {st}")

        if USE_FUSE:
            step("B reads the folder in place, with no copy step")
            mount = state_b.get("read_peer_files_at") or state_b.get("mount_path")
            ok(f"B reads A's files under {mount}")

            found = {}
            deadline = time.time() + 90
            while time.time() < deadline:
                found = {}
                for root_dir, _, files in os.walk(mount):
                    for fn in files:
                        full = os.path.join(root_dir, fn)
                        found[os.path.relpath(full, mount).replace(os.sep, "/")] = full
                if len(found) >= len(digests):
                    break
                time.sleep(2)

            ok(f"the folder appears under the mount with {len(found)} files") if found \
                else bad(f"the mount at {mount} stayed empty")

            checked = mismatched = 0
            for rel, sha in digests.items():
                full = found.get(f"project/{rel}")
                if not full:
                    continue
                with open(full, "rb") as fh:
                    if hashlib.sha256(fh.read()).hexdigest() != sha:
                        mismatched += 1
                checked += 1
            if checked and mismatched == 0:
                ok(f"B read {checked} files off the mount and every one matches A's copy")
            else:
                bad(f"{mismatched} of {checked} files read off the mount differ")

        step("Calls can be repeated")
        again, _ = a.tool("kd_create_share")
        ok("kd_create_share returns the same code") if again.get("code") == code_a \
            else bad("the code changed on a second call")

        rejoin, _ = b.tool("kd_join", {"code": code_a})
        ok("kd_join on a live session reports the session") if rejoin.get("connected") \
            else bad(f"join returned {rejoin}")

        resend, err = a.tool("kd_send_file", {"path": "project"})
        ok(f"offering the folder again reports {resend.get('files')} files") if not err \
            else bad(f"the second offer returned {resend}")

        step("Close the session")
        for s in (a, b):
            out, err = s.tool("kd_disconnect")
            ok(f"{s.name} closed the session") if not err else bad(f"{s.name} returned {out}")
        out, err = a.tool("kd_disconnect")
        ok("closing an already closed session is accepted") if not err \
            else bad("the second close failed")

    finally:
        a.close()
        b.close()

    print(f"\n== {passed} passed, {failed} failed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
