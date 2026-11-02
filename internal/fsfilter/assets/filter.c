/* SPDX-License-Identifier: MIT
 * Admission-owned denial sets. Workloads never receive map/program descriptors.
 * Compile with clang -target bpfel; CO-RE relocates the minimal kernel views.
 */
#include <linux/bpf.h>

typedef unsigned char u8;
typedef unsigned int u32;
typedef unsigned long long u64;
#define SEC(s) __attribute__((section(s), used))
#define CORE __attribute__((preserve_access_index))
#define INLINE static __attribute__((always_inline)) inline
#define NOINLINE static __attribute__((noinline))
#define UINT(name, val) int (*name)[val]
#define TYPE(name, val) val *name

struct super_block { unsigned int s_dev; } CORE;
struct inode { struct super_block *i_sb; unsigned long i_ino; } CORE;
struct qstr { const unsigned char *name; } CORE;
struct dentry { struct inode *d_inode; struct dentry *d_parent; struct qstr d_name; } CORE;
struct path { struct dentry *dentry; } CORE;
struct file { struct path f_path; unsigned int f_flags; } CORE;
struct vm_area_struct { struct file *vm_file; unsigned long vm_flags; } CORE;
struct task_struct { unsigned int flags; int exit_state; struct task_struct *group_leader; } CORE;

static void *(*lookup)(void *, const void *) = (void *)BPF_FUNC_map_lookup_elem;
static long (*read_str)(void *, u32, const void *) = (void *)BPF_FUNC_probe_read_kernel_str;
static struct task_struct *(*current_task)(void) = (void *)BPF_FUNC_get_current_task_btf;
static void *(*kptr_xchg)(void *, void *) = (void *)BPF_FUNC_kptr_xchg;
static void *(*task_storage)(void *, struct task_struct *, void *, u64) = (void *)BPF_FUNC_task_storage_get;
static long (*under_cgroup)(void *, u32) = (void *)BPF_FUNC_current_task_under_cgroup;
static long (*loop)(u32, void *, void *, u64) = (void *)BPF_FUNC_loop;
extern struct task_struct *bpf_task_acquire(struct task_struct *) __attribute__((section(".ksyms")));
extern void bpf_task_release(struct task_struct *) __attribute__((section(".ksyms")));

#define SECRET 1
#define READONLY 2
#define CUSTODY 4
#define ROOT 8
#define GRANTED 16
#define WRITE 1
#define MOVE 2
#define MAX_DEPTH 128
/* The Unicode compiler proves every stage expands at most two runes per input
 * byte. Components are at most 255 bytes; keep task storage below 8 KiB. */
#define MAX_RUNES 512
#define SEED_A 14695981039346656037ULL
#define SEED_B 7809847782465536322ULL
#define PRIME_A 1099511628211ULL
#define PRIME_B 14029467366897019727ULL

struct label { u32 flags, root; };
struct object_key { u64 ino; u32 dev, reserved; };
struct key { u64 a, b; u32 root, reserved; };
struct name_key { u64 a, b; };
struct rune_mapping { u32 decomposition[4], fold[4], ndecomposition, nfold, ccc; };
struct settings { u32 normalize, fold; };
struct owner { struct task_struct __attribute__((btf_type_tag("kptr"))) *task; };
struct scratch {
    struct name_key names[MAX_DEPTH];
    u32 input[MAX_RUNES], output[MAX_RUNES];
    u8 name[256];
};

struct { UINT(type, BPF_MAP_TYPE_CGROUP_ARRAY); UINT(max_entries, 1);
    TYPE(key, u32); TYPE(value, u32); } scope SEC(".maps");
struct { UINT(type, BPF_MAP_TYPE_HASH); UINT(max_entries, 100000);
    TYPE(key, struct object_key); TYPE(value, struct label); } objects SEC(".maps");
struct { UINT(type, BPF_MAP_TYPE_ARRAY); UINT(max_entries, 1);
    TYPE(key, u32); TYPE(value, struct owner); } owners SEC(".maps");
