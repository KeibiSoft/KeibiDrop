#!/usr/bin/env python3
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# One pane of the two-machine demo: an agent driving a real kdmcp server and
# showing its tool calls the way an assistant shows them.
#
# Every call here is real. The two panes find each other's code through a
# rendezvous directory, which stands in for the humans or the channel that
# would carry it.
#
#   agent_pane.py A|B <rendezvous-dir> <work-dir>

import hashlib
import json
import os
import random
import shutil
import subprocess
import sys
import time

ROLE = sys.argv[1]
RV = sys.argv[2]
WORK = sys.argv[3]
KDMCP = os.environ.get("KDMCP", "kdmcp")

# Three colours and nothing else: grey for the frame, near-white for text,
# one accent for the tool calls and the values that matter.
DIM = "\033[38;5;243m"
FG = "\033[38;5;253m"
ACC = "\033[38;5;80m"
B = "\033[1m"
R = "\033[0m"

MACHINE = "laptop" if ROLE == "A" else "gpu box"
PROMPT = ("share the dataset with the gpu box" if ROLE == "A"
          else "read the dataset from the laptop")


def slow(text, delay=0.012):
    for ch in text:
        sys.stdout.write(ch)
        sys.stdout.flush()
        time.sleep(delay)
    sys.stdout.write("\n")
    sys.stdout.flush()


def user_line(text):
    sys.stdout.write(f"{DIM}› {R}{FG}")
    slow(text, 0.018)
    sys.stdout.write(R)
    print()


def call(name, arg=None):
    shown = f"{ACC}{name}{R}"
    if arg:
        shown += f"{DIM}({arg}){R}"
    print(f"{ACC}⏺{R} {shown}", flush=True)


def result(text, accent=False):
    body = f"{ACC}{text}{R}" if accent else f"{FG}{text}{R}"
    print(f"  {DIM}⎿{R} {body}", flush=True)
    time.sleep(0.25)


def note(text):
    print(f"    {DIM}{text}{R}", flush=True)


def shell(cmd, out_lines):
    print(f"{DIM}${R} {FG}{cmd}{R}", flush=True)
    time.sleep(0.3)
    for l in out_lines:
        print(f"  {DIM}{l}{R}", flush=True)
        time.sleep(0.12)


