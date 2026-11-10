package box

import _ "embed"

// The helper is compiled inside the pinned image build, never on the host or
// from a project checkout. Both image definitions hash these exact source bytes.
//
//go:embed supervisor/main.go
var supervisorSource []byte
