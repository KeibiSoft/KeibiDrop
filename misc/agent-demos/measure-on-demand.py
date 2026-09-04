#!/usr/bin/env python3
# SPDX-License-Identifier: MPL-2.0
# Copyright (c) 2025 KeibiSoft S.R.L.
#
# How much of a file crosses the network when a program reads part of it.
#
# A offers a large file. B reads a small slice of it through the mount, and we
# compare the bytes that moved against the size of the file.
#
# Usage: KDMCP=/path/to/kdmcp uv run measure-on-demand.py [workdir]

import hashlib
import json
import os
import shutil
import subprocess
import sys
import time

KDMCP = os.environ.get("KDMCP", "kdmcp")
WORK = sys.argv[1] if len(sys.argv) > 1 else "/tmp/kd-ondemand"
SIZE = int(os.environ.get("SIZE", str(256 * 1024 * 1024)))
SLICE = int(os.environ.get("SLICE", str(1024 * 1024)))


class Server:
    def __init__(self, env):
        full = dict(os.environ); full.update(env)
        self.p = subprocess.Popen([KDMCP], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=open(f"{env['KD_LOG_FILE']}.err", "w"),
                                  text=True, bufsize=1, env=full)
        self.n = 0
        self.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                 "clientInfo": {"name": "m", "version": "1"}})

    def call(self, m, params=None):
        self.n += 1
        r = {"jsonrpc": "2.0", "id": self.n, "method": m}
        if params is not None:
            r["params"] = params
        self.p.stdin.write(json.dumps(r) + "\n"); self.p.stdin.flush()
        return json.loads(self.p.stdout.readline())

    def tool(self, name, args=None):
        res = self.call("tools/call", {"name": name, "arguments": args or {}})["result"]
        return json.loads(res["content"][0]["text"]), res.get("isError", False)

    def close(self):
        try:
            self.p.stdin.close(); self.p.wait(timeout=10)
        except Exception:
            self.p.kill()


def env(root, inb, outb, share=None):
    return {"KEIBIDROP_CONFIG_DIR": f"{root}/cfg", "KD_SAVE_PATH": f"{root}/recv",
            "KD_MOUNT_PATH": f"{root}/mnt", "KD_LOG_FILE": f"{root}/kd.log",
            "KD_INBOUND_PORT": str(inb), "KD_OUTBOUND_PORT": str(outb),
            "KD_INCOGNITO": "1", "KD_SHARE_ROOT": share or ""}


def main():
    shutil.rmtree(WORK, ignore_errors=True)
    for s in ("A/cfg", "A/mnt", "A/recv", "A/share", "B/cfg", "B/mnt", "B/recv"):
        os.makedirs(f"{WORK}/{s}", exist_ok=True)

    payload = os.urandom(SIZE)
    with open(f"{WORK}/A/share/big.bin", "wb") as fh:
        fh.write(payload)
    print(f"file on A: {SIZE:,} bytes")

    a = Server(env(f"{WORK}/A", 26591, 26592, share=f"{WORK}/A/share"))
    b = Server(env(f"{WORK}/B", 26595, 26596))
    try:
        ca = a.tool("kd_create_share")[0]["code"]
        cb = b.tool("kd_create_share")[0]["code"]
        a.tool("kd_accept_peer", {"code": cb})
        b.tool("kd_accept_peer", {"code": ca})
        t = time.time()
        while time.time() - t < 180:
            sa, _ = a.tool("kd_status"); sb, _ = b.tool("kd_status")
            if sa.get("connected") and sb.get("connected"):
                break
            time.sleep(0.5)
        print(f"connected over {sa.get('mode')}, fuse={sb.get('fuse')}")
        if not sb.get("fuse"):
            print("FUSE is off on B, so there is no mount to measure")
            return 1

        a.tool("kd_send_file", {"path": "big.bin"})
        mount = sb.get("read_peer_files_at") or sb.get("mount_path")
        target = os.path.join(mount, "big.bin")
        t = time.time()
        while not os.path.exists(target) and time.time() - t < 90:
            time.sleep(0.5)
        if not os.path.exists(target):
            print(f"the file never appeared at {target}")
            return 1

        st = os.stat(target)
        print(f"B sees big.bin through the mount, size {st.st_size:,} bytes")

        # Baseline the counters, then read one slice from the middle.
        before, _ = b.tool("kd_status")
        base_in = before["bytes_in"]

        offset = SIZE // 2
        t = time.time()
        with open(target, "rb") as fh:
            fh.seek(offset)
            chunk = fh.read(SLICE)
        elapsed = time.time() - t

        after, _ = b.tool("kd_status")
        moved = after["bytes_in"] - base_in

        okc = chunk == payload[offset:offset + SLICE]
        print()
        print(f"read {len(chunk):,} bytes from the middle in {elapsed:.2f}s")
        print(f"bytes B received off the wire for that read: {moved:,}")
        print(f"file size:                                   {SIZE:,}")
        print(f"fraction of the file that moved:             {moved / SIZE * 100:.2f}%")
        print(f"content matches A: {okc}")
        print()
        print("A copy would have moved the whole file.")
    finally:
        a.close(); b.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