struct { UINT(type, BPF_MAP_TYPE_HASH); UINT(max_entries, 100000);
    TYPE(key, struct key); TYPE(value, u32); } paths SEC(".maps");
struct { UINT(type, BPF_MAP_TYPE_HASH); UINT(max_entries, 20000);
    TYPE(key, u32); TYPE(value, struct rune_mapping); } runes SEC(".maps");
struct { UINT(type, BPF_MAP_TYPE_HASH); UINT(max_entries, 128);
    TYPE(key, u32); TYPE(value, u32); } devices SEC(".maps");
struct { UINT(type, BPF_MAP_TYPE_HASH); UINT(max_entries, 32);
    TYPE(key, u32); TYPE(value, struct settings); } config SEC(".maps");
/* Cgroup LSM can be preempted: per-CPU scratch could mix two tasks' decisions.
 * Task-local storage follows the current thread and is freed on its exit. */
struct { UINT(type, BPF_MAP_TYPE_TASK_STORAGE); UINT(map_flags, BPF_F_NO_PREALLOC);
    TYPE(key, int); TYPE(value, struct scratch); } work SEC(".maps");
#ifdef FS_DIAGNOSTICS
struct diagnostic { u64 ino; u32 dev, reason, operation; char name[256]; };
struct { UINT(type, BPF_MAP_TYPE_ARRAY); UINT(max_entries, 1);
    TYPE(key, u32); TYPE(value, struct diagnostic); } fs_debug SEC(".maps");
#endif
INLINE int denied(struct dentry *d, int operation, int reason) {
#ifdef FS_DIAGNOSTICS
    u32 zero = 0;
    struct diagnostic *value = lookup(&fs_debug, &zero);
    if (value) {
        value->reason = reason; value->operation = operation;
        struct inode *inode = d->d_inode;
        value->ino = inode ? inode->i_ino : 0;
        value->dev = inode ? inode->i_sb->s_dev : 0;
        read_str(value->name, sizeof(value->name), d->d_name.name);
    }
#endif
    return 0;
}

INLINE int restricted(u32 flags, int operation, int leaf) {
    return (flags & SECRET) || (leaf && (((flags & READONLY) && operation) || ((flags & CUSTODY) && (operation & MOVE))));
}

/* Inode pins belong to this exact tagged task, not a reusable PID number.
 * PF_EXITING precedes exit_files. Call again after classification and before allow.
 */
NOINLINE int owner_alive(void) {
    u32 zero = 0;
    struct owner *owner = lookup(&owners, &zero);
    if (!owner) return 0;
    struct task_struct *task = owner->task;
    return task && !(task->flags & 0x4) && !task->exit_state;
}

SEC("syscall") int admit_owner(void *ctx) {
    u32 zero = 0;
    struct owner *owner = lookup(&owners, &zero);
    if (!owner || owner->task) return 1;
    struct task_struct *task = bpf_task_acquire(current_task());
    if (!task) return 1;
    struct task_struct *old = kptr_xchg(&owner->task, task);
    if (old) bpf_task_release(old);
    return 0;
}

INLINE u32 ccc(u32 rune) {
    struct rune_mapping *value = lookup(&runes, &rune);
    return value ? value->ccc : 0;
}

struct order_context { struct scratch *s; u32 at, klass; };
static long order_step(u32 unused, void *data) {
    struct order_context *c = data;
    u32 at = c->at;
    if (!at || at >= MAX_RUNES) return 1;
    u32 previous = c->s->output[at-1];
    u32 before = ccc(previous);
    if (!before || before <= c->klass) return 1;
    c->s->output[at] = previous;
    c->at = at-1;
    return 0;
}

/* Stable canonical ordering, bounded by a single filesystem component. */
NOINLINE int ordered_append(struct scratch *s, u32 count, u32 rune) {
    if (count >= MAX_RUNES) return -1;
    struct order_context c = {s, count, ccc(rune)};
    if (c.klass && loop(MAX_RUNES, order_step, &c, 0) < 0) return -1;
    if (c.at >= MAX_RUNES) return -1;
    s->output[c.at] = rune;
    return count + 1;
}

