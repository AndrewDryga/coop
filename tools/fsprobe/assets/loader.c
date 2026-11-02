#define _GNU_SOURCE
#include <linux/bpf.h>
#include <linux/btf.h>
#include <sys/syscall.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <sys/prctl.h>
#include <dirent.h>
#include <limits.h>
#include <sys/wait.h>
#include <sys/mount.h>
#include <fcntl.h>
#include <unistd.h>
#include <stdio.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>

/* Qualification only: cgroup-scoped fixed fixture rules, never production policy. */
static int bpf_call(int cmd, union bpf_attr *a) { return syscall(__NR_bpf, cmd, a, sizeof(*a)); }
static size_t extra_size(unsigned kind, unsigned count) {
    switch (kind) {
    case BTF_KIND_INT: return 4;
    case BTF_KIND_ARRAY: return sizeof(struct btf_array);
    case BTF_KIND_STRUCT: case BTF_KIND_UNION: return count * sizeof(struct btf_member);
    case BTF_KIND_ENUM: return count * sizeof(struct btf_enum);
    case BTF_KIND_FUNC_PROTO: return count * sizeof(struct btf_param);
    case BTF_KIND_VAR: return sizeof(struct btf_var);
    case BTF_KIND_DATASEC: return count * sizeof(struct btf_var_secinfo);
    case BTF_KIND_DECL_TAG: return sizeof(struct btf_decl_tag);
    case BTF_KIND_ENUM64: return count * sizeof(struct btf_enum64);
    default: return 0;
    }
}
#define INS(c,d,s,o,i) ((struct bpf_insn){.code=(c),.dst_reg=(d),.src_reg=(s),.off=(o),.imm=(i)})
#define MOVI(d,i) INS(BPF_ALU64|BPF_MOV|BPF_K,d,0,0,i)
#define MOVR(d,s) INS(BPF_ALU64|BPF_MOV|BPF_X,d,s,0,0)
#define LOAD(sz,d,s,o) INS(BPF_LDX|BPF_MEM|sz,d,s,o,0)
#define STORE(sz,d,s,o) INS(BPF_STX|BPF_MEM|sz,d,s,o,0)
#define CALL(i) INS(BPF_JMP|BPF_CALL,0,0,0,i)
#define EXIT INS(BPF_JMP|BPF_EXIT,0,0,0,0)
static void imm64(struct bpf_insn *p, int reg, uint64_t value) {
    p[0] = INS(BPF_LD|BPF_DW|BPF_IMM,reg,0,0,(uint32_t)value);
    p[1] = INS(0,0,0,0,value >> 32);
}

