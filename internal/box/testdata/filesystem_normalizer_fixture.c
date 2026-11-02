/* Test-image-only observations and faults, injected into loader.c. */
static void test_record(FILE *out, const char *kind, const char *name, int fd) {
    uint32_t id = 0, program = 0, target = 0, function = 0;
    if (!strcmp(kind, "map")) {
        struct bpf_map_info info = {0}; uint32_t size = sizeof(info);
        if (bpf_obj_get_info_by_fd(fd, &info, &size)) _exit(80);
        id = info.id;
    } else if (!strcmp(kind, "program")) {
        struct bpf_prog_info info = {0}; uint32_t size = sizeof(info);
        if (bpf_obj_get_info_by_fd(fd, &info, &size)) _exit(81);
        id = info.id;
    } else {
        struct bpf_link_info info = {0}; uint32_t size = sizeof(info);
        if (bpf_obj_get_info_by_fd(fd, &info, &size)) _exit(82);
        id = info.id; program = info.prog_id;
        if (!strcmp(name, "replacement")) {
            target = info.tracing.target_obj_id;
            function = info.tracing.target_btf_id;
        }
    }
    fprintf(out, "{\"kind\":\"%s\",\"name\":\"%s\",\"id\":%u,\"program\":%u,\"target\":%u,\"function\":%u}\n",
            kind, name, id, program, target, function);
}
static void test_snapshot(void) {
    FILE *out = fopen("/state/normalizer-ids.tmp", "wx");
    if (!out) _exit(83);
    struct bpf_object *all[] = {object, normalizer};
    for (int i = 0; i < 2; i++) {
        struct bpf_program *program;
        bpf_object__for_each_program(program, all[i])
            test_record(out, "program", bpf_program__name(program), bpf_program__fd(program));
        struct bpf_map *map;
        bpf_object__for_each_map(map, all[i]) {
            char name[128]; snprintf(name, sizeof(name), "%s/%s", i ? "extension" : "base", bpf_map__name(map));
            test_record(out, "map", name, bpf_map__fd(map));
        }
    }
    for (int i = 0; i < nnormalizer_links; i++)
        test_record(out, "link", "replacement", bpf_link__fd(normalizer_links[i]));
    for (int i = 0; i < nattached; i++)
        test_record(out, "link", "guard", bpf_link__fd(attached[i]));
    int function = btf__find_by_name_kind(bpf_object__btf(object), "name_key", BTF_KIND_FUNC);
    fprintf(out, "{\"kind\":\"function\",\"id\":%d}\n", function);
    if (function <= 0 || fclose(out) || rename("/state/normalizer-ids.tmp", "/state/normalizer-ids")) _exit(84);
}
static int test_fault(const char *name) {
    const char *fault = getenv("COOP_TEST_NORMALIZER");
    return fault && !strcmp(fault, name);
}
static void test_signal(void) {
    if (test_fault("owner-death")) _exit(47);
    struct bpf_program *program = bpf_object__find_program_by_name(object, "guard_file_open");
    struct bpf_prog_info info = {0}; uint32_t size = sizeof(info);
    if (!program || bpf_obj_get_info_by_fd(bpf_program__fd(program), &info, &size)) _exit(85);
    for (int i = 0; i < nnormalizer_links; i++) {
        struct bpf_link_info link = {0}; size = sizeof(link);
        if (bpf_obj_get_info_by_fd(bpf_link__fd(normalizer_links[i]), &link, &size)) _exit(86);
        if (link.tracing.target_obj_id != info.id) continue;
        if (bpf_link__destroy(normalizer_links[i])) _exit(87);
        normalizer_links[i] = NULL;
        FILE *out = fopen("/state/replacement-detached", "wx");
        if (!out || fclose(out)) _exit(88);
        return;
    }
    _exit(89);
}
static int test_control(int argc, char **argv) {
    if (!strcmp(argv[1], "test-signal")) {
        DIR *proc = opendir("/proc");
        if (!proc) return 1;
        struct dirent *entry; int owner = 0;
        while ((entry = readdir(proc))) {
            int pid = atoi(entry->d_name);
            if (pid <= 0 || pid == getpid()) continue;
            char path[128], name[64]; snprintf(path, sizeof(path), "/proc/%d/comm", pid);
            FILE *in = fopen(path, "r");
            if (!in) continue;
            int match = fgets(name, sizeof(name), in) && !strcmp(name, "coop-fs-kernel\n");
            fclose(in);
            if (match) { if (owner) { closedir(proc); return 1; } owner = pid; }
        }
        closedir(proc);
        return !owner || kill(owner, SIGUSR2);
    }
    /* Each lookup FD is closed immediately so observation cannot retain an object. */
    for (int attempt = 0; attempt < 200; attempt++) {
        int alive = 0;
        for (int i = 2; i < argc; i++) {
            char kind[16], extra; unsigned id;
            if (sscanf(argv[i], "%15[^:]:%u%c", kind, &id, &extra) != 2 || !id) return 1;
            int fd;
            if (!strcmp(kind, "program")) fd = bpf_prog_get_fd_by_id(id);
            else if (!strcmp(kind, "map")) fd = bpf_map_get_fd_by_id(id);
            else if (!strcmp(kind, "link")) fd = bpf_link_get_fd_by_id(id);
            else return 1;
            if (fd >= 0) { close(fd); alive++; }
            else if (errno != ENOENT) { perror(argv[i]); return 1; }
        }
        if (!alive) return 0;
        usleep(10000);
    }
    fprintf(stderr, "captured BPF identities survived retirement\n");
    return 1;
}