struct normalize_context { struct scratch *s; u32 length, count; int folding, error; };
static long normalize_step(u32 i, void *data) {
        struct normalize_context *c = data;
        if (i >= c->length || i >= MAX_RUNES) return 1;
        struct scratch *s = c->s;
        u32 count = c->count;
        int folding = c->folding;
        u32 rune = s->input[i];
        struct rune_mapping *value = lookup(&runes, &rune);
        u32 n = value ? (folding ? value->nfold : value->ndecomposition) : 0;
        if (n > 4) { c->error = 1; return 1; }
        if (!n) {
            int next = folding ? (count < MAX_RUNES ? (s->output[count] = rune, count + 1) : -1) : ordered_append(s, count, rune);
            if (next < 0) { c->error = 1; return 1; }
            count = next;
        } else {
            for (u32 j = 0; j < 4; j++) {
                if (j >= n) break;
                u32 r = folding ? value->fold[j] : value->decomposition[j];
                int next = folding ? (count < MAX_RUNES ? (s->output[count] = r, count + 1) : -1) : ordered_append(s, count, r);
                if (next < 0) { c->error = 1; return 1; }
                count = next;
            }
        }
        c->count = count;
        return 0;
}
static long normalize_copy(u32 i, void *data) {
    struct normalize_context *c = data;
    if (i >= c->count || i >= MAX_RUNES) return 1;
    c->s->input[i] = c->s->output[i];
    return 0;
}
NOINLINE int normalize(struct scratch *s, u32 length, int folding) {
    struct normalize_context c = {s, length, 0, folding, 0};
    if (loop(MAX_RUNES, normalize_step, &c, 0) < 0 || c.error) return -1;
    if (loop(MAX_RUNES, normalize_copy, &c, 0) < 0) return -1;
    return c.count;
}

struct decode_context { struct scratch *s; u32 length, at, count, error, nonascii; };
NOINLINE int byte_at(struct scratch *s, u32 at) {
    if (at >= 256) return -1;
    return s->name[at];
}
static long decode_step(u32 unused, void *data) {
        struct decode_context *c = data;
        struct scratch *s = c->s;
        u32 i = c->at, length = c->length, count = c->count;
        if (i >= length) return 1;
        if (i >= 256) { c->error = 1; return 1; }
        int first = byte_at(s, i++);
        if (first < 0) { c->error = 1; return 1; }
        u32 rune = first, continuation = 0, minimum = 0;
        if (rune >= 0x80) c->nonascii = 1;
        if (rune >= 0xc2 && rune <= 0xdf) { rune &= 31; continuation = 1; minimum = 0x80; }
        else if (rune >= 0xe0 && rune <= 0xef) { rune &= 15; continuation = 2; minimum = 0x800; }
        else if (rune >= 0xf0 && rune <= 0xf4) { rune &= 7; continuation = 3; minimum = 0x10000; }
        else if (rune >= 0x80) { c->error = 1; return 1; }
        for (u32 j = 0; j < 3; j++) {
            if (j >= continuation) break;
            if (i >= length || i >= 256) { c->error = 1; return 1; }
            int next = byte_at(s, i++);
            if (next < 0 || (next & 0xc0) != 0x80) { c->error = 1; return 1; }
            rune = rune * 64 + (next & 63);
        }
        if (rune < minimum || rune > 0x10ffff || (rune >= 0xd800 && rune <= 0xdfff) || count >= MAX_RUNES) { c->error = 1; return 1; }
        s->input[count++] = rune;
        c->at = i; c->count = count;
        return 0;
}
struct hash_context { struct scratch *s; u64 a, b; u32 length, fold; };
static long hash_step(u32 i, void *data) {
    struct hash_context *c = data;
    if (i >= c->length || i >= MAX_RUNES) return 1;
    u32 rune = c->s->input[i];
    if (c->fold && rune >= 'A' && rune <= 'Z') rune += 32;
    c->a = (c->a ^ rune) * PRIME_A;
    c->b = (c->b ^ rune) * PRIME_B;
    return 0;
}
__attribute__((noinline)) int name_key(struct scratch *s, u32 length, struct settings *settings, u32 depth) {
    if (!s || !settings || length > 255) return -1;
    struct decode_context c = {s, length, 0, 0, 0, 0};
    if (loop(256, decode_step, &c, 0) < 0 || c.error) return -1;
    int n = c.count;
    if (c.nonascii && settings->normalize) n = normalize(s, n, 0);
    if (n < 0) return -1;
    if (c.nonascii && settings->fold) n = normalize(s, n, 1);
    if (n < 0) return -1;
    if (c.nonascii && settings->normalize) n = normalize(s, n, 0);
    if (n < 0 || depth >= MAX_DEPTH) return -1;
    struct hash_context h = {s, SEED_A, SEED_B, n, !c.nonascii && settings->fold};
    if (loop(MAX_RUNES, hash_step, &h, 0) < 0) return -1;
    s->names[depth] = (struct name_key){h.a, h.b};
    return 0;
}

