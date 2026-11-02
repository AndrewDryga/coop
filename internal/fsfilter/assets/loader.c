/* SPDX-License-Identifier: MIT
 * Shared controller and independent owners of fresh admission-time inode pins.
 * libbpf supplies ELF/BTF/CO-RE and verifier handling; no host tool installation.
 */
#define _GNU_SOURCE
#include <bpf/libbpf.h>
#include <bpf/bpf.h>
#include <bpf/btf.h>
#include <linux/openat2.h>
#include <sys/syscall.h>
#include <sys/stat.h>
#include <sys/vfs.h>
#include <linux/magic.h>
#include <sys/sysmacros.h>
#include <unistd.h>
#include <fcntl.h>
#include <dirent.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#include <limits.h>
#include <time.h>

#define LIMIT 100000
#define SECRET 1
#define READONLY 2
#define CUSTODY 4
#define ROOT 8
#define GRANTED 16
struct path_key { uint64_t a, b; uint32_t root, reserved; };
struct object_key { uint64_t ino; uint32_t dev, reserved; };
struct label { uint32_t flags, root; };
struct rune_mapping { uint32_t decomposition[4], fold[4], ndecomposition, nfold, ccc; };
struct settings { uint32_t normalize, fold; };
static struct bpf_object *object, *normalizer;
static struct bpf_link *normalizer_links[32];
static int nnormalizer_links;
static int objects, devices, *pins, npins, nattached, group = -1;
static struct bpf_link *attached[32];
static char link_paths[32][PATH_MAX], pin_directory[PATH_MAX];
static int policy_maps[4] = {-1, -1, -1, -1};
static const char *policy_names[] = {"objects", "paths", "devices", "config"};
static dev_t root_device;
static char verifier_log[1024*1024];

struct hook_shape { const char *name; int arguments, index; const char *pointee; };
static const struct hook_shape hooks[] = {
    {"file_open", 1, 0, "file"}, {"file_permission", 2, 0, "file"},
    {"file_truncate", 1, 0, "file"}, {"file_receive", 1, 0, "file"},
    {"mmap_file", 4, 0, "file"}, {"file_mprotect", 3, 0, "vm_area_struct"},
    {"inode_setattr", 3, 1, "dentry"}, {"inode_unlink", 2, 1, "dentry"},
    {"inode_rmdir", 2, 1, "dentry"}, {"inode_link", 3, 0, "dentry"},
    {"inode_rename", 4, 1, "dentry"}, {"inode_create", 3, 1, "dentry"},
    {"inode_mkdir", 3, 1, "dentry"}, {"inode_symlink", 3, 1, "dentry"},
    {"inode_mknod", 4, 1, "dentry"}, {"inode_follow_link", 3, 0, "dentry"},
    {"inode_readlink", 1, 0, "dentry"}, {"inode_getattr", 1, 0, "path"},
    {"inode_getxattr", 2, 0, "dentry"}, {"inode_listxattr", 1, 0, "dentry"},
    {"inode_setxattr", 6, 1, "dentry"}, {"inode_removexattr", 3, 1, "dentry"},
    {"inode_get_acl", 3, 1, "dentry"}, {"inode_set_acl", 4, 1, "dentry"},
    {"inode_remove_acl", 3, 1, "dentry"}, {"inode_file_setattr", 2, 0, "dentry"},
    {"inode_file_getattr", 2, 0, "dentry"},
};
static const struct btf_type *base_type(struct btf *btf, uint32_t id) {
    const struct btf_type *type;
    for (int i = 0; i < 32; i++) {
        type = btf__type_by_id(btf, id);
        if (!type) return NULL;
        if (!btf_is_mod(type) && !btf_is_typedef(type) && !btf_is_type_tag(type)) return type;
        id = type->type;
    }
    return NULL;
}
static int pointer_to(struct btf *btf, uint32_t id, const char *name) {
    const struct btf_type *type = base_type(btf, id);
    if (!type || !btf_is_ptr(type)) return 0;
    type = base_type(btf, type->type);
    return type && btf_is_struct(type) && !strcmp(btf__name_by_offset(btf, type->name_off), name);
}
/* CO-RE relocates fields, not ctx argument positions. Unsupported signatures
 * must fail before attaching anything, even if the verifier accepts the cast. */
