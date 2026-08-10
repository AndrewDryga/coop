//go:build !cooplivetest

package acpctl

func PrepareACPReload() (func(), error) { return func() {}, nil }
