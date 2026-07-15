//go:build !cooplivetest

package cli

func prepareACPReload() (func(), error) { return func() {}, nil }
