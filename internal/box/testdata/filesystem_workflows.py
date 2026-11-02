import ctypes
import errno
import mmap
import os

libc = ctypes.CDLL(None, use_errno=True)
libc.mprotect.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_int]
libc.renameat2.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]


def denied(call, *args):
    try:
        call(*args)
    except OSError as error:
        assert error.errno == errno.EPERM, (call, args, error)
    else:
        raise AssertionError((call, args, "was allowed"))


for name in [".env", ".ENV", "a-secret-alias", "private/key", "PRIVATE/KEY", "Café", "CAFE\u0301", "hidden-link.pem"]:
    denied(open, name)
    denied(os.stat, name)
    denied(os.listxattr, name)
    denied(os.unlink, name)
    denied(os.rename, name, "moved-secret")
    denied(os.link, name, "linked-secret")
    denied(os.chmod, name, 0o600)
    denied(os.utime, name)
denied(os.readlink, "hidden-link.pem")
denied(os.rename, "private", "moved-private")
denied(os.rename, "public", ".env")
denied(os.link, "public", "private/new")
denied(os.symlink, "public", "private/new")
denied(os.mkdir, "private/new")
for source, target in [(b".env", b"public"), (b"public", b".env")]:
    assert libc.renameat2(-100, source, -100, target, 2) == -1
    assert ctypes.get_errno() == errno.EPERM

# The policy remains readable. Private writable mappings change only memory.
with open(".coopignore", "rb") as file:
    before = file.read()
    view = mmap.mmap(file.fileno(), 0, flags=mmap.MAP_PRIVATE, prot=mmap.PROT_READ | mmap.PROT_WRITE)
    buffer = (ctypes.c_char * len(view)).from_buffer(view)
    address = ctypes.addressof(buffer)
    assert libc.mprotect(address, len(view), mmap.PROT_READ) == 0
    assert libc.mprotect(address, len(view), mmap.PROT_READ | mmap.PROT_WRITE) == 0
    view[0:1] = b"X"
    del buffer
    view.close()
    file.seek(0)
    assert file.read() == before
for name in [".coopignore", "policy-alias"]:
    denied(open, name, "w")
    denied(os.unlink, name)
    denied(os.rename, name, "moved-policy")
    denied(os.link, name, "new-policy-alias")
    denied(os.chmod, name, 0o600)
    denied(os.utime, name)
    denied(os.setxattr, name, "user.coop", b"x")
    denied(os.removexattr, name, "user.coop")

# Ordinary public hardlinks, atomic replacements, and directory moves work.
os.link("public", "public-link")
assert open("public-link").read() == "public fixture"
with open("public-next", "w") as file:
    file.write("edited")
os.replace("public-next", "public")
assert open("public-link").read() == "public fixture"
assert open("public").read() == "edited"
assert libc.renameat2(-100, b"public", -100, b"public-link", 2) == 0
os.mkdir("public-dir")
os.rename("public-dir", "public-moved")
print("case, Unicode, metadata, mutation, private mappings and public workflows passed")
