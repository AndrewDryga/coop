module github.com/AndrewDryga/coop/tools/staticcheck

go 1.27

// This alternate module pins the gate's analyzer and the Go importer it reads
// export data with (make install-staticcheck, and this repository's
// .agent/Dockerfile). It does not affect Coop's dependencies.
require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/tools v0.51.0 // indirect
	honnef.co/go/tools v0.8.2 // indirect
)

tool honnef.co/go/tools/cmd/staticcheck
