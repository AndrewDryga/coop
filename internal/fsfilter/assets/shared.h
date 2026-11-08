/* SPDX-License-Identifier: MIT
 * Helpers are trusted writers. Each independent admission publishes once under
 * its held cgroup identity; only its owner reference changes denial to access.
 */
struct owner_value { uint64_t task, token, published; };
struct kernel_ref { char kind, name[64]; uint32_t id; int fd; };
struct shared_identity {
    char root[96], boot[64];
    uint64_t inode, device, pin_inode, pin_device;
    struct kernel_ref refs[96];
    int count, directory;
};
static struct shared_identity shared;
static const char *custody_phase = "arguments";

static int kernel_filesystems(void) {
    struct statfs cg, bp;
    if (statfs("/host-cgroup", &cg) || statfs("/host-bpf", &bp)) return -1;
    if (cg.f_type != CGROUP2_SUPER_MAGIC || bp.f_type != BPF_FS_MAGIC) { errno = ENOTSUP; return -1; }
    return 0;
}
static int decimal(const char *text, uint64_t *value) {
    char *end; errno = 0;
    if (!*text || strspn(text, "0123456789") != strlen(text)) { errno = EINVAL; return -1; }
    *value = strtoull(text, &end, 10);
    return errno || *end ? -1 : 0;
}
static int owned_name(const char *name, const char *prefix) {
    size_t n = strlen(prefix);
    return !strncmp(name, prefix, n) && strlen(name+n) == 32 && strspn(name+n, "0123456789abcdef") == 32;
}
static int path_at(char *path, const char *base, const char *name) {
    if (snprintf(path, PATH_MAX, "%s/%s", base, name) >= PATH_MAX) { errno = ENAMETOOLONG; return -1; }
    return 0;
}
/* Under Docker's systemd cgroup driver the controller and each admission are
 * slices systemd created (fsfilter.ControllerSlice and AdmissionSlice); the
 * helper never makes or names cgroups behind systemd's back. Otherwise they are
 * plain directories this helper makes. */