static int validate_hooks(void) {
    struct btf *btf = btf__load_vmlinux_btf();
    if (!btf) return -1;
    int result = -1;
    for (unsigned i = 0; i < sizeof(hooks)/sizeof(hooks[0]); i++) {
        const struct hook_shape *hook = &hooks[i];
        char name[128]; snprintf(name, sizeof(name), "bpf_lsm_%s", hook->name);
        int id = btf__find_by_name_kind(btf, name, BTF_KIND_FUNC);
        const struct btf_type *function = id > 0 ? btf__type_by_id(btf, id) : NULL;
        const struct btf_type *proto = function ? btf__type_by_id(btf, function->type) : NULL;
        if (!proto || !btf_is_func_proto(proto) || btf_vlen(proto) != hook->arguments || !pointer_to(btf, btf_params(proto)[hook->index].type, hook->pointee)) {
            fprintf(stderr, "unsupported filesystem hook signature: %s\n", hook->name); goto done;
        }
        if (!strcmp(hook->name, "inode_link") && !pointer_to(btf, btf_params(proto)[2].type, "dentry")) goto done;
        if (!strcmp(hook->name, "inode_rename") && !pointer_to(btf, btf_params(proto)[3].type, "dentry")) goto done;
    }
    result = 0;
done:
    btf__free(btf);
    if (result) errno = ENOTSUP;
    return result;
}

