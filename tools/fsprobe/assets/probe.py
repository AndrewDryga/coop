"""Inert qualification workload. Only paths in the generated fixture are touched."""
import ctypes
import errno
import json
import os
from pathlib import Path
import sys


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
             "Cafe\u0301.pem", "key.pem", "\u212aEY.PEM", "private/key", "PRIVATE/key",
             ".coopignore", ".COOPIGNORE", "policy-link", "policy-hard", "public.txt"]
    if mode == "replacement":
        # Open the newly created hardlink FIRST, before a protected name could
        # classify the replacement inode. Reserve this spelling until now.
        paths = ["replacement-hard", ".eNV"] + paths
    if mode == "unicode":
        paths = ["café.pem", "cAFÉ.PEM", "caFe\u0301.PeM", "\u212aey.PeM"] + paths
    print(json.dumps({path: read(root / path) for path in dict.fromkeys(paths)}))
elif mode == "ancestor":
    print(json.dumps(read(root / "nested/.env")))
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
