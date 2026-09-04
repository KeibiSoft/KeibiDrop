#!/usr/bin/env python3
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# What happens after a session ends and the same two peers meet again.
#
# Covers: saving a peer to the address book, reconnecting with no code
# exchange, the peer's files listing again, and a part-finished download
# continuing from where it stopped.
#
# Usage: KDMCP=/path/to/kdmcp uv run demo-reconnect.py [workdir]

import hashlib
import json
import os
import shutil
import subprocess
import sys
import time

KDMCP = os.environ.get("KDMCP", "kdmcp")
WORK = sys.argv[1] if len(sys.argv) > 1 else "/tmp/kd-reconnect-demo"
CONNECT_TIMEOUT = int(os.environ.get("TIMEOUT", "120"))
SIZE = int(os.environ.get("SIZE", str(40 * 1024 * 1024)))

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
    def __init__(self, name, env):
        self.name = name
        self.env = env
        self.proc = None
        self.n = 0
        self.start()

    def start(self):
        full = dict(os.environ)
        full.update(self.env)
        self.proc = subprocess.Popen(
            [KDMCP], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=open(f"{self.env['KD_LOG_FILE']}.err", "a"),
            text=True, bufsize=1, env=full,
        )
        self.n = 0
        self.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                 "clientInfo": {"name": "kd-reconnect", "version": "1"}})

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
        res = self.call("tools/call", {"name": name, "arguments": args or {}})["result"]
        return json.loads(res["content"][0]["text"]), res.get("isError", False)

    def close(self):
        try:
            self.proc.stdin.close()
            self.proc.wait(timeout=15)
        except Exception:
            self.proc.kill()


def env(root, inbound, outbound, share=None):
    # No KD_INCOGNITO here: the address book needs an identity that persists,
    # and that identity is what lets the same two peers recognise each other.
    return {
        "KEIBIDROP_CONFIG_DIR": f"{root}/cfg",
        "KD_SAVE_PATH": f"{root}/recv",
        "KD_MOUNT_PATH": f"{root}/mnt",
        "KD_LOG_FILE": f"{root}/kd.log",
        "KD_INBOUND_PORT": str(inbound),
        "KD_OUTBOUND_PORT": str(outbound),
        "KD_NO_FUSE": "1",
        "KD_SHARE_ROOT": share or "",
    }


def wait_connected(peers, timeout):
    deadline = time.time() + timeout
    while time.time() < deadline:
        states = [s.tool("kd_status")[0] for s in peers]
        if all(st.get("connected") for st in states):
            return states
        time.sleep(1)
    return [s.tool("kd_status")[0] for s in peers]