static int read_exact(FILE *in, void *value, size_t size) {
    if (fread(value, 1, size, in) == size) return 0;
    errno = EINVAL; return -1;
}
static int map_fd(const char *name) {
    for (int i = 0; i < 4; i++) if (policy_maps[i] >= 0 && !strcmp(name, policy_names[i])) return policy_maps[i];
    struct bpf_map *map = bpf_object__find_map_by_name(object, name);
    if (!map) { errno = EINVAL; return -1; }
    return bpf_map__fd(map);
}
static int load_normalizer(const struct bpf_object_open_opts *options) {
    struct bpf_program *first = bpf_object__find_program_by_name(object, "guard_file_open");
    if (!first) { errno = EINVAL; return -1; }
    normalizer = bpf_object__open_file("/usr/local/libexec/coop-fs/normalize.o", options);
    if (!normalizer) return -1;
    struct bpf_map *map;
    bpf_object__for_each_map(map, normalizer) {
        int fd = map_fd(bpf_map__name(map));
        if (fd < 0 || bpf_map__reuse_fd(map, fd)) return -1;
    }
    struct bpf_program *replacement = bpf_object__find_program_by_name(normalizer, "normalize_name");
    if (!replacement) { errno = EINVAL; return -1; }
    if (bpf_program__set_attach_target(replacement, bpf_program__fd(first), "name_key") || bpf_object__load(normalizer)) return -1;
    struct bpf_program *program;
    bpf_object__for_each_program(program, object) {
        if (strncmp(bpf_program__name(program), "guard_", 6)) continue;
        if (nnormalizer_links >= 32) { errno = E2BIG; return -1; }
        struct bpf_link *link = bpf_program__attach_freplace(replacement, bpf_program__fd(program), "name_key");
        if (!link) return -1;
        /* Unpinned and exclusively owned here: closing restores the denying
         * stub. Durable custody still belongs to the ordinary base LSM links. */
        normalizer_links[nnormalizer_links++] = link;
    }
    return 0;
}
static int seed(int fd, uint32_t flags, uint32_t root) {
    struct stat st;
    if (fstat(fd, &st)) goto fail;
    if (st.st_dev != root_device) { errno = EXDEV; goto fail; }
    if ((flags & GRANTED) && (!S_ISREG(st.st_mode) || flags != (GRANTED|READONLY))) { errno = EINVAL; goto fail; }
    struct object_key key = {st.st_ino, (major(st.st_dev) << 20) | minor(st.st_dev), 0};
    struct label label = {0};
    int present = bpf_map_lookup_elem(objects, &key, &label) == 0;
    if (!present && errno != ENOENT) goto fail;
    if (root && label.root && label.root != root) { errno = EINVAL; goto fail; }
    label.flags |= flags;
    if (flags & GRANTED) label.flags &= ~SECRET;
    if (root) label.root = root;
    if (bpf_map_update_elem(objects, &key, &label, BPF_ANY)) goto fail;
    uint32_t one = 1;
    if (bpf_map_update_elem(devices, &key.dev, &one, BPF_ANY)) goto fail;
    if (present) { close(fd); return 0; }
    if (npins >= LIMIT) { errno = E2BIG; goto fail; }
    pins[npins++] = fd;
    return 0;
fail:
    close(fd); return -1;
}
static int beneath(int root, const char *path, int flags) {
    struct open_how how = {.flags = flags | O_CLOEXEC | O_NOFOLLOW,
        .resolve = RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS};
    return syscall(SYS_openat2, root, path, &how, sizeof(how));
}
static int pin_path(int root, const char *path, uint32_t flags) {
    int fd = beneath(root, path, O_PATH);
    return fd < 0 ? -1 : seed(fd, flags, 0);
}
static int seed_policy(FILE *in, const uint32_t *header, const char *root_path) {
    int paths = map_fd("paths"), config = map_fd("config");
    int roots[32]; dev_t root_devices[32];
    objects = map_fd("objects"); devices = map_fd("devices");
    if (paths < 0 || objects < 0 || devices < 0 || config < 0) return -1;
    for (uint32_t i = 0; i < header[0]; i++) {
        uint32_t profile[2];
        if (read_exact(in, profile, sizeof(profile)) || profile[0] != i+1 || profile[1] & ~3) { errno = EINVAL; return -1; }
        struct settings settings = {!!(profile[1] & 1), !!(profile[1] & 2)};
        if (bpf_map_update_elem(config, &profile[0], &settings, BPF_NOEXIST)) return -1;
    }
    for (uint32_t i = 0; i < header[1]; i++) {
        struct path_key key; uint32_t flags;
        if (read_exact(in, &key, sizeof(key)) || read_exact(in, &flags, sizeof(flags))) return -1;
        if (!flags || flags & ~(SECRET|READONLY|CUSTODY) || !key.root || key.root > header[0] || key.reserved) { errno = EINVAL; return -1; }
        if (bpf_map_update_elem(paths, &key, &flags, BPF_NOEXIST)) return -1;
    }
    for (uint32_t i = 0; i < header[2]; i++) {
        uint32_t key; struct rune_mapping value;
        if (read_exact(in, &key, sizeof(key)) || read_exact(in, &value, sizeof(value))) return -1;
        if (key > 0x10ffff || value.ndecomposition > 4 || value.nfold > 4 || value.ccc > 255) { errno = EINVAL; return -1; }
    }
    for (uint32_t i = 0; i < header[0]; i++) {
        char source[PATH_MAX];
        if (snprintf(source, sizeof(source), "%s/%u", root_path, i+1) >= (int)sizeof(source)) { errno = ENAMETOOLONG; return -1; }
        int root = open(source, O_PATH | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
        if (root < 0) return -1;
        struct stat st;
        if (fstat(root, &st)) { close(root); return -1; }
        root_devices[i] = root_device = st.st_dev;
        roots[i] = root;
        if (seed(root, ROOT|CUSTODY, i+1)) return -1;
    }
    for (uint32_t i = 0; i < header[3]+header[4]; i++) {
        uint32_t entry[3]; char path[PATH_MAX];
        if (read_exact(in, entry, sizeof(entry))) return -1;
        if (!entry[0] || entry[0] > header[0] || !entry[1] || entry[1] >= sizeof(path) || !entry[2]) { errno = EINVAL; return -1; }
        uint32_t flags = entry[2];
        if ((i < header[3] && flags & ~(SECRET|READONLY|CUSTODY)) || (i >= header[3] && flags != (GRANTED|READONLY))) { errno = EINVAL; return -1; }
        if (read_exact(in, path, entry[1])) return -1;
        if (memchr(path, 0, entry[1])) { errno = EINVAL; return -1; }
        path[entry[1]] = 0;
        if (path[0] == '/' || !strcmp(path, ".") || !strcmp(path, "..") || !strncmp(path, "../", 3) || strstr(path, "/../")) { errno = EINVAL; return -1; }
        int root = roots[entry[0]-1];
        root_device = root_devices[entry[0]-1];
        if (pin_path(root, path, flags)) return -1;
    }
    if (fgetc(in) != EOF || ferror(in)) { errno = EINVAL; return -1; }
    int frozen[] = {paths, devices, objects, config};
    for (unsigned i = 0; i < sizeof(frozen)/sizeof(frozen[0]); i++) if (bpf_map_freeze(frozen[i])) return -1;
    return 0;
}
#include "shared.h"
