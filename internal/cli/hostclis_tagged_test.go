//go:build providere2e || providerlivee2e

package cli

// The tagged suites run the real provider CLIs and runtimes, or their own fixtures, on purpose, so
// they run without the unit tests' host CLI guard (hostclis_test.go).
func guardHostCLIs(tests func() int) int { return tests() }