static char *btf, *strings;
static size_t btf_size, string_size;
static const struct btf_type *types[200000];
static int type_count;
static void fail(const char *what) { perror(what); exit(2); }
static const char *name_at(unsigned offset) {
    if (offset >= string_size || !memchr(strings + offset, 0, string_size - offset)) {
        errno = EINVAL; fail("invalid BTF string");
    }
    return strings + offset;
}
static void read_btf(void) {
    FILE *f = fopen("/sys/kernel/btf/vmlinux", "rb");
    if (!f) fail("BTF unavailable");
    if (fseek(f, 0, SEEK_END)) fail("seek BTF");
    long size = ftell(f);
    if (size < (long)sizeof(struct btf_header) || size > 32*1024*1024) { errno=EINVAL; fail("BTF size"); }
    rewind(f); btf_size=size; btf=malloc(btf_size);
    if (!btf || fread(btf, 1, btf_size, f) != btf_size) fail("read BTF");
    fclose(f);
    struct btf_header *h=(void *)btf;
    if (h->magic != BTF_MAGIC || h->version != 1 || h->hdr_len > btf_size ||
        (uint64_t)h->hdr_len+h->str_off+h->str_len > btf_size ||
        (uint64_t)h->hdr_len+h->type_off+h->type_len > btf_size) { errno=EINVAL; fail("BTF header"); }
    strings=btf+h->hdr_len+h->str_off; string_size=h->str_len;
    size_t p=h->hdr_len+h->type_off, end=p+h->type_len;
    while (p<end) {
        if (end-p<sizeof(struct btf_type) || type_count+1 >= 200000) { errno=EINVAL; fail("BTF types"); }
        const struct btf_type *t=(void *)(btf+p);
        unsigned kind=BTF_INFO_KIND(t->info);
        if (kind>BTF_KIND_ENUM64) { errno=ENOTSUP; fail("BTF kind"); }
        size_t n=sizeof(*t)+extra_size(kind,BTF_INFO_VLEN(t->info));
        if (n>end-p) { errno=EINVAL; fail("BTF type bounds"); }
        types[++type_count]=t; name_at(t->name_off); p+=n;
    }
}
static int member_offset(const struct btf_type *t, const char *field, int depth) {
    unsigned kind=BTF_INFO_KIND(t->info);
    if (depth>8 || (kind!=BTF_KIND_STRUCT && kind!=BTF_KIND_UNION)) return -1;
    const struct btf_member *m=(const void *)(t+1);
    for (unsigned j=0;j<BTF_INFO_VLEN(t->info);j++) {
        unsigned bits=BTF_INFO_KFLAG(t->info)?BTF_MEMBER_BIT_OFFSET(m[j].offset):m[j].offset;
        if (bits%8) continue;
        const char *name=name_at(m[j].name_off);
        if (!strcmp(name,field)) return bits/8;
        if (!*name && m[j].type>0 && m[j].type<=(unsigned)type_count) {
            int inner=member_offset(types[m[j].type],field,depth+1);
            if (inner>=0) return bits/8+inner;
        }
    }
    return -1;
}
static int offset(const char *type, const char *field) {
    for (int i=1;i<=type_count;i++) {
        const struct btf_type *t=types[i];
        if (BTF_INFO_KIND(t->info)==BTF_KIND_STRUCT && !strcmp(name_at(t->name_off),type)) {
            int v=member_offset(t,field,0);
            if (v>=0 && v<32768) return v;
        }
    }
    fprintf(stderr,"missing BTF field %s.%s\n",type,field); exit(2);
}
static int hook(const char *name, const char *arg, int *position) {
    char full[80]; snprintf(full,sizeof(full),"bpf_lsm_%s",name);
    for (int i=1;i<=type_count;i++) {
        const struct btf_type *t=types[i];
        if (BTF_INFO_KIND(t->info)!=BTF_KIND_FUNC || strcmp(name_at(t->name_off),full)) continue;
        if (!t->type || t->type>(unsigned)type_count) break;
        t=types[t->type];
        if (BTF_INFO_KIND(t->info)!=BTF_KIND_FUNC_PROTO) break;
        const struct btf_param *p=(const void *)(t+1);
        for (unsigned j=0;j<BTF_INFO_VLEN(t->info);j++) {
            if (!strcmp(name_at(p[j].name_off),arg)) { *position=j*8; return i; }
        }
    }
    fprintf(stderr,"missing BTF hook argument %s.%s\n",name,arg); exit(2);
}