struct walk_context { struct dentry *d; struct scratch *s; struct settings *settings; u32 root, depth; int operation, result, identify; };
static long walk_step(u32 i, void *data) {
        struct walk_context *c = data;
        if (i >= MAX_DEPTH) { c->result = -1; return 1; }
        struct dentry *d = c->d;
        struct inode *inode = d->d_inode;
        struct object_key object = {};
        if (inode) { object.ino = inode->i_ino; object.dev = inode->i_sb->s_dev; }
        struct label *label = inode ? lookup(&objects, &object) : 0;
        if (label) {
            if (i == 0 && (label->flags & GRANTED)) { c->result = c->operation ? -1 : 2; return 1; }
            if (restricted(label->flags, c->operation, i == 0)) { c->result = -1; return 1; }
            if (label->flags & ROOT) { c->root = label->root; c->depth = i; return 1; }
        }
        if (!c->identify) {
            long n = read_str(c->s->name, sizeof(c->s->name), d->d_name.name);
            if (n <= 0 || n > 256 || name_key(c->s, n-1, c->settings, i)) { c->result = -1; return 1; }
        }
        struct dentry *parent = d->d_parent;
        if (!parent || parent == d) { c->result = 1; return 1; }
        c->d = parent;
        if (i == MAX_DEPTH-1) { c->result = -1; return 1; }
        return 0;
}
struct path_context { struct scratch *s; struct key key; u32 depth; int operation, denied; };
static long path_step(u32 n, void *data) {
    struct path_context *c = data;
    if (n >= c->depth) return 1;
    u32 i = c->depth - n - 1;
    if (i >= MAX_DEPTH) { c->denied = 1; return 1; }
    c->key.a = c->key.a * PRIME_A ^ c->s->names[i].a;
    c->key.b = c->key.b * PRIME_B ^ c->s->names[i].b;
    u32 *flags = lookup(&paths, &c->key);
    if (flags && restricted(*flags, c->operation, i == 0)) { c->denied = 1; return 1; }
    return 0;
}

NOINLINE int access(struct dentry *d, int operation) {
    if (!d) return 1;
    struct inode *inode = d->d_inode;
    struct dentry *parent = d->d_parent;
    if (!inode && parent) inode = parent->d_inode;
    if (!inode) return 1;
    u32 device = inode->i_sb->s_dev;
    if (!lookup(&devices, &device)) return 1;
    if (!owner_alive()) return denied(d, operation, 1);
    struct walk_context identified = {d, 0, 0, 0, 0, operation, 0, 1};
    if (loop(MAX_DEPTH, walk_step, &identified, 0) < 0 || identified.result < 0) return denied(d, operation, 3);
    if (identified.result == 1 || identified.result == 2) return owner_alive();
    if (!identified.root) return denied(d, operation, 4);
    if (!identified.depth) return owner_alive();
    struct settings *settings = lookup(&config, &identified.root);
    struct scratch *s = task_storage(&work, current_task(), 0, BPF_LOCAL_STORAGE_GET_F_CREATE);
    if (!settings) return denied(d, operation, 20);
    if (!s) return denied(d, operation, 21);
    struct walk_context c = {d, s, settings, 0, 0, operation, 0, 0};
    if (loop(MAX_DEPTH, walk_step, &c, 0) < 0 || c.result < 0) return denied(d, operation, 3);
    if (c.root != identified.root || c.depth != identified.depth || c.result) return denied(d, operation, 4);
    struct path_context p = {s, {SEED_A, SEED_B, c.root, 0}, c.depth, operation, 0};
    if (loop(MAX_DEPTH, path_step, &p, 0) < 0 || p.denied) return denied(d, operation, 5);
    return owner_alive();
}

