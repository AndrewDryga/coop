/* SPDX-License-Identifier: MIT
 * Single-threaded owner of admission-time inode pins and cgroup attachments.
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
    int paths = map_fd("paths"), runes = map_fd("runes"), config = map_fd("config");
    int roots[32]; dev_t root_devices[32];
    objects = map_fd("objects"); devices = map_fd("devices");
    if (paths < 0 || runes < 0 || objects < 0 || devices < 0 || config < 0) return -1;
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
        if (bpf_map_update_elem(runes, &key, &value, BPF_NOEXIST)) return -1;
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
    int frozen[] = {paths, runes, devices, objects, config};
    for (unsigned i = 0; i < sizeof(frozen)/sizeof(frozen[0]); i++) if (bpf_map_freeze(frozen[i])) return -1;
    return 0;
}
static int cleanup(void) {
    int failed = 0;
    for (int i = nattached-1; i >= 0; i--) {
        if (link_paths[i][0] && unlink(link_paths[i]) && errno != ENOENT) { perror("unpin filesystem hook"); failed = 1; }
        if (bpf_link__destroy(attached[i])) { perror("detach filesystem hook"); failed = 1; }
        attached[i] = NULL;
    }
    if (failed) return -1;
    nattached = 0;
    if (pin_directory[0] && rmdir(pin_directory)) { perror("remove filesystem pins"); return -1; }
    pin_directory[0] = 0;
    for (int i = nnormalizer_links-1; i >= 0; i--) {
        if (bpf_link__destroy(normalizer_links[i])) { perror("detach filesystem normalizer"); return -1; }
        normalizer_links[i] = NULL;
    }
    nnormalizer_links = 0;
    if (normalizer) { bpf_object__close(normalizer); normalizer = NULL; }
    if (object) { bpf_object__close(object); object = NULL; }
    for (int i = 0; i < npins; i++) close(pins[i]);
    npins = 0;
    if (group >= 0) { close(group); group = -1; }
    free(pins); pins = NULL;
    return 0;
}
static int children(void) {
    int fd = openat(group, ".", O_RDONLY | O_DIRECTORY | O_CLOEXEC);
    if (fd < 0) return -1;
    DIR *dir = fdopendir(fd);
    if (!dir) { close(fd); return -1; }
    int result = 0;
    for (;;) {
        errno = 0;
        struct dirent *entry = readdir(dir);
        if (!entry) { if (errno) result = -1; break; }
        if (!strcmp(entry->d_name, ".") || !strcmp(entry->d_name, "..")) continue;
        struct stat st;
        if (fstatat(fd, entry->d_name, &st, AT_SYMLINK_NOFOLLOW)) { result = -1; break; }
        if (S_ISDIR(st.st_mode)) { result = 1; break; }
    }
    closedir(dir); return result;
}
static int write_birth(const char *parent, const char *boot) {
    struct stat cg, directory;
    if (fstat(group, &cg) || stat(pin_directory, &directory)) return -1;
    int fd = open("/state/birth.json.new", O_WRONLY|O_CREAT|O_EXCL|O_CLOEXEC|O_NOFOLLOW, 0600);
    if (fd < 0) return -1;
    FILE *out = fdopen(fd, "w");
    if (!out) { close(fd); return -1; }
    int result = -1, count = 0;
    fprintf(out, "{\"parent\":\"/%s\",\"boot_id\":\"%s\",\"cgroup_inode\":%llu,\"cgroup_device\":%llu,\"pin_directory_inode\":%llu,\"pin_directory_device\":%llu,\"programs\":[", strrchr(parent, '/')+1, boot, (unsigned long long)cg.st_ino, (unsigned long long)cg.st_dev, (unsigned long long)directory.st_ino, (unsigned long long)directory.st_dev);
    struct bpf_program *program;
    bpf_object__for_each_program(program, object) {
        const char *name = bpf_program__name(program);
        if (strncmp(name, "guard_", 6)) continue;
        struct bpf_prog_info info = {0}; uint32_t size = sizeof(info);
        if (bpf_obj_get_info_by_fd(bpf_program__fd(program), &info, &size)) goto done;
        fprintf(out, "%s{\"name\":\"%s\",\"id\":0,\"program\":%u}", count++ ? "," : "", name, info.id);
    }
    fputs("]}\n", out);
    if (fflush(out) || fsync(fd)) goto done;
    if (rename("/state/birth.json.new", "/state/birth.json")) goto done;
    int dir = open("/state", O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
    if (dir < 0) goto done;
    result = fsync(dir); close(dir);
done:
    if (fclose(out)) result = -1;
    return result;
}

static int kernel_filesystems(void) {
    struct statfs cg, bp;
    if (statfs("/host-cgroup", &cg) || statfs("/host-bpf", &bp)) return -1;
    if (cg.f_type != CGROUP2_SUPER_MAGIC || bp.f_type != BPF_FS_MAGIC) { errno = ENOTSUP; return -1; }
    return 0;
}

static int recover(int argc, char **argv) {
    if (argc < 7 || argc > 39 || kernel_filesystems()) { errno = EINVAL; return -1; }
    const char *name = strrchr(argv[2], '/');
    if (!name || snprintf(pin_directory, sizeof(pin_directory), "/host-bpf/%s", name+1) >= (int)sizeof(pin_directory)) { errno = EINVAL; return -1; }
    group = open(argv[2], O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
    if (group < 0) {
        struct stat missing;
        if (errno == ENOENT && lstat(pin_directory, &missing) && errno == ENOENT) return 0;
        return -1;
    }
    char *end;
    unsigned long long identity = strtoull(argv[3], &end, 10);
    if (*end) { errno = EINVAL; return -1; }
    unsigned long long device = strtoull(argv[4], &end, 10);
    struct stat st;
    if (*end || fstat(group, &st) || (identity && (st.st_ino != identity || st.st_dev != device)) || children() != 0) { errno = EBUSY; return -1; }
    unsigned long long pin_identity = strtoull(argv[5], &end, 10);
    if (*end) { errno = EINVAL; return -1; }
    unsigned long long pin_device = strtoull(argv[6], &end, 10);
    if (*end) { errno = EINVAL; return -1; }
    if (lstat(pin_directory, &st)) {
        if (errno == ENOENT) { int result = rmdir(argv[2]); close(group); group = -1; return result; }
        return -1;
    }
    if (!S_ISDIR(st.st_mode) || (pin_identity && (st.st_ino != pin_identity || st.st_dev != pin_device))) { errno = ESTALE; return -1; }
    DIR *directory = opendir(pin_directory);
    if (!directory) return -1;
    int valid = 1;
    for (;;) {
        errno = 0;
        struct dirent *entry = readdir(directory);
        if (!entry) { if (errno) valid = 0; break; }
        if (!strcmp(entry->d_name, ".") || !strcmp(entry->d_name, "..")) continue;
        int known = 0;
        for (int i = 7; i < argc; i++) {
            size_t length = strlen(entry->d_name);
            if (!strncmp(argv[i], entry->d_name, length) && argv[i][length] == ':') known = 1;
        }
        if (!known) { valid = 0; break; }
    }
    closedir(directory);
    if (!valid) { errno = ESTALE; return -1; }
    int fds[32], count = 0, result = -1;
    char paths[32][PATH_MAX];
    for (int i = 7; i < argc; i++) {
        char hook[128], extra; unsigned id, program;
        if (sscanf(argv[i], "%127[^:]:%u:%u%c", hook, &id, &program, &extra) != 3 || !identity || !pin_identity || !program || strncmp(hook, "guard_", 6)) { errno = EINVAL; goto done; }
        int known = 0;
        for (unsigned j = 0; j < sizeof(hooks)/sizeof(hooks[0]); j++) if (!strcmp(hook+6, hooks[j].name)) known = 1;
        if (!known || snprintf(paths[count], PATH_MAX, "%s/%s", pin_directory, hook) >= PATH_MAX) { errno = EINVAL; goto done; }
        int fd = bpf_obj_get(paths[count]);
        if (fd < 0 && errno == ENOENT) continue;
        if (fd < 0) goto done;
        struct bpf_link_info info = {0}; uint32_t size = sizeof(info);
        if (bpf_obj_get_info_by_fd(fd, &info, &size) || info.type != BPF_LINK_TYPE_TRACING || (id && info.id != id) || info.prog_id != program) { close(fd); errno = ESTALE; goto done; }
        fds[count++] = fd;
    }
    /* Acquire and verify every surviving reference before the first unlink. The
     * host has already proved all referencing Docker objects are gone. */
    for (int i = 0; i < count; i++) if (unlink(paths[i])) goto done;
    if (rmdir(pin_directory) && errno != ENOENT) goto done;
    if (rmdir(argv[2])) goto done;
    result = 0;