struct rule { uint64_t ino, parent; unsigned dev; char name[64]; int policy; };
static struct rule rules[16]; static int nrules;
static void add_rule(const char *parent, const char *name, int policy) {
    struct stat st, dir; char path[PATH_MAX];
    snprintf(path,sizeof(path),"%s/%s",parent,name);
    if (stat(parent,&dir) || stat(path,&st)) fail("fixture stat");
    if (st.st_dev!=dir.st_dev || nrules>=16 || strlen(name)>=64) { errno=EINVAL; fail("fixture rule"); }
    struct rule *r=&rules[nrules++];
    r->ino=st.st_ino; r->parent=dir.st_ino; r->dev=(major(st.st_dev)<<20)|minor(st.st_dev);
    strcpy(r->name,name); r->policy=policy;
    for (char *p=r->name;*p;p++) if (*p>='A' && *p<='Z') *p+=32;
}
static struct bpf_insn code[8192]; static int count;
static int labels[256], nlabels;
struct patch { int at, label; }; static struct patch patches[2048]; static int npatches;
static void emit(struct bpf_insn i) { if (count>=8192) abort(); code[count++]=i; }
static int label(void) { if (nlabels>=256) abort(); labels[nlabels]=-1; return nlabels++; }
static void mark(int l) { labels[l]=count; }
static void jump(int op, int reg, int value, int l) {
    if (npatches>=2048) abort();
    patches[npatches++]=(struct patch){count,l};
    emit(INS(BPF_JMP|op|BPF_K,reg,0,0,value));
}
static void constant(int reg, uint64_t value) {
    if (count+2>8192) abort();
    imm64(code+count,reg,value); count+=2;
}
static void compare64(int reg, uint64_t value, int mismatch) {
    constant(5,value);
    if (npatches>=2048) abort();
    patches[npatches++]=(struct patch){count,mismatch};
    emit(INS(BPF_JMP|BPF_JNE|BPF_X,reg,5,0,0));
}
static int dino, dparent, dname, qname, ino, sb, dev, nlink, telemetry;
struct open_record {
    uint64_t first_ino, last_ino;
    uint32_t first_links, last_links;
    uint64_t opens;
    uint32_t dev, reserved;
};
static void map_register(int reg) {
    emit(INS(BPF_LD|BPF_DW|BPF_IMM,reg,BPF_PSEUDO_MAP_FD,0,telemetry));
    emit(INS(0,0,0,0,0));
}
static void stack_register(int reg, int at) {
    emit(MOVR(reg,10)); emit(INS(BPF_ALU64|BPF_ADD|BPF_K,reg,0,0,at));
}
// Record kernel metadata at file_open, before userspace can issue a refreshing
// stat. Only this inert fixture's device is observed, within the two cgroups.
static void observe_open(int arg, int deny) {
    int done=label(), fresh=label();
    emit(LOAD(BPF_DW,7,6,arg));
    emit(LOAD(BPF_DW,7,7,offset("file","f_path")+offset("path","dentry")));
    emit(LOAD(BPF_DW,7,7,dino)); jump(BPF_JEQ,7,0,done);
    emit(LOAD(BPF_DW,1,7,sb)); emit(LOAD(BPF_W,2,1,dev));
    jump(BPF_JNE,2,rules[0].dev,done);
    for (int at=-296;at<0;at+=8) emit(INS(BPF_ST|BPF_DW|BPF_MEM,10,0,at,0));
    emit(LOAD(BPF_DW,1,6,arg));
    emit(INS(BPF_ALU64|BPF_ADD|BPF_K,1,0,0,offset("file","f_path")));
    stack_register(2,-256); emit(MOVI(3,256)); emit(CALL(BPF_FUNC_d_path));
    jump(BPF_JSLE,0,0,deny);
    emit(LOAD(BPF_DW,9,7,ino)); emit(LOAD(BPF_W,8,7,nlink));
    map_register(1); stack_register(2,-256); emit(CALL(BPF_FUNC_map_lookup_elem));
    jump(BPF_JEQ,0,0,fresh);
    emit(STORE(BPF_DW,0,9,8)); emit(STORE(BPF_W,0,8,20));
    emit(LOAD(BPF_DW,1,0,24)); emit(INS(BPF_ALU64|BPF_ADD|BPF_K,1,0,0,1));
    emit(STORE(BPF_DW,0,1,24)); jump(BPF_JA,0,0,done);
    mark(fresh);
    emit(STORE(BPF_DW,10,9,-296)); emit(STORE(BPF_DW,10,9,-288));
    emit(STORE(BPF_W,10,8,-280)); emit(STORE(BPF_W,10,8,-276));
    emit(INS(BPF_ST|BPF_DW|BPF_MEM,10,0,-272,1));
    emit(INS(BPF_ST|BPF_W|BPF_MEM,10,0,-264,rules[0].dev));
    map_register(1); stack_register(2,-256); stack_register(3,-296);
    emit(MOVI(4,BPF_NOEXIST)); emit(CALL(BPF_FUNC_map_update_elem));
    jump(BPF_JNE,0,0,deny);
    mark(done);
}
static void literal(const char *name, int yes, int no) {
    jump(BPF_JNE,9,strlen(name)+1,no);
    for (unsigned j=0;j<strlen(name);j++) {
        unsigned char c=name[j]; emit(LOAD(BPF_B,1,10,-64+j));
        if (c>='a' && c<='z') emit(INS(BPF_ALU64|BPF_OR|BPF_K,1,0,0,32));
        jump(BPF_JNE,1,c,no);
    }
    jump(BPF_JA,0,0,yes);
}
// The experimental policy is basename .env, suffix *.pem, and a read-only
// .coopignore at any depth on the fixture device. Exact Unicode rules remain
// separate. Link counts are telemetry only: stale host-share attributes cannot
// classify secrets, and public hardlinks must remain usable.
static void patterns(int deny, int writes_only) {
    int done=label(), parent=label(), names=label(), pem=label(), policy=label();
    emit(LOAD(BPF_DW,1,7,dino)); jump(BPF_JEQ,1,0,parent);
    emit(LOAD(BPF_DW,2,1,sb)); emit(LOAD(BPF_W,2,2,dev));
    jump(BPF_JNE,2,rules[0].dev,done);
    jump(BPF_JA,0,0,names);
    mark(parent);
    emit(LOAD(BPF_DW,1,7,dparent)); emit(LOAD(BPF_DW,1,1,dino));
    jump(BPF_JEQ,1,0,done);
    emit(LOAD(BPF_DW,1,1,sb)); emit(LOAD(BPF_W,1,1,dev));
    jump(BPF_JNE,1,rules[0].dev,done);
    mark(names);
    for (int at=-64;at<0;at+=8) emit(INS(BPF_ST|BPF_DW|BPF_MEM,10,0,at,0));
    emit(LOAD(BPF_DW,3,7,dname+qname)); stack_register(1,-64); emit(MOVI(2,64));
    emit(CALL(BPF_FUNC_probe_read_kernel_str)); emit(MOVR(9,0));
    jump(BPF_JSLE,9,0,deny); jump(BPF_JGE,9,64,deny);
    literal(".env",deny,pem); mark(pem);
    jump(BPF_JLT,9,5,policy);
    // A bounded dynamic stack offset selects the suffix, independent of the
    // Unicode prefix, case normalization, or a replaced ancestor's identity.
    stack_register(3,-69); emit(INS(BPF_ALU64|BPF_ADD|BPF_X,3,9,0,0));
    for (int j=0;j<4;j++) {
        emit(LOAD(BPF_B,1,3,j));
        if (j) emit(INS(BPF_ALU64|BPF_OR|BPF_K,1,0,0,32));
        jump(BPF_JNE,1,".pem"[j],policy);
    }
    jump(BPF_JA,0,0,deny);
    mark(policy);
    if (writes_only) jump(BPF_JEQ,8,0,done);
    literal(".coopignore",deny,done);
    mark(done);
}
// r7 is a verifier-tracked dentry. Match its original inode or parent + basename.
static void match(const struct rule *r, int yes, int no) {
    int byname=label();
    emit(LOAD(BPF_DW,1,7,dino)); jump(BPF_JEQ,1,0,byname);
    emit(LOAD(BPF_DW,2,1,ino)); compare64(2,r->ino,byname);
    emit(LOAD(BPF_DW,1,1,sb)); emit(LOAD(BPF_W,2,1,dev)); jump(BPF_JNE,2,r->dev,byname);
    jump(BPF_JA,0,0,yes);
    mark(byname);
    emit(LOAD(BPF_DW,1,7,dparent)); jump(BPF_JEQ,1,0,no);
    emit(LOAD(BPF_DW,1,1,dino)); jump(BPF_JEQ,1,0,no);
    emit(LOAD(BPF_DW,2,1,ino)); compare64(2,r->parent,no);
    emit(LOAD(BPF_DW,1,1,sb)); emit(LOAD(BPF_W,2,1,dev)); jump(BPF_JNE,2,r->dev,no);
    emit(LOAD(BPF_DW,3,7,dname+qname));
    emit(MOVR(1,10)); emit(INS(BPF_ALU64|BPF_ADD|BPF_K,1,0,0,-64)); emit(MOVI(2,64));
    emit(CALL(BPF_FUNC_probe_read_kernel_str)); jump(BPF_JNE,0,strlen(r->name)+1,no);
    for (unsigned j=0;j<strlen(r->name);j++) {
        unsigned char c=r->name[j]; emit(LOAD(BPF_B,1,10,-64+j));
        if (c>='a' && c<='z') emit(INS(BPF_ALU64|BPF_OR|BPF_K,1,0,0,32));
        jump(BPF_JNE,1,c,no);
    }
    jump(BPF_JA,0,0,yes);
}
static int load_program(const char *name, const char *argument, const char *second, int is_file, int writes_only) {
    count=nlabels=npatches=0;
    int arg, arg2=0, id=hook(name,argument,&arg);
    if (second) hook(name,second,&arg2);
    int allow=label(), deny=label(); emit(MOVR(6,1));
    if (writes_only) {
        observe_open(arg,deny);
        emit(LOAD(BPF_DW,1,6,arg)); emit(LOAD(BPF_W,2,1,offset("file","f_flags")));
        emit(INS(BPF_ALU64|BPF_AND|BPF_K,2,0,0,O_ACCMODE|O_TRUNC));
        // Read opens can still match a secret, but must skip policy rules below.
        emit(MOVR(8,2));
    }
    for (int endpoint=0;endpoint<(second?2:1);endpoint++) {
        emit(LOAD(BPF_DW,7,6,endpoint?arg2:arg));
        if (is_file) emit(LOAD(BPF_DW,7,7,offset("file","f_path")+offset("path","dentry")));
        jump(BPF_JEQ,7,0,allow);
        patterns(deny,writes_only);
        for (int i=0;i<nrules;i++) {
            int next=label();
            if (writes_only && rules[i].policy) jump(BPF_JEQ,8,0,next);
            match(&rules[i],deny,next); mark(next);
        }
    }
    mark(allow); emit(MOVI(0,1)); emit(EXIT);
    mark(deny); emit(MOVI(0,0)); emit(EXIT);
    for (int i=0;i<npatches;i++) {
        int delta=labels[patches[i].label]-patches[i].at-1;
        if (labels[patches[i].label]<0 || delta<0 || delta>32767) abort();
        code[patches[i].at].off=delta;
    }
    char log[1024*1024]={0};
    union bpf_attr a={.prog_type=BPF_PROG_TYPE_LSM,.insn_cnt=count,.insns=(uint64_t)code,
        .license=(uint64_t)"GPL",.log_buf=(uint64_t)log,.log_size=sizeof(log),.log_level=1,
        .expected_attach_type=BPF_LSM_CGROUP,.attach_btf_id=id};
    int fd=bpf_call(BPF_PROG_LOAD,&a);
    if (fd<0) {
        size_t length=strnlen(log,sizeof(log));
        fprintf(stderr,"hook %s: %s (%d instructions; log %zu bytes)\n%s",name,strerror(errno),count,length,log+(length>60000?length-60000:0));
        return -1;
    }
    return fd;
}
static int visited;
static int find_cgroup(const char *path, uint64_t inode, int depth) {
    if (++visited>20000 || depth>32) { errno=E2BIG; return -1; }
    struct stat st;
    if (lstat(path,&st) || !S_ISDIR(st.st_mode)) return -1;
    if (st.st_ino==inode) return open(path,O_RDONLY|O_DIRECTORY|O_NOFOLLOW);
    DIR *dir=opendir(path); if (!dir) return -1;
    struct dirent *e; int found=-1;
    while ((e=readdir(dir))) {
        if (!strcmp(e->d_name,".") || !strcmp(e->d_name,"..")) continue;
        char child[PATH_MAX];
        if (snprintf(child,sizeof(child),"%s/%s",path,e->d_name)>=(int)sizeof(child)) continue;
        found=find_cgroup(child,inode,depth+1); if (found>=0) break;
    }
    closedir(dir); return found;
}
int main(int argc, char **argv) {
    setvbuf(stdout,NULL,_IONBF,0);
    if (argc==3 && !strcmp(argv[1],"--dump-map")) {
        char *end; errno=0; unsigned long id=strtoul(argv[2],&end,10);
        if (errno || !id || id>UINT32_MAX || *end) { errno=EINVAL; fail("map ID"); }
        union bpf_attr a={.map_id=id}; int fd=bpf_call(BPF_MAP_GET_FD_BY_ID,&a);
        if (fd<0) fail("open telemetry");
        struct bpf_map_info info={0};
        a=(union bpf_attr){.info={.bpf_fd=fd,.info_len=sizeof(info),.info=(uint64_t)&info}};
        if (bpf_call(BPF_OBJ_GET_INFO_BY_FD,&a) || info.type!=BPF_MAP_TYPE_HASH ||
            info.key_size!=256 || info.value_size!=sizeof(struct open_record) || info.max_entries!=512) {
            errno=EINVAL; fail("telemetry identity");
        }
        unsigned char key[256], next[256]; int n=0;
        putchar('[');
        for (;;) {
            a=(union bpf_attr){.map_fd=fd,.key=n?(uint64_t)key:0,.next_key=(uint64_t)next};
            if (bpf_call(BPF_MAP_GET_NEXT_KEY,&a)) {
                if (errno==ENOENT) break;
                fail("next telemetry key");
            }
            if (++n>512 || !memchr(next,0,sizeof(next))) { errno=E2BIG; fail("telemetry bounds"); }
            memcpy(key,next,sizeof(key)); struct open_record value={0};
            a=(union bpf_attr){.map_fd=fd,.key=(uint64_t)key,.value=(uint64_t)&value};
            if (bpf_call(BPF_MAP_LOOKUP_ELEM,&a)) fail("read telemetry");
            printf("%s{\"path_hex\":\"",n>1?",":"");
            for (size_t j=0;j<strlen((char *)key);j++) printf("%02x",key[j]);
            printf("\",\"first_ino\":%llu,\"last_ino\":%llu,\"first_links\":%u,\"last_links\":%u,\"opens\":%llu,\"dev\":%u}",
                (unsigned long long)value.first_ino,(unsigned long long)value.last_ino,
                value.first_links,value.last_links,(unsigned long long)value.opens,value.dev);
        }
        puts("]"); close(fd); return 0;
    }
    if (argc==9 && !strcmp(argv[1],"--check-gone")) {
        for (int i=2;i<argc;i++) {
            char *end; errno=0; unsigned long id=strtoul(argv[i],&end,10);
            if (errno || !id || id>UINT32_MAX || *end) { errno=EINVAL; fail("program ID"); }
            int gone=0;
            for (int attempt=0;attempt<20;attempt++) {
                union bpf_attr a={.prog_id=id};
                int fd=bpf_call(i==8?BPF_MAP_GET_FD_BY_ID:BPF_PROG_GET_FD_BY_ID,&a);
                if (fd<0 && errno==ENOENT) { gone=1; break; }
                if (fd<0) fail("observe kernel cleanup");
                close(fd); usleep(100000);
            }
            if (!gone) { fprintf(stderr,"kernel object %lu still exists\n",id); return 2; }
        }
        puts("{\"programs_gone\":6,\"maps_gone\":1}"); return 0;
    }
    if (argc!=3) { fprintf(stderr,"expected two workload cgroup inode IDs\n"); return 2; }
    int groups[2];
    for (int i=0;i<2;i++) {
        char *end; errno=0; uint64_t id=strtoull(argv[i+1],&end,10);
        if (errno || !id || *end) { errno=EINVAL; fail("cgroup ID"); }
        visited=0; groups[i]=find_cgroup("/host-cgroup",id,0);
        if (groups[i]<0) fail("workload cgroup unavailable");
    }
    read_btf();
    dino=offset("dentry","d_inode"); dparent=offset("dentry","d_parent");
    dname=offset("dentry","d_name"); qname=offset("qstr","name");
    ino=offset("inode","i_ino"); sb=offset("inode","i_sb"); dev=offset("super_block","s_dev");
    nlink=offset("inode","i_nlink");
    const char *parents[]={"/fixture","/fixture/nested"};
    for (int i=0;i<2;i++) {
        add_rule(parents[i],".env",0); add_rule(parents[i],"Café.pem",0);
        add_rule(parents[i],"Café",0);
        add_rule(parents[i],"key.pem",0); add_rule(parents[i],".coopignore",1);
        char private[PATH_MAX]; snprintf(private,sizeof(private),"%s/private",parents[i]);
        add_rule(private,"key",0);
    }
    union bpf_attr create={.map_type=BPF_MAP_TYPE_HASH,.key_size=256,.value_size=sizeof(struct open_record),.max_entries=512};
    telemetry=bpf_call(BPF_MAP_CREATE,&create); if (telemetry<0) fail("create telemetry");
    struct bpf_map_info map_info={0};
    union bpf_attr map_attr={.info={.bpf_fd=telemetry,.info_len=sizeof(map_info),.info=(uint64_t)&map_info}};
    if (bpf_call(BPF_OBJ_GET_INFO_BY_FD,&map_attr)) fail("telemetry identity");
    int programs[6];
    programs[0]=load_program("file_open","file",NULL,1,1);
    programs[1]=load_program("file_truncate","file",NULL,1,0);
    programs[2]=load_program("inode_setattr","dentry",NULL,0,0);
    programs[3]=load_program("inode_unlink","dentry",NULL,0,0);
    programs[4]=load_program("inode_rename","old_dentry","new_dentry",0,0);
    programs[5]=load_program("inode_link","old_dentry",NULL,0,0);
    for (int i=0;i<6;i++) if (programs[i]<0) return 2;
    int attached=0;
    for (int i=0;i<12;i++) {
        union bpf_attr a={.target_fd=groups[i/6],.attach_bpf_fd=programs[i%6],
            .attach_type=BPF_LSM_CGROUP,.attach_flags=BPF_F_ALLOW_MULTI};
        if (bpf_call(BPF_PROG_ATTACH,&a)) {
            perror("attach cgroup hook");
            for (int j=0;j<attached;j++) {
                a=(union bpf_attr){.target_fd=groups[j/6],.attach_bpf_fd=programs[j%6],.attach_type=BPF_LSM_CGROUP};
                if (bpf_call(BPF_PROG_DETACH,&a)) perror("partial attachment cleanup");
            }
            return 2;
        }
        attached++;
    }
    printf("{\"attached\":12,\"map_id\":%u,\"program_ids\":[",map_info.id);
    for (int i=0;i<6;i++) {
        struct bpf_prog_info info={0};
        union bpf_attr a={.info={.bpf_fd=programs[i],.info_len=sizeof(info),.info=(uint64_t)&info}};
        if (bpf_call(BPF_OBJ_GET_INFO_BY_FD,&a)) fail("program identity");
        printf("%s%u",i?",":"",info.id); close(programs[i]);
    }
    puts("]}");
    // Direct cgroup attachment owns the references; this loader now exits entirely.
    close(telemetry); close(groups[0]); close(groups[1]); free(btf); return 0;
}