INLINE int file_access(struct file *file, int operation) {
    return !file || access(file->f_path.dentry, operation);
}
/* Preserve prior LSM decisions; the cgroup map retains the exact parent object. */
#define HOOK(name, argc) \
    static int check_##name(u64 *ctx); \
    SEC("lsm/" #name) int guard_##name(u64 *ctx) { \
        int prior = ctx[argc]; \
        if (prior) return prior; \
        long governed = under_cgroup(&scope, 0); \
        if (governed <= 0) return 0; \
        return check_##name(ctx) ? 0 : -1; \
    } \
    static int check_##name(u64 *ctx)
HOOK(file_open, 1) { struct file *f = (void *)ctx[0]; return file_access(f, (f->f_flags & (3|512)) ? WRITE : 0); }
HOOK(file_permission, 2) { return file_access((void *)ctx[0], (ctx[1] & 2) ? WRITE : 0); }
HOOK(file_truncate, 1) { return file_access((void *)ctx[0], WRITE); }
HOOK(file_receive, 1) { struct file *f = (void *)ctx[0]; return file_access(f, (f->f_flags & 3) ? WRITE : 0); }
/* Private writable mappings are copy-on-write, not mutations of the file. */
HOOK(mmap_file, 4) { return file_access((void *)ctx[0], (ctx[2] & 2) && (ctx[3] & 1) ? WRITE : 0); }
HOOK(file_mprotect, 3) { struct vm_area_struct *v = (void *)ctx[0]; return file_access(v->vm_file, (ctx[2] & 2) && (v->vm_flags & 8) ? WRITE : 0); }
HOOK(inode_setattr, 3) { return access((void *)ctx[1], WRITE); }
HOOK(inode_unlink, 2) { return access((void *)ctx[1], MOVE); }
HOOK(inode_rmdir, 2) { return access((void *)ctx[1], MOVE); }
HOOK(inode_link, 3) { return access((void *)ctx[0], MOVE) && access((void *)ctx[2], MOVE); }
HOOK(inode_rename, 4) { return access((void *)ctx[1], MOVE) && access((void *)ctx[3], MOVE); }
HOOK(inode_create, 3) { return access((void *)ctx[1], WRITE); }
HOOK(inode_mkdir, 3) { return access((void *)ctx[1], WRITE); }
HOOK(inode_symlink, 3) { return access((void *)ctx[1], WRITE); }
HOOK(inode_mknod, 4) { return access((void *)ctx[1], WRITE); }
HOOK(inode_follow_link, 3) { return access((void *)ctx[0], 0); }
HOOK(inode_readlink, 1) { return access((void *)ctx[0], 0); }
HOOK(inode_getattr, 1) { struct path *p = (void *)ctx[0]; return access(p->dentry, 0); }
HOOK(inode_getxattr, 2) { return access((void *)ctx[0], 0); }
HOOK(inode_listxattr, 1) { return access((void *)ctx[0], 0); }
HOOK(inode_setxattr, 6) { return access((void *)ctx[1], WRITE); }
HOOK(inode_removexattr, 3) { return access((void *)ctx[1], WRITE); }
HOOK(inode_get_acl, 3) { return access((void *)ctx[1], 0); }
HOOK(inode_set_acl, 4) { return access((void *)ctx[1], WRITE); }
HOOK(inode_remove_acl, 3) { return access((void *)ctx[1], WRITE); }
HOOK(inode_file_setattr, 2) { return access((void *)ctx[0], WRITE); }
HOOK(inode_file_getattr, 2) { return access((void *)ctx[0], 0); }
char LICENSE[] SEC("license") = "GPL";
