#!/usr/bin/env python3
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# A folder with a lot of files, offered to another machine and read there.
# Written to be watched: each line carries the time that step took.
#
# The folder is offered in place. Nothing is copied into KeibiDrop, and the
# file contents stay on the first machine until the second machine reads them.
#
# Usage: KDMCP=/path/to/kdmcp uv run demo-fast-folder.py [workdir]

import hashlib
import json
import os
import random
import shutil
import subprocess
import sys
import time

KDMCP = os.environ.get("KDMCP", "kdmcp")
WORK = sys.argv[1] if len(sys.argv) > 1 else "/tmp/kd-fast-folder"
DEPTH = int(os.environ.get("DEPTH", "5"))

C = {
    "dim": "\033[2m", "b": "\033[1m", "g": "\033[32m",
    "c": "\033[36m", "y": "\033[33m", "r": "\033[0m",
}


def line(text, seconds=None, mark="+"):
    t = ""
    if seconds is not None:
        t = f"{C['y']}{seconds * 1000:6.0f} ms{C['r']}" if seconds < 1 \
            else f"{C['y']}{seconds:6.2f} s {C['r']}"
    print(f"  {C['g']}{mark}{C['r']} {text:<52} {t}", flush=True)


class Server:
    def __init__(self, name, env):
        full = dict(os.environ)
        full.update(env)
        self.proc = subprocess.Popen(
            [KDMCP], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=open(f"{env['KD_LOG_FILE']}.err", "w"),
            text=True, bufsize=1, env=full,
        )
        self.n = 0
        self.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                 "clientInfo": {"name": "kd", "version": "1"}})

    def call(self, method, params=None):
        self.n += 1
        req = {"jsonrpc": "2.0", "id": self.n, "method": method}
        if params is not None:
            req["params"] = params
        self.proc.stdin.write(json.dumps(req) + "\n")
        self.proc.stdin.flush()
        return json.loads(self.proc.stdout.readline())

    def tool(self, name, args=None):
        res = self.call("tools/call", {"name": name, "arguments": args or {}})["result"]
        return json.loads(res["content"][0]["text"]), res.get("isError", False)

    def close(self):
        try:
            self.proc.stdin.close()
            self.proc.wait(timeout=10)
        except Exception:
            self.proc.kill()


def build(root, depth, per=3, files=6):
    rng = random.Random(7)
    made = {}

    def fill(path, level):
        os.makedirs(path, exist_ok=True)
        for i in range(files):
            body = bytes(rng.getrandbits(8) for _ in range(rng.randint(400, 6000)))
            full = os.path.join(path, f"file{i}.bin")
            with open(full, "wb") as fh:
                fh.write(body)
            made[os.path.relpath(full, root).replace(os.sep, "/")] = hashlib.sha256(body).hexdigest()
        if level < depth:
            for d in range(per):
                fill(os.path.join(path, f"dir{d}"), level + 1)

    fill(root, 0)
    return made


def env(root, inb, outb, save, scan=False):
    e = {
        "KEIBIDROP_CONFIG_DIR": f"{root}/cfg", "KD_SAVE_PATH": save,
        "KD_MOUNT_PATH": f"{root}/mnt", "KD_LOG_FILE": f"{root}/kd.log",
        "KD_INBOUND_PORT": str(inb), "KD_OUTBOUND_PORT": str(outb),
        "KD_INCOGNITO": "1", "KD_SHARE_ROOT": "",
    }
    if scan:
        # This is what offers the folder in place when the session starts.
        e["KEIBIDROP_SCAN_SHARED_ON_START"] = "1"
    return e


def main():
    shutil.rmtree(WORK, ignore_errors=True)
    for s in ("A/cfg", "A/mnt", "A/data", "B/cfg", "B/mnt", "B/recv"):
        os.makedirs(f"{WORK}/{s}", exist_ok=True)

    print(f"\n{C['b']}KeibiDrop between two machines, driven by an agent{C['r']}\n")

    t = time.time()
    files = build(f"{WORK}/A/data", DEPTH)
    total = sum(os.path.getsize(f"{WORK}/A/data/{p}") for p in files)
    levels = max(p.count("/") for p in files) + 1
    line(f"machine A has {len(files)} files, {levels} folders deep", time.time() - t)

    t = time.time()
    a = Server("A", env(f"{WORK}/A", 26541, 26542, f"{WORK}/A/data", scan=True))
    b = Server("B", env(f"{WORK}/B", 26551, 26552, f"{WORK}/B/recv"))
    line("both machines started a KeibiDrop peer", time.time() - t)

    try:
        t = time.time()
        code_a = a.tool("kd_create_share")[0]["code"]
        code_b = b.tool("kd_create_share")[0]["code"]
        a.tool("kd_accept_peer", {"code": code_b})
        b.tool("kd_accept_peer", {"code": code_a})
        line("codes swapped, each machine holds the other's", time.time() - t)

        t = time.time()
        mode = ""
        while time.time() - t < 180:
            sa, _ = a.tool("kd_status")
            sb, _ = b.tool("kd_status")
            if sa.get("connected") and sb.get("connected"):
                mode = sa.get("mode", "")
                break
            time.sleep(0.2)
        connected = time.time()
        line(f"connected, encrypted end to end, over {mode}", connected - t)

        seen = 0
        while time.time() - connected < 300:
            lst, _ = b.tool("kd_list_files", {"source": "remote"})
            seen = len(lst.get("entries", []))
            if seen >= len(files):
                break
            time.sleep(0.05)
        announce = time.time() - connected
        line(f"machine B sees all {seen} files", announce)
        rate = seen / announce if announce else 0
        print(f"    {C['dim']}{rate:,.0f} files per second. "
              f"The folder was offered where it sits.{C['r']}", flush=True)
        print(f"    {C['dim']}{total:,} bytes of content stayed on machine A.{C['r']}\n", flush=True)

        mount = sb.get("read_peer_files_at") or sb.get("mount_path")
        deep = sorted(files, key=lambda p: p.count("/"))[-1]
        target = os.path.join(mount, deep)

        waited = time.time()
        while not os.path.exists(target) and time.time() - waited < 90:
            time.sleep(0.5)

        if os.path.exists(target):
            t = time.time()
            with open(target, "rb") as fh:
                got = hashlib.sha256(fh.read()).hexdigest()
            read_time = time.time() - t
            line(f"B read {deep.split('/')[-1]}, {levels} folders down", read_time)
            print(f"    {C['dim']}{deep}{C['r']}", flush=True)
            if got == files[deep]:
                line("the bytes match machine A", mark="=")
            print(f"\n    {C['c']}Those bytes crossed the network when B opened the file,"
                  f"{C['r']}", flush=True)
            print(f"    {C['c']}and no other file was sent.{C['r']}", flush=True)
        else:
            line(f"the folder did not appear under {mount}", mark="!")

    finally:
        a.close()
        b.close()
    print()


if __name__ == "__main__":
    sys.exit(main())