done:
    for (int i = 0; i < count; i++) close(fds[i]);
    close(group); group = -1;
    return result;
}
int main(int argc, char **argv) {
    if (argc > 1 && !strcmp(argv[1], "release")) {
        if (recover(argc, argv)) { perror("recover filesystem custody"); return 1; }
        return 0;
    }
    const char *stage = "read policy";
    if (argc != 4) { fprintf(stderr, "expected root, owned parent, boot identity\n"); return 1; }
    sigset_t releases; sigemptyset(&releases); sigaddset(&releases, SIGUSR1);
    if (sigprocmask(SIG_BLOCK, &releases, NULL)) { perror("block release signal"); return 1; }
    FILE *in = stdin;
    if (kernel_filesystems()) goto fail;
    struct bpf_object_open_opts options = {.sz = sizeof(options), .kernel_log_buf = verifier_log, .kernel_log_size = sizeof(verifier_log)};
    object = bpf_object__open_file("/usr/local/libexec/coop-fs/filter.o", &options);
    if (!object) goto fail;
    stage = "load kernel programs";
    if (validate_hooks()) goto fail;
    if (bpf_object__load(object)) goto fail;
    stage = "load shared filename normalization";
    if (load_normalizer(&options)) goto fail;
    stage = "read policy";
    char magic[8]; uint32_t header[5];
    if (read_exact(in, magic, sizeof(magic)) || memcmp(magic, "COOPFS03", 8) || read_exact(in, header, sizeof(header)) || !header[0] || header[0] > 32 || header[1] > LIMIT || !header[2] || header[2] > 20000 || header[3] > LIMIT-header[0] || header[4] > LIMIT-header[0]-header[3]) { errno = EINVAL; goto fail; }
    uint32_t object_capacity = header[0]+header[3]+header[4];
    pins = calloc(object_capacity, sizeof(*pins));
    if (!pins) goto fail;
    stage = "seed protected objects";
    if (seed_policy(in, header, argv[1])) goto fail;
    fclose(in); in = NULL;
    struct bpf_program *owner = bpf_object__find_program_by_name(object, "admit_owner");
    stage = "establish descriptor owner";
    struct bpf_test_run_opts run = {.sz = sizeof(run)};
    if (!owner || bpf_prog_test_run_opts(bpf_program__fd(owner), &run) || run.retval) { errno = EINVAL; goto fail; }
    group = open(argv[2], O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
    stage = "open owned cgroup";
    if (group < 0 || children() != 0) goto fail;
    uint32_t zero = 0;
    int scope = map_fd("scope");
    if (scope < 0 || bpf_map_update_elem(scope, &zero, &group, BPF_ANY) || bpf_map_freeze(scope)) goto fail;
    char directory[PATH_MAX];
    if (snprintf(directory, sizeof(directory), "/host-bpf/%s", strrchr(argv[2], '/')+1) >= (int)sizeof(directory)) { errno = ENAMETOOLONG; goto fail; }
    stage = "create persistent attachment directory";
    if (mkdir(directory, 0700)) goto fail;
    strcpy(pin_directory, directory);
    stage = "persist attachment birth";
    if (write_birth(argv[2], argv[3])) goto fail;
    struct bpf_program *program;
    bpf_object__for_each_program(program, object) {
        if (program == owner) continue;
        if (nattached >= 32 || strncmp(bpf_program__name(program), "guard_", 6)) { errno = EINVAL; goto fail; }
        stage = bpf_program__name(program);
        struct bpf_link *link = bpf_program__attach_lsm(program);
        if (!link) goto fail;
        attached[nattached++] = link;
        char *path = link_paths[nattached-1];
        if (snprintf(path, PATH_MAX, "%s/%s", pin_directory, stage) >= PATH_MAX || bpf_link__pin(link, path)) goto fail;
    }
    struct stat group_stat;
    if (fstat(group, &group_stat)) goto fail;
    printf("{\"status\":\"ready\",\"parent\":\"/%s\",\"boot_id\":\"%s\",\"cgroup_inode\":%llu,\"programs\":%d,\"pins\":%d,\"links\":[", strrchr(argv[2], '/')+1, argv[3], (unsigned long long)group_stat.st_ino, nattached, npins);
    for (int i = 0; i < nattached; i++) {
        struct bpf_link_info info = {0}; uint32_t size = sizeof(info);
        if (bpf_obj_get_info_by_fd(bpf_link__fd(attached[i]), &info, &size)) goto fail;
        printf("%s{\"name\":\"%s\",\"id\":%u,\"program\":%u}", i ? "," : "", strrchr(link_paths[i], '/')+1, info.id, info.prog_id);
    }
    puts("]}");
    if (fflush(stdout)) goto fail;
    for (;;) {
        int signal;
        if (sigwait(&releases, &signal)) { fprintf(stderr, "filesystem guard lost release channel; protection retained\n"); return 1; }
        int busy = children();
        if (busy != 0) { fprintf(stderr, "filesystem guard retains custody: child observation %d\n", busy); continue; }
        if (cleanup()) return 1;
        if (rmdir(argv[2])) { perror("remove owned cgroup"); return 1; }
        return 0;
    }
fail:
    fprintf(stderr, "filesystem admission (%s): %s\n", stage, strerror(errno));
    if (in) fclose(in);
    if (cleanup()) return 1;
    if (rmdir(argv[2])) perror("remove failed admission cgroup");
    return 1;
}
