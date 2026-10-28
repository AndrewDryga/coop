package runtime

import (
	"context"
	"os"
	"testing"
)

func TestDockerNetworksRejectsPartialOutputFromFailedCommand(t *testing.T) {
	for _, phase := range []string{"list", "inspect"} {
		t.Run(phase, func(t *testing.T) {
			rt, _ := fixtureDocker(t, dockerFixture{})
			script := `#!/bin/sh
case "$*" in
 *"network ls"*) printf 'network-id\n'; `
			if phase == "list" {
				script += "exit 7 ;;\n"
			} else {
				script += "exit 0 ;;\n"
			}
			script += ` *"network inspect"*) printf '{"name":"bridge","internal":false,"ipam":[{"Subnet":"172.18.0.0/16","Gateway":"172.18.0.1"}]}\n'; exit 7 ;;
esac
exit 9
`
			if err := os.WriteFile(rt.Name, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			docker, err := BindDocker(context.Background(), rt, fixtureSock(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer docker.Close()
			networks, err := docker.Networks(context.Background())
			if err == nil || networks != nil {
				t.Fatalf("failed %s returned usable partial inventory: %+v %v", phase, networks, err)
			}
		})
	}
}
