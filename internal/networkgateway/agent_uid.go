package networkgateway

// The gateway service runs as UID 65532 and has different egress authority.
// An agent with that UID could use its kernel rules and private IPC.
func validAgentUID(uid uint32) bool {
	return uid > 0 && uid != 65532 && uid <= 1<<31-1
}
