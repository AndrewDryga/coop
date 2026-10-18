package workerproto

import "bytes"

// ValidateWorkspaceCheckpointBundle authenticates every tar member against the external
// descriptor and inner manifest before a caller uploads or restores any bytes.
func ValidateWorkspaceCheckpointBundle(
	checkpoint WorkspaceCheckpoint,
	bundle []byte,
) (WorkspaceCheckpointBundleManifest, error) {
	return ReadWorkspaceCheckpointBundle(checkpoint, bytes.NewReader(bundle), nil)
}
