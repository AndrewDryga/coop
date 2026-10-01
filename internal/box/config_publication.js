const fs = require("node:fs");
const crypto = require("node:crypto");
const path = require("node:path");
const limit = 4 * 1024 * 1024; // Keep aligned with agents.MaxNativeConfigBytes.

// A host rename can expose stale content and metadata through a VM bind mount.
// Verify the published bytes, not just valid JSON.
function published(file) {
  let fd;
  try {
    fd = fs.openSync(file.path, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
    const before = fs.fstatSync(fd);
    if (!before.isFile() || before.size > limit) return false;
    const hash = crypto.createHash("sha256");
    const chunk = Buffer.alloc(4096);
    let size = 0;
    for (;;) {
      const n = fs.readSync(fd, chunk, 0, chunk.length, null);
      if (!n) break;
      size += n;
      if (size > limit) return false;
      hash.update(chunk.subarray(0, n));
    }
    const after = fs.fstatSync(fd);
    return size === before.size && size === after.size && before.dev === after.dev &&
      before.ino === after.ino && before.mtimeMs === after.mtimeMs &&
      before.ctimeMs === after.ctimeMs && hash.digest("hex") === file.digest;
  } catch {
    return false;
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
  }
}

try {
  for (const file of JSON.parse(process.env.COOP_CONFIG_PUBLICATION)) {
    let ready = false;
    for (let attempt = 0; attempt < 3 && !ready; attempt++) ready = published(file);
    if (!ready) throw new Error("agent config publication did not converge: " + path.basename(file.path));
  }
} catch (error) {
  console.error(error.message);
  process.exitCode = 1;
}
