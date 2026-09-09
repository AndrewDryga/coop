//go:build !linux

package networkgateway

func OpenBootClock() (*BootClock, error) { return nil, Failure("clock_unsupported") }
