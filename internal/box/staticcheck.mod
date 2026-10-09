module github.com/AndrewDryga/coop/internal/box/staticcheck

go 1.27

// Staticcheck's published dependency predates Go 1.27.2's V5 export format.
// Keep the analyzer unchanged and use the compatible upstream importer. This
// alternate module is embedded in images and does not affect Coop's dependencies.
require (
	github.com/BurntSushi/toml v1.4.1-0.20240526193622-a339e1f7089c // indirect
	golang.org/x/exp/typeparams v0.0.0-20231108232855-2478ac86f678 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/tools v0.50.0 // indirect
	honnef.co/go/tools v0.8.1 // indirect
)

tool honnef.co/go/tools/cmd/staticcheck
