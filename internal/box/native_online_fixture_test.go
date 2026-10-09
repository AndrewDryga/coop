package box

import (
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/nativedocker"
	"testing"
)

func nativeOnlineRuntime(t *testing.T, calls, boxEnv string) runtime.Runtime {
	return nativedocker.New(t, calls, boxEnv)
}
func TestNativeOnlineFixtureProcess(t *testing.T) { nativedocker.Process(t) }