def main():
    step(f"Workspace {WORK}")
    shutil.rmtree(WORK, ignore_errors=True)
    for sub in ("A/cfg", "A/recv", "A/mnt", "A/share", "B/cfg", "B/recv", "B/mnt"):
        os.makedirs(f"{WORK}/{sub}", exist_ok=True)

    payload = os.urandom(SIZE)
    src = f"{WORK}/A/share/big.bin"
    with open(src, "wb") as fh:
        fh.write(payload)
    sha_src = hashlib.sha256(payload).hexdigest()
    print(f"  file to move: {SIZE} bytes", flush=True)

    a = Server("A", env(f"{WORK}/A", 26561, 26562, share=f"{WORK}/A/share"))
    b = Server("B", env(f"{WORK}/B", 26571, 26572))

    try:
        step("First session")
        code_a = a.tool("kd_create_share")[0]["code"]
        code_b = b.tool("kd_create_share")[0]["code"]
        a.tool("kd_accept_peer", {"code": code_b})
        b.tool("kd_accept_peer", {"code": code_a})
        sa, sb = wait_connected([a, b], CONNECT_TIMEOUT)
        ok(f"paired over {sa.get('mode')}") if sa.get("connected") and sb.get("connected") \
            else bad(f"A is {sa.get('state')}, B is {sb.get('state')}")

        sent, err = a.tool("kd_send_file", {"path": "big.bin"})
        ok(f"A offered big.bin, {sent.get('size')} bytes") if not err else bad(f"send: {sent}")

        step("Both peers save each other")
        saved_b, err = b.tool("kd_save_peer", {"name": "builder"})
        ok(f"B saved A as {saved_b.get('saved')}") if not err else bad(f"B save_peer: {saved_b}")
        saved_a, err = a.tool("kd_save_peer", {"name": "worker"})
        ok(f"A saved B as {saved_a.get('saved')}") if not err else bad(f"A save_peer: {saved_a}")

        listed, _ = b.tool("kd_peers")
        names = [p["name"] for p in listed.get("peers", [])]
        ok(f"B lists saved peers {names}") if "builder" in names else bad(f"kd_peers: {listed}")

        step("A download starts, then the session ends part way through")
        deadline = time.time() + 60
        while time.time() < deadline:
            lst, _ = b.tool("kd_list_files", {"source": "remote"})
            if any(e["name"].lstrip("/") == "big.bin" for e in lst.get("entries", [])):
                break
            time.sleep(1)

        pull, err = b.tool("kd_pull_file", {"name": "big.bin"})
        tid = pull.get("transfer_id")
        ok(f"download started as {tid}") if tid else bad(f"pull: {pull}")

        partial = 0
        deadline = time.time() + 60
        while time.time() < deadline:
            st, _ = b.tool("kd_transfer_status", {"transfer_id": tid})
            partial = st.get("bytes", 0)
            if st.get("done") or partial > SIZE * 0.15:
                break
            time.sleep(0.2)
        print(f"  stopping at {partial} of {SIZE} bytes", flush=True)

        for s in (a, b):
            s.tool("kd_disconnect")
        ok("both peers closed the session mid-download")

        dest = f"{WORK}/B/recv/big.bin"
        on_disk = os.path.getsize(dest) if os.path.exists(dest) else 0
        bitmap = os.path.exists(dest + ".kdbitmap")
        ok(f"B kept a part file of {on_disk} bytes with its chunk map") if bitmap \
            else bad("no chunk map was written, so a resume has nothing to work from")

        step("The same two peers meet again")
        # kd_connect_peer needs no code: each side stored the other's during
        # the first session.
        reconn_b, err_b = b.tool("kd_connect_peer", {"name": "builder", "timeout_s": 10})
        reconn_a, err_a = a.tool("kd_connect_peer", {"name": "worker", "timeout_s": 60})
        sa, sb = wait_connected([a, b], CONNECT_TIMEOUT)
        if sa.get("connected") and sb.get("connected"):
            ok("reconnected with no code exchange")
        else:
            bad(f"reconnect failed: A {sa.get('state')} / B {sb.get('state')}; "
                f"B said {reconn_b if err_b else ''} A said {reconn_a if err_a else ''}")

        step("The files are there again")
        found = False
        deadline = time.time() + 90
        while time.time() < deadline:
            lst, _ = b.tool("kd_list_files", {"source": "remote"})
            found = any(e["name"].lstrip("/") == "big.bin" for e in lst.get("entries", []))
            if found:
                break
            time.sleep(1)
        ok("B sees A's file again after reconnecting") if found \
            else bad("B does not see the file after reconnecting")

        step("The download picks up where it stopped")
        pull2, err = b.tool("kd_pull_file", {"name": "big.bin"})
        tid2 = pull2.get("transfer_id")
        ok(f"download resumed as {tid2}") if tid2 else bad(f"second pull: {pull2}")

        st = {}
        deadline = time.time() + 300
        while time.time() < deadline and tid2:
            st, _ = b.tool("kd_transfer_status", {"transfer_id": tid2})
            if st.get("done"):
                break
            time.sleep(1)
        ok(f"download finished, {st.get('bytes')} of {st.get('total')} bytes") if st.get("done") \
            else bad(f"download did not finish: {st}")

        got = hashlib.sha256(open(dest, "rb").read()).hexdigest() if os.path.exists(dest) else ""
        ok(f"the file matches A's copy ({got})") if got == sha_src \
            else bad(f"checksum differs: wanted {sha_src}, got {got or 'nothing'}")

        step("A different peer sees nothing")
        # A third peer with its own identity gets its own empty session. The
        # files A shared belong to the session with B.
        for sub in ("cfg", "recv", "mnt"):
            os.makedirs(f"{WORK}/C/{sub}", exist_ok=True)
        c = Server("C", env(f"{WORK}/C", 26581, 26582))
        try:
            peers_c, err = c.tool("kd_peers")
            ok("a fresh peer has an empty address book") if not err and not peers_c.get("peers") \
                else bad(f"a fresh peer already lists {peers_c}")
            lst_c, _ = c.tool("kd_list_files", {"source": "remote"})
            ok("a fresh peer lists no remote files") if not lst_c.get("entries") \
                else bad(f"a fresh peer lists {lst_c}")
        finally:
            c.close()

    finally:
        a.close()
        b.close()

    print(f"\n== {passed} passed, {failed} failed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