static int systemd_slices(void) {
    const char *value = getenv("COOP_FS_CGROUPS");
    return value && !strcmp(value, "systemd");
}
/* Callers pass only names owned_name accepted, so a stem fits a 96-byte buffer. */
static void slice_stem(char *out, const char *name) {
    for (; *name; name++) *out++ = *name == '-' ? '_' : *name;
    *out = 0;
}
static int controller_path(char *path, const char *root) {
    if (!systemd_slices()) return path_at(path, "/host-cgroup", root);
    char stem[96]; slice_stem(stem, root);
    if (snprintf(path, PATH_MAX, "/host-cgroup/%s.slice", stem) >= PATH_MAX) { errno = ENAMETOOLONG; return -1; }
    return 0;
}
static int admission_name(char *name, const char *root, const char *parent) {
    char a[96], b[96]; slice_stem(a, root); slice_stem(b, parent);
    int n = systemd_slices() ? snprintf(name, 256, "%s-%s.slice", a, b) : snprintf(name, 256, "%s", parent);
    if (n >= 256) { errno = ENAMETOOLONG; return -1; }
    return 0;
}
static int empty_group(int fd) {
    int copy = openat(fd, ".", O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
    if (copy < 0) return -1;
    DIR *dir = fdopendir(copy);
    if (!dir) { close(copy); return -1; }
    int result = 0;
    for (;;) {
        errno = 0; struct dirent *e = readdir(dir);
        if (!e) { if (errno) result = -1; break; }
        if (!strcmp(e->d_name,".") || !strcmp(e->d_name,"..")) continue;
        struct stat st;
        if (fstatat(copy,e->d_name,&st,AT_SYMLINK_NOFOLLOW)) { result = -1; break; }
        if (S_ISDIR(st.st_mode)) { errno = EBUSY; result = -1; break; }
    }
    closedir(dir);
    if (result) return result;
    int procs = openat(fd,"cgroup.procs",O_RDONLY|O_NOFOLLOW|O_CLOEXEC);
    if (procs < 0) return -1;
    char c; ssize_t n = read(procs,&c,1); int saved = errno; close(procs);
    if (n != 0) { errno = n < 0 ? saved : EBUSY; return -1; }
    return 0;
}
static int object_kind(int fd, char kind) {
    char path[64], target[64];
    snprintf(path,sizeof(path),"/proc/self/fd/%d",fd);
    ssize_t len=readlink(path,target,sizeof(target)-1);
    if(len<0)return -1;
    target[len]=0;
    const char *expected=kind=='m'?"anon_inode:bpf-map":kind=='p'?"anon_inode:bpf-prog":"anon_inode:bpf-link";
    if(strcmp(target,expected)){errno=ESTALE;return -1;}
    return 0;
}
static uint32_t object_id(int fd, char kind) {
    if(object_kind(fd,kind))return 0;
    if (kind == 'm') { struct bpf_map_info info = {0}; uint32_t n = sizeof(info); return bpf_obj_get_info_by_fd(fd,&info,&n) ? 0 : info.id; }
    struct bpf_prog_info info = {0}; uint32_t n = sizeof(info); return bpf_obj_get_info_by_fd(fd,&info,&n) ? 0 : info.id;
}
static int object_path(char *path, char kind, const char *name) {
    if (snprintf(path,PATH_MAX,"%s/%s_%s",pin_directory,kind=='m'?"map":"program",name) >= PATH_MAX) { errno=ENAMETOOLONG;return -1; }
    return 0;
}
static int sync_output(FILE *out, const char *temporary, const char *destination) {
    int result = fflush(out) || fsync(fileno(out));
    if (fclose(out)) result = -1;
    if (result || rename(temporary,destination)) return -1;
    int dir = open("/state",O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
    if (dir < 0) return -1;
    result = fsync(dir);close(dir);return result;
}
static int controller_birth(FILE *out, const char *root, const char *boot) {
    struct stat cg, pin;
    if (fstat(group,&cg) || lstat(pin_directory,&pin)) return -1;
    fprintf(out,"{\"version\":1,\"root\":\"%s\",\"boot_id\":\"%s\",\"cgroup_inode\":%llu,\"cgroup_device\":%llu,\"pin_directory_inode\":%llu,\"pin_directory_device\":%llu,\"maps\":[",root,boot,(unsigned long long)cg.st_ino,(unsigned long long)cg.st_dev,(unsigned long long)pin.st_ino,(unsigned long long)pin.st_dev);
    struct bpf_map *map; int count=0;
    bpf_object__for_each_map(map,object) {
        uint32_t id=object_id(bpf_map__fd(map),'m');if(!id)return -1;
        fprintf(out,"%s{\"name\":\"%s\",\"id\":%u}",count++?",":"",bpf_map__name(map),id);
    }
    fputs("],\"programs\":[",out);struct bpf_program *program;count=0;
    bpf_object__for_each_program(program,object) {
        uint32_t id=object_id(bpf_program__fd(program),'p');if(!id)return -1;
        fprintf(out,"%s{\"name\":\"%s\",\"id\":%u}",count++?",":"",bpf_program__name(program),id);
    }
    return fputs("]}",out)<0?-1:0;
}
static int invoke(int fd, uint64_t id, uint64_t token) {
    uint64_t args[2]={id,token};
    struct bpf_test_run_opts run={.sz=sizeof(run),.ctx_in=args,.ctx_size_in=sizeof(args)};
    if(bpf_prog_test_run_opts(fd,&run))return -1;
    if(run.retval){fprintf(stderr,"owner publication rejected: %u\n",run.retval);errno=EPERM;return -1;}return 0;
}
static int install_controller(const char *root,const char *boot) {
    if(!owned_name(root,"coop-fs-controller-") || strlen(boot)!=36 || kernel_filesystems())return -1;
    char path[PATH_MAX];if(controller_path(path,root)||(!systemd_slices()&&mkdir(path,0700)))return -1;
    group=open(path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(group<0||empty_group(group))return -1;
    struct bpf_object_open_opts options={.sz=sizeof(options),.kernel_log_buf=verifier_log,.kernel_log_size=sizeof(verifier_log)};
    object=bpf_object__open_file("/usr/local/libexec/coop-fs/filter.o",&options);
    if(!object||validate_hooks()||bpf_object__load(object)||load_normalizer(&options))return -1;
    struct bpf_program *inspect=bpf_object__find_program_by_name(object,"hierarchy_root");
    uint64_t ancestors[64]={0};struct bpf_test_run_opts run={.sz=sizeof(run),.ctx_in=ancestors,.ctx_size_in=sizeof(ancestors)};
    struct stat hierarchy;
    if(!inspect)return -1;
    if(bpf_prog_test_run_opts(bpf_program__fd(inspect),&run)||run.retval)return -1;
    if(stat("/host-cgroup",&hierarchy))return -1;
    uint32_t depth=0;
    for(uint32_t i=0;i<62;i++)if(ancestors[i]==hierarchy.st_ino){depth=i+2;break;}
    if(!depth){fprintf(stderr,"cgroup mount is outside the controller ancestry\n");errno=ENOTSUP;return -1;}
    uint32_t zero=0;uint64_t controller=0;
    if(bpf_map_update_elem(map_fd("dispatch"),&zero,&depth,BPF_ANY)||bpf_map_freeze(map_fd("dispatch")))return -1;
    if(bpf_map_update_elem(map_fd("scope"),&zero,&group,BPF_ANY)||bpf_map_freeze(map_fd("scope")))return -1;
    for(int i=0;i<4;i++)if(bpf_map_delete_elem(map_fd(policy_names[i]),&controller)&&errno!=ENOENT)return -1;
    FILE *in=fopen("/usr/share/coop-fs/unicode.bin","rb");if(!in)return -1;
    uint32_t key;struct rune_mapping value;
    while(fread(&key,sizeof(key),1,in)==1)if(read_exact(in,&value,sizeof(value))||bpf_map_update_elem(map_fd("runes"),&key,&value,BPF_NOEXIST)){fclose(in);return -1;}
    int bad=!feof(in)||ferror(in);fclose(in);if(bad||bpf_map_freeze(map_fd("runes")))return -1;
    struct owner_value owner={0};
    if(bpf_map_update_elem(map_fd("owners"),&controller,&owner,BPF_NOEXIST))return -1;
    struct bpf_program *admit=bpf_object__find_program_by_name(object,"admit_owner");
    if(!admit||invoke(bpf_program__fd(admit),0,0))return -1;
    if(path_at(pin_directory,"/host-bpf",root)||mkdir(pin_directory,0700))return -1;
    FILE *out=fopen("/state/birth.json.new","wx");if(!out)return -1;
    if(controller_birth(out,root,boot)){fclose(out);return -1;}
    if(sync_output(out,"/state/birth.json.new","/state/birth.json"))return -1;
    struct bpf_map *map;
    bpf_object__for_each_map(map,object)if(object_path(path,'m',bpf_map__name(map))||bpf_map__pin(map,path))return -1;
    struct bpf_program *program;
    bpf_object__for_each_program(program,object) {
        if(object_path(path,'p',bpf_program__name(program))||bpf_program__pin(program,path))return -1;
        if(strncmp(bpf_program__name(program),"guard_",6))continue;
        if(nattached>=32){errno=E2BIG;return -1;}
        struct bpf_link *link=bpf_program__attach_lsm(program);if(!link)return -1;
        attached[nattached]=link;
        if(path_at(link_paths[nattached],pin_directory,bpf_program__name(program))||bpf_link__pin(link,link_paths[nattached]))return -1;
        nattached++;
    }
    fputs("{\"status\":\"controller-ready\",\"birth\":",stdout);
    if(controller_birth(stdout,root,boot))return -1;
    fputs(",\"links\":[",stdout);
    for(int i=0;i<nattached;i++) {
        struct bpf_link_info info={0};uint32_t size=sizeof(info);
        if(bpf_obj_get_info_by_fd(bpf_link__fd(attached[i]),&info,&size))return -1;
        printf("%s{\"name\":\"%s\",\"id\":%u,\"program\":%u}",i?",":"",strrchr(link_paths[i],'/')+1,info.id,info.prog_id);
    }
    puts("]}");if(fflush(stdout))return -1;
    for(;;)pause();
}

static int known_ref(char kind,const char *name) {
    if(kind=='m') {
        const char *names[]={"scope","dispatch","owners","runes","work","objects","paths","devices","config","objects_template","paths_template","devices_template","config_template"};
        for(unsigned i=0;i<sizeof(names)/sizeof(names[0]);i++)if(!strcmp(name,names[i]))return 1;
    } else if(kind=='p'||kind=='l') {
        if(kind=='p'&&(!strcmp(name,"admit_owner")||!strcmp(name,"release_owner")||!strcmp(name,"hierarchy_root")))return 1;
        for(unsigned i=0;i<sizeof(hooks)/sizeof(hooks[0]);i++)if(!strncmp(name,"guard_",6)&&!strcmp(name+6,hooks[i].name))return 1;
    }
    return 0;
}
static int parse_shared(int argc,char **argv) {
    if((argc!=5+13+30&&argc!=5+13+30+27)||!owned_name(argv[0],"coop-fs-controller-")||kernel_filesystems()){errno=EINVAL;return -1;}
    strcpy(shared.root,argv[0]);
    if(decimal(argv[1],&shared.inode)||decimal(argv[2],&shared.device)||decimal(argv[3],&shared.pin_inode)||decimal(argv[4],&shared.pin_device)||!shared.inode||!shared.device||!shared.pin_inode||!shared.pin_device)return -1;
    if(path_at(pin_directory,"/host-bpf",shared.root))return -1;
    for(int i=5;i<argc;i++) {
        struct kernel_ref *r=&shared.refs[shared.count];char extra;
        if(sscanf(argv[i],"%c:%63[^:]:%u%c",&r->kind,r->name,&r->id,&extra)!=3||!r->id||!known_ref(r->kind,r->name)){errno=EINVAL;return -1;}
        for(int j=0;j<shared.count;j++)if(shared.refs[j].kind==r->kind&&(!strcmp(shared.refs[j].name,r->name)||shared.refs[j].id==r->id)){errno=EINVAL;return -1;}
        r->fd=-1;shared.count++;
    }
    int maps=0,programs=0,links=0;
    for(int i=0;i<shared.count;i++){maps+=shared.refs[i].kind=='m';programs+=shared.refs[i].kind=='p';links+=shared.refs[i].kind=='l';}
    if(maps!=13||programs!=30||(links&&links!=27)){errno=EINVAL;return -1;}
    return 0;
}
static int verified_directory(const char *path,uint64_t inode,uint64_t device) {
    int fd=open(path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);if(fd<0)return -1;
    struct stat st;if(fstat(fd,&st)||st.st_ino!=inode||st.st_dev!=device){close(fd);errno=ESTALE;return -1;}return fd;
}
/* Reject a substituted descriptor before any map mutation or unlink. IDs are
 * scoped by object kind, so an ID comparison alone is insufficient. */
static int object_schema(struct kernel_ref *r) {
    if(object_id(r->fd,r->kind)!=r->id){errno=ESTALE;return -1;}
    if(r->kind=='p') {
        struct bpf_prog_info info={0};uint32_t size=sizeof(info);
        if(bpf_obj_get_info_by_fd(r->fd,&info,&size))return -1;
        if(info.type!=(!strncmp(r->name,"guard_",6)?BPF_PROG_TYPE_LSM:BPF_PROG_TYPE_SYSCALL)){errno=ESTALE;return -1;}
        return 0;
    }
    uint32_t type=BPF_MAP_TYPE_HASH,key=4,value=4,max=32,flags=0;
    if(!strcmp(r->name,"scope")){type=BPF_MAP_TYPE_CGROUP_ARRAY;max=1;}
    else if(!strcmp(r->name,"dispatch")){type=BPF_MAP_TYPE_ARRAY;max=1;}
    else if(!strcmp(r->name,"owners")){key=8;value=24;max=1025;flags=BPF_F_NO_PREALLOC;}
    else if(!strcmp(r->name,"runes")){value=sizeof(struct rune_mapping);max=20000;}
    else if(!strcmp(r->name,"work")){type=BPF_MAP_TYPE_TASK_STORAGE;value=6400;max=0;flags=BPF_F_NO_PREALLOC;}
    else if(!strstr(r->name,"_template")){type=BPF_MAP_TYPE_HASH_OF_MAPS;key=8;max=1024;}
    else if(!strcmp(r->name,"objects_template")){key=sizeof(struct object_key);value=sizeof(struct label);max=LIMIT;flags=BPF_F_NO_PREALLOC;}
    else if(!strcmp(r->name,"paths_template")){key=sizeof(struct path_key);max=LIMIT;flags=BPF_F_NO_PREALLOC;}
    else if(!strcmp(r->name,"config_template"))value=sizeof(struct settings);
    struct bpf_map_info info={0};uint32_t size=sizeof(info);
    if(bpf_obj_get_info_by_fd(r->fd,&info,&size))return -1;
    if(info.type!=type||info.key_size!=key||info.value_size!=value||info.max_entries!=max||info.map_flags!=flags){errno=ESTALE;return -1;}
    return 0;
}
static int open_shared(int missing) {
    shared.directory=verified_directory(pin_directory,shared.pin_inode,shared.pin_device);
    if(shared.directory<0)return -1;
    for(int i=0;i<shared.count;i++) {
        struct kernel_ref *r=&shared.refs[i];if(r->kind=='l')continue;char path[PATH_MAX];if(object_path(path,r->kind,r->name))return -1;
        r->fd=bpf_obj_get(path);
        if(r->fd<0){if(missing&&errno==ENOENT)continue;return -1;}
        if(object_schema(r))return -1;
    }
    return 0;
}
static int shared_fd(char kind,const char *name) {
    for(int i=0;i<shared.count;i++)if(shared.refs[i].kind==kind&&!strcmp(shared.refs[i].name,name))return shared.refs[i].fd;
    errno=EINVAL;return -1;
}
static void map_json(FILE *out) {
    fputc('[',out);
    for(int i=0;i<4;i++)fprintf(out,"%s{\"name\":\"%s\",\"id\":%u}",i?",":"",policy_names[i],object_id(policy_maps[i],'m'));
    fputc(']',out);
}
static int own_admission(const char *sources,const char *parent,const char *boot,uint64_t token) {
    custody_phase="open controller";
    if(!owned_name(parent,"coop-fs-")||strlen(boot)!=36||!token||open_shared(0))return -1;
    custody_phase="create admission cgroup";
    char root[PATH_MAX],path[PATH_MAX],name[256];if(controller_path(root,shared.root)||admission_name(name,shared.root,parent))return -1;
    int parent_fd=verified_directory(root,shared.inode,shared.device);if(parent_fd<0)return -1;
    if(!systemd_slices()&&mkdirat(parent_fd,name,0700)){close(parent_fd);return -1;}
    group=openat(parent_fd,name,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);close(parent_fd);
    if(group<0||empty_group(group)||path_at(path,root,name))return -1;
    custody_phase="create policy maps";
    for(int i=0;i<4;i++) {
        char template[64];snprintf(template,sizeof(template),"%s_template",policy_names[i]);
        struct bpf_map_info info={0};uint32_t size=sizeof(info);
        if(bpf_obj_get_info_by_fd(shared_fd('m',template),&info,&size))return -1;
        struct bpf_map_create_opts opts={.sz=sizeof(opts),.map_flags=info.map_flags};
        policy_maps[i]=bpf_map_create(info.type,policy_names[i],info.key_size,info.value_size,info.max_entries,&opts);
        if(policy_maps[i]<0)return -1;
    }
    custody_phase="read policy";
    char magic[8];uint32_t header[5];FILE *in=stdin;
    if(read_exact(in,magic,sizeof(magic))||memcmp(magic,"COOPFS03",8)||read_exact(in,header,sizeof(header))||!header[0]||header[0]>32||header[1]>LIMIT||!header[2]||header[2]>20000||header[3]>LIMIT-header[0]||header[4]>LIMIT-header[0]-header[3]){errno=EINVAL;return -1;}
    custody_phase="seed policy";
    pins=calloc(header[0]+header[3]+header[4],sizeof(*pins));if(!pins||seed_policy(in,header,sources))return -1;
    fclose(in);
    custody_phase="persist admission birth";
    struct stat st;if(fstat(group,&st))return -1;
    uint64_t key=st.st_ino;
    FILE *out=fopen("/state/birth.json.new","wx");if(!out)return -1;
    fprintf(out,"{\"version\":2,\"parent\":\"%s\",\"controller\":\"%s\",\"boot_id\":\"%s\",\"cgroup_inode\":%llu,\"cgroup_device\":%llu,\"token\":%llu,\"maps\":",parent,shared.root,boot,(unsigned long long)key,(unsigned long long)st.st_dev,(unsigned long long)token);
    map_json(out);fputs("}\n",out);
    if(sync_output(out,"/state/birth.json.new","/state/birth.json"))return -1;
    custody_phase="publish policy tables";
    struct owner_value owner={.token=token};
    if(bpf_map_update_elem(shared_fd('m',"owners"),&key,&owner,BPF_NOEXIST))return -1;
    for(int i=0;i<4;i++)if(bpf_map_update_elem(shared_fd('m',policy_names[i]),&key,&policy_maps[i],BPF_NOEXIST))return -1;
    custody_phase="publish owner";
    if(invoke(shared_fd('p',"admit_owner"),key,token))return -1;
    printf("{\"status\":\"ready\",\"parent\":\"/%s\",\"controller\":\"%s\",\"boot_id\":\"%s\",\"cgroup_inode\":%llu,\"token\":%llu,\"pins\":%d,\"maps\":",parent,shared.root,boot,(unsigned long long)key,(unsigned long long)token,npins);
    map_json(stdout);puts("}");if(fflush(stdout))return -1;
    for(;;)pause();
}

/* Retirement acquires all surviving owned references before its first mutation.
 * The host additionally excludes every stopped/restartable Docker consumer. */
static int release_admission(const char *parent,uint64_t inode,uint64_t device,uint64_t token,uint32_t *ids) {
    char root[PATH_MAX],path[PATH_MAX],name[256];if(!owned_name(parent,"coop-fs-")||controller_path(root,shared.root)||admission_name(name,shared.root,parent)||path_at(path,root,name))return -1;
    int rootfd=verified_directory(root,shared.inode,shared.device);
    if(rootfd<0)return -1;
    close(rootfd);
    group=verified_directory(path,inode,device);
    /* A slice whose stop failed after its cgroup was removed can get a fresh, empty cgroup from
     * systemd. It is not this admission's; retirement treats it as gone and the stop removes it. */
    if(group<0&&errno==ESTALE&&systemd_slices()){int fd=open(path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);int fresh=fd>=0&&!empty_group(fd);if(fd>=0)close(fd);if(fresh)errno=ENOENT;else errno=ESTALE;}
    if(group<0&&errno!=ENOENT)return -1;
    if((group>=0&&empty_group(group))||open_shared(0))return -1;
    int owner_map=shared_fd('m',"owners");struct owner_value owner={0};
    int present=bpf_map_lookup_elem(owner_map,&inode,&owner)==0;
    if((!present&&errno!=ENOENT)||(present&&owner.token!=token)){errno=ESTALE;return -1;}
    int found[4]={0},fds[4]={-1,-1,-1,-1},result=-1;
    for(int i=0;i<4;i++) {
        uint32_t id=0;int map=shared_fd('m',policy_names[i]);
        if(bpf_map_lookup_elem(map,&inode,&id)){if(errno==ENOENT)continue;goto done;}
        if(id!=ids[i]){errno=ESTALE;goto done;}
        fds[i]=bpf_map_get_fd_by_id(id);if(fds[i]<0)goto done;found[i]=1;
    }
    if(present&&invoke(shared_fd('p',"release_owner"),inode,token))goto done;
    if(present&&bpf_map_delete_elem(owner_map,&inode))goto done;
    for(int i=0;i<4;i++)if(found[i]&&bpf_map_delete_elem(shared_fd('m',policy_names[i]),&inode))goto done;
    result=group<0?0:rmdir(path);
done:
    for(int i=0;i<4;i++)if(fds[i]>=0)close(fds[i]);
    if(result)return result;
    /* The host stopped the owner before retirement. Wait for deferred kernel
     * destruction rather than calling table deletion proof of disappearance. */
    for(int attempt=0;attempt<1000;attempt++) {
        int remaining=0;
        for(int i=0;i<4;i++) {
            int fd=bpf_map_get_fd_by_id(ids[i]);
            if(fd>=0){close(fd);remaining=1;}else if(errno!=ENOENT)return -1;
        }
        if(!remaining)return 0;
        struct timespec delay={.tv_nsec=5000000};nanosleep(&delay,NULL);
    }
    errno=EBUSY;return -1;
}
static int known_pin(const char *name) {
    for(int i=0;i<shared.count;i++) {
        char expected[96];snprintf(expected,sizeof(expected),"%s_%s",shared.refs[i].kind=='m'?"map":"program",shared.refs[i].name);
        if(!strcmp(name,expected))return 1;
        if(shared.refs[i].kind=='p'&&!strncmp(shared.refs[i].name,"guard_",6)&&!strcmp(name,shared.refs[i].name))return 1;
    }
    return 0;
}
static int release_controller(void) {
    char root[PATH_MAX];if(controller_path(root,shared.root))return -1;
    group=verified_directory(root,shared.inode,shared.device);
    if(group<0){struct stat st;if(errno==ENOENT&&lstat(pin_directory,&st)&&errno==ENOENT)return 0;return -1;}
    if(empty_group(group))return -1;
    if(open_shared(1)){if(errno==ENOENT){struct stat st;if(lstat(pin_directory,&st)&&errno==ENOENT)return rmdir(root);}return -1;}
    int copy=openat(shared.directory,".",O_RDONLY|O_DIRECTORY|O_CLOEXEC);if(copy<0)return -1;
    DIR *dir=fdopendir(copy);if(!dir){close(copy);return -1;}
    for(;;){errno=0;struct dirent *e=readdir(dir);if(!e){int err=errno;closedir(dir);if(err){errno=err;return -1;}break;}if(strcmp(e->d_name,".")&&strcmp(e->d_name,"..")&&!known_pin(e->d_name)){closedir(dir);errno=ESTALE;return -1;}}
    int links[32],count=0;char paths[32][PATH_MAX];
    for(int i=0;i<shared.count;i++) {
        struct kernel_ref *r=&shared.refs[i];if(r->kind!='p'||strncmp(r->name,"guard_",6))continue;
        uint32_t expected_link=0;
        for(int j=0;j<shared.count;j++)if(shared.refs[j].kind=='l'&&!strcmp(shared.refs[j].name,r->name))expected_link=shared.refs[j].id;
        if(path_at(paths[count],pin_directory,r->name))return -1;
        int fd=bpf_obj_get(paths[count]);if(fd<0){if(errno==ENOENT)continue;return -1;}
        struct bpf_link_info info={0};uint32_t size=sizeof(info);
        if(object_kind(fd,'l')||bpf_obj_get_info_by_fd(fd,&info,&size)||info.type!=BPF_LINK_TYPE_TRACING||info.prog_id!=r->id||(expected_link&&info.id!=expected_link)){close(fd);errno=ESTALE;return -1;}
        links[count++]=fd;
    }
    for(int i=0;i<count;i++)if(unlink(paths[i]))return -1;
    for(int i=0;i<shared.count;i++)if(shared.refs[i].kind!='l'&&shared.refs[i].fd>=0){char path[PATH_MAX];if(object_path(path,shared.refs[i].kind,shared.refs[i].name)||unlink(path))return -1;}
    if(rmdir(pin_directory)||rmdir(root))return -1;
    for(int i=0;i<count;i++)close(links[i]);
    return 0;
}
static int abandon(const char *controller,const char *parent) {
    if(!owned_name(controller,"coop-fs-controller-")||(parent&&!owned_name(parent,"coop-fs-"))||kernel_filesystems())return -1;
    char root[PATH_MAX],path[PATH_MAX],name[256];if(controller_path(root,controller))return -1;
    if(parent){if(admission_name(name,controller,parent)||path_at(path,root,name))return -1;}else strcpy(path,root);
    int fd=open(path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
    if(fd>=0){int result=empty_group(fd);close(fd);if(result)return -1;}else if(errno!=ENOENT)return -1;
    if(!parent){char pin[PATH_MAX];if(path_at(pin,"/host-bpf",controller))return -1;if(rmdir(pin)&&errno!=ENOENT)return -1;}
    return rmdir(path)&&errno!=ENOENT?-1:0;
}
int main(int argc,char **argv) {
    int result=-1;
    if(argc==4&&!strcmp(argv[1],"controller"))result=install_controller(argv[2],argv[3]);
    else if((argc==3||argc==4)&&!strcmp(argv[1],"abandon"))result=abandon(argv[2],argc==4?argv[3]:NULL);
    else if(argc>10&&!strcmp(argv[1],"owner")) {
        uint64_t token;if(!decimal(argv[5],&token)&&!parse_shared(argc-6,argv+6))result=own_admission(argv[2],argv[3],argv[4],token);
    } else if(argc>10&&!strcmp(argv[1],"release-run")) {
        uint64_t inode,device,token,values[4];uint32_t ids[4];int valid=1;
        for(int i=0;i<4;i++){if(decimal(argv[6+i],&values[i])||values[i]>UINT32_MAX||!values[i])valid=0;ids[i]=values[i];}
        if(valid&&!decimal(argv[3],&inode)&&!decimal(argv[4],&device)&&!decimal(argv[5],&token)&&inode&&device&&token&&!parse_shared(argc-10,argv+10))result=release_admission(argv[2],inode,device,token,ids);
    } else if(argc>6&&!strcmp(argv[1],"release-controller")&&!parse_shared(argc-2,argv+2))result=release_controller();
    else errno=EINVAL;
    if(result){fprintf(stderr,"filesystem custody (%s): %s\n%s",custody_phase,strerror(errno),verifier_log);return 1;}return 0;
}
