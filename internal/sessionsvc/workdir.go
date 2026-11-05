package sessionsvc

// StableWorkdir is the in-box cwd of every remote session: one fixed name, so a native history
// never depends on a host path or a fork's name.
const StableWorkdir = "/workspace"
