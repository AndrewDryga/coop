package networkgateway

// Identity binds private IPC in both directions to one frozen execution. UID
// authenticates the service role; it cannot identify a stale/swapped run volume.
type Identity struct {
	Clock             ClockDomain `json:"clock"`
	RunID             string      `json:"run_id"`
	Epoch             string      `json:"gateway_epoch"`
	PolicyFingerprint string      `json:"policy_fingerprint"`
}

func (i Identity) Valid() bool {
	return i.Clock.Valid() && lowerHex(i.RunID, 32) && lowerHex(i.Epoch, 32) && lowerHex(i.PolicyFingerprint, 64)
}

func lowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
