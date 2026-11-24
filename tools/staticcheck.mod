module github.com/AndrewDryga/coop/tools/staticcheck

go 1.27

// This alternate module pins the gate's analyzer and the Go importer it reads
// export data with (make install-staticcheck, and this repository's
// .agent/Dockerfile). It does not affect Coop's dependencies.
require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20261124000051-d6646c509b6d // indirect
	golang.org/x/mod v0.43.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/tools v0.53.0 // indirect
	honnef.co/go/tools v0.8.2 // indirect
)

tool honnef.co/go/tools/cmd/staticcheck
