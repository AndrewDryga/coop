"""Inert qualification workload. Only paths in the generated fixture are touched."""
import ctypes
import errno
import json
import mmap
import os
from pathlib import Path
import sys
import time


def read(path):
    try:
        with open(path, "rb") as stream:
            body = stream.read(256)
        text = body.decode("utf-8", errors="replace")
        if text not in ("", "INERT_SECRET", "INERT_REPLACEMENT", "INERT_UNICODE_REPLACEMENT",
                        "INERT_ANCESTOR_REPLACEMENT", "fixture policy\n", "PUBLIC",
                        "PUBLIC_HOST_EDIT", "GUEST_PUBLIC_EDIT", "MUTATED") and not text.startswith(".coop-fsprobe-"):
            return {"result": "unexpected-data"}
        return {"result": "read", "body": text}
    except OSError as error:
        return {"result": "error", "errno": error.errno}


root, mode = Path(sys.argv[1]), sys.argv[2]
if mode == "identity":
    print(json.dumps({
        "cgroup": os.stat("/sys/fs/cgroup").st_ino,
        "mountinfo": Path("/proc/self/mountinfo").read_text(),
        "status": Path("/proc/self/status").read_text(),
        "fixture": read(root / "identity"),
    }))
elif mode in ("read", "replacement", "unicode"):
    # Process names are deliberately changed before every access sequence.
    libc = ctypes.CDLL(None, use_errno=True)
    if libc.prctl(15, b"ordinary-editor", 0, 0, 0):
        raise OSError(ctypes.get_errno(), "prctl")
    paths = [".env", ".ENV", ".Env", ".eNv", ".enV", ".EnV", ".ENv",
             "env-link", "env-hard", "replacement-hard", "Café.pem", "CAFÉ.PEM",
             "Cafe\u0301.pem", "Café", "key.pem", "\u212aEY.PEM", "private/key", "PRIVATE/key",
             ".coopignore", ".COOPIGNORE", "policy-link", "policy-hard", "public.txt"]
    if mode == "replacement":
        # Open the newly created hardlink FIRST, before a protected name could
        # classify the replacement inode. Reserve this spelling until now.
        paths = ["replacement-hard", ".eNV"] + paths
    if mode == "unicode":
        paths = ["café.pem", "cAFÉ.PEM", "caFe\u0301.PeM", "\u212aey.PeM", "cAFÉ", "caFe\u0301"] + paths
    print(json.dumps({path: read(root / path) for path in dict.fromkeys(paths)}))
elif mode == "ancestor":
    print(json.dumps(read(root / "nested/.env")))
elif mode == "open-first":
    # No stat: the kernel observation must precede any userspace refresh.
    print(json.dumps(read(root / sys.argv[3])))
elif mode == "guest-alias":
    # Mutation FIRST: reading the secret before linking/renaming would let an
    # inode-learning policy hide its failure to enforce the mutation itself.
    try:
        if sys.argv[3] == "link":
            os.link(root / ".env", root / "guest-alias")
        elif sys.argv[3] == "rename":
            os.rename(root / ".env", root / "guest-alias")
        elif sys.argv[3] == "replace":
            (root / "guest-save").write_text("MUTATED")
            os.replace(root / "guest-save", root / ".env")
        else:
            raise ValueError(sys.argv[3])
        outcome = {"result": "mutated"}
    except OSError as error:
        outcome = {"result": "error", "errno": error.errno}
    print(json.dumps({"outcome": outcome, "alias": read(root / "guest-alias")}))
elif mode == "hold":
    # Keep a previously permitted descriptor and mapping while the HOST adds a
    # protected alias and edits the inode. This is distinct from a new open.
    with open(root / "held-public", "rb") as stream:
        with mmap.mmap(stream.fileno(), 0, access=mmap.ACCESS_READ) as mapping:
            (root / "held-ready").write_text("READY")
            deadline = time.monotonic() + 8
            while not (root / "held-release").exists():
                if time.monotonic() >= deadline:
                    raise TimeoutError("host did not release held descriptor")
                time.sleep(0.02)
            fd_body = stream.read(256).decode("utf-8")
            map_body = mapping[:256].decode("utf-8")
            print(json.dumps({"descriptor": fd_body, "mapping": map_body,
                              "fresh_open": read(root / "held-public")}))
elif mode == "mutate":
    operation, spelling = sys.argv[3:5]
    target = root / spelling
    before = read(target)
    try:
        if operation == "write":
            with open(target, "r+b") as stream:
                stream.write(b"MUTATED")
        elif operation == "truncate":
            os.truncate(target, 0)
        elif operation == "unlink":
            target.unlink()
        elif operation == "rename":
            target.rename(root / "moved-policy")
        elif operation == "replace":
            (root / "policy-save").write_text("MUTATED")
            os.replace(root / "policy-save", target)
        elif operation == "link":
            os.link(target, root / "new-policy-link")
        else:
            raise ValueError(operation)
        outcome = {"result": "mutated"}
    except OSError as error:
        outcome = {"result": "error", "errno": error.errno}
    print(json.dumps({"before": before, "outcome": outcome}))
elif mode == "public":
    path = root / "new-public"
    path.write_text("GUEST_PUBLIC_EDIT")
    path.rename(root / "renamed-public")
    print(json.dumps({"created": read(root / "renamed-public"), "host": read(root / "public.txt")}))
elif mode == "custody":
    failures = {}
    for label, path in [("cgroup", "/sys/fs/cgroup/cgroup.procs"), ("loader", "/probe/loader.c")]:
        try:
            fd = os.open(path, os.O_WRONLY)
            os.close(fd)
            failures[label] = "writable"
        except OSError as error:
            failures[label] = error.errno
    failures["status"] = Path("/proc/self/status").read_text()
    print(json.dumps(failures))
else:
    raise ValueError(mode)