class MCP:
    """A real kdmcp subprocess, spoken to over stdio."""

    def __init__(self, env):
        full = dict(os.environ)
        full.update(env)
        self.p = subprocess.Popen(
            [KDMCP], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=open(f"{env['KD_LOG_FILE']}.err", "w"),
            text=True, bufsize=1, env=full,
        )
        self.n = 0
        self.rpc("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                "clientInfo": {"name": "agent", "version": "1"}})

    def rpc(self, method, params=None):
        self.n += 1
        req = {"jsonrpc": "2.0", "id": self.n, "method": method}
        if params is not None:
            req["params"] = params
        self.p.stdin.write(json.dumps(req) + "\n")
        self.p.stdin.flush()
        return json.loads(self.p.stdout.readline())

    def tool(self, name, args=None):
        res = self.rpc("tools/call", {"name": name, "arguments": args or {}})["result"]
        return json.loads(res["content"][0]["text"]), res.get("isError", False)

    def close(self):
        try:
            self.p.stdin.close()
            self.p.wait(timeout=10)
        except Exception:
            self.p.kill()


def build_dataset(root, depth=5, per=3, files=6):
    rng = random.Random(11)
    made = {}

    def fill(path, level):
        os.makedirs(path, exist_ok=True)
        for i in range(files):
            body = bytes(rng.getrandbits(8) for _ in range(rng.randint(400, 6000)))
            full = os.path.join(path, f"shard{i}.bin")
            with open(full, "wb") as fh:
                fh.write(body)
            made[os.path.relpath(full, root).replace(os.sep, "/")] = hashlib.sha256(body).hexdigest()
        if level < depth:
            for d in range(per):
                fill(os.path.join(path, f"part{d}"), level + 1)

    fill(root, 0)
    return made


def put(name, value):
    with open(os.path.join(RV, name), "w") as fh:
        fh.write(value)


def get(name, timeout=180):
    path = os.path.join(RV, name)
    deadline = time.time() + timeout
    while time.time() < deadline:
        if os.path.exists(path):
            v = open(path).read().strip()
            if v:
                return v
        time.sleep(0.2)
    return ""


def main():
    root = f"{WORK}/{ROLE}"
    data = f"{root}/dataset"
    for sub in ("cfg", "mnt", "recv"):
        os.makedirs(f"{root}/{sub}", exist_ok=True)

    digests = {}
    total_bytes = 0
    if ROLE == "A":
        digests = build_dataset(data)
        total_bytes = sum(os.path.getsize(os.path.join(data, p)) for p in digests)

    env = {
        "KEIBIDROP_CONFIG_DIR": f"{root}/cfg",
        # A points save_path at the dataset and turns on the start-up scan, so
        # the folder is offered where it sits when the session comes up.
        "KD_SAVE_PATH": data if ROLE == "A" else f"{root}/recv",
        "KD_MOUNT_PATH": f"{root}/mnt",
        "KD_LOG_FILE": f"{root}/kd.log",
        "KD_INBOUND_PORT": "26601" if ROLE == "A" else "26611",
        "KD_OUTBOUND_PORT": "26602" if ROLE == "A" else "26612",
        "KD_INCOGNITO": "1",
        "KD_SHARE_ROOT": "",
    }
    if ROLE == "A":
        env["KEIBIDROP_SCAN_SHARED_ON_START"] = "1"

    print(f"{DIM}{'─' * 56}{R}")
    print(f"{B}{FG}  agent on the {MACHINE}{R}")
    print(f"{DIM}{'─' * 56}{R}\n")
    time.sleep(0.6 if ROLE == "A" else 1.0)

    user_line(PROMPT)

    kd = MCP(env)
    try:
        call("kd_setup")
        setup, _ = kd.tool("kd_setup")
        if ROLE == "A":
            # Publish what is being offered so the other pane can wait for the
            # listing to finish arriving instead of reading it half-filled.
            put("A.count", str(len(digests)))
            put("A.bytes", str(total_bytes))
            result(f"{len(digests):,} files ready to offer, in place", accent=True)
        else:
            result(f"mount mode: {setup['mode']}", accent=True)
            # The real listing of the mount point before anything is
            # connected. Whatever shows up later arrived over the session; it
            # was not sitting on this disk.
            cold = sorted(os.listdir(setup["mount_path"])) or ["(nothing here)"]
            shell("ls mnt/", cold[:4])
            print()

        call("kd_create_share")
        mine, _ = kd.tool("kd_create_share")
        code = mine["code"]
        result(f"my code {code[:10]}…")
        put(f"{ROLE}.code", code)

        other = get("B.code" if ROLE == "A" else "A.code")
        call("kd_accept_peer", f'code: "{other[:10]}…"')
        kd.tool("kd_accept_peer", {"code": other})
        result("codes exchanged once, both directions authenticated")

        call("kd_status")
        deadline = time.time() + 180
        st = {}
        while time.time() < deadline:
            st, _ = kd.tool("kd_status")
            if st.get("connected"):
                break
            time.sleep(0.4)
        result(f"connected, encrypted end to end, over {st.get('mode')}", accent=True)

        if ROLE == "A":
            note("the dataset stays on this machine")
            print()

            get("B.listed", 120)
            call("kd_status")
            st, _ = kd.tool("kd_status")
            listed = st.get("bytes_out", 0)
            result(f"{listed:,} bytes sent")
            note("that is the listing. no file content has moved.")
            print()

            print(f"  {DIM}the gpu box is working through the shards{R}", flush=True)
            get("B.done", 300)
            call("kd_status")
            st, _ = kd.tool("kd_status")
            moved = st.get("bytes_out", 0)
            result(f"{moved:,} bytes sent, {moved - listed:,} of it file content",
                   accent=True)
            note(f"the dataset on disk here is {total_bytes:,} bytes")
            note("it sent the parts that were opened, and nothing else")
            put("A.done", "1")

        else:
            call("kd_list_files", 'source: "remote"')
            expected = int(get("A.count", 60) or 0)
            total_bytes = int(get("A.bytes", 60) or 0)
            deadline = time.time() + 120
            entries = []
            while time.time() < deadline:
                lst, _ = kd.tool("kd_list_files", {"source": "remote"})
                entries = lst.get("entries", [])
                if expected and len(entries) >= expected:
                    break
                time.sleep(0.3)
            depth = max(e["name"].lstrip("/").count("/") for e in entries) + 1
            result(f"{len(entries):,} files, {depth} folders deep", accent=True)
            note("nothing downloaded")
            put("B.listed", "1")
            print()

            mount = st.get("read_peer_files_at") or st.get("mount_path")
            names = sorted(e["name"].lstrip("/") for e in entries)
            deep = sorted(names, key=lambda n: n.count("/"))[-1]
            target = os.path.join(mount, deep)
            waited = time.time()
            while not os.path.exists(target) and time.time() - waited < 90:
                time.sleep(0.5)

            # Walk into the tree, listing with sizes at three depths. Each of
            # these is a cold read: the names and sizes come off the session,
            # and no file content has been fetched yet.
            parts = deep.split("/")[:-1]
            for depth_at in (1, 3, len(parts)):
                rel = "/".join(parts[:depth_at])
                d = os.path.join(mount, rel)
                if not os.path.isdir(d):
                    continue
                rows = []
                for n in sorted(os.listdir(d))[:4]:
                    full = os.path.join(d, n)
                    if os.path.isdir(full):
                        rows.append(f"{'dir':>9}  {n}/")
                    else:
                        rows.append(f"{os.path.getsize(full):>9,}  {n}")
                shell(f"ls -l mnt/{rel}/", rows + ["…"])
            note("names and sizes only; no file content has been read yet")
            print()

            # First byte off a file nothing has touched: that wait is the round
            # trip to the other machine.
            before = kd.tool("kd_status")[0]["bytes_in"]
            t = time.time()
            with open(target, "rb") as fh:
                head = fh.read(1)
            rtt = (time.time() - t) * 1000
            print(f"{DIM}${R} {FG}head -c 1 mnt/{deep.split('/')[-1]}{R}", flush=True)
            print(f"  {ACC}first byte in {rtt:.0f} ms{R}   {DIM}round trip to the laptop{R}",
                  flush=True)
            time.sleep(1.2)
            print()

            # The work itself: read a batch of shards and checksum them.
            batch = [n for n in names if n.count("/") >= 3][:24]
            print(f"{DIM}› {R}{FG}checksum 24 shards and report throughput{R}\n", flush=True)
            call("Read", "24 files under mnt/")
            t = time.time()
            read_bytes = 0
            digest = hashlib.sha256()
            for n in batch:
                p = os.path.join(mount, n)
                try:
                    with open(p, "rb") as fh:
                        blob = fh.read()
                except OSError:
                    continue
                read_bytes += len(blob)
                digest.update(blob)
            took = time.time() - t
            after = kd.tool("kd_status")[0]["bytes_in"]
            moved = after - before

            result(f"{len(batch)} shards, {read_bytes:,} bytes in {took:.2f}s", accent=True)
            note(f"{read_bytes / took / 1024:,.0f} KB/s through the mount")
            note(f"rolling sha256 {digest.hexdigest()[:32]}…")
            print()
            print(f"  {ACC}{moved:,} bytes crossed the network{R}", flush=True)
            note(f"the dataset is {total_bytes:,} bytes; it read what it opened")
            put("B.done", "1")

        time.sleep(4)
    finally:
        kd.close()


if __name__ == "__main__":
    sys.exit(main())
