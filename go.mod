module github.com/colespringer/waxseal

go 1.26

// CI and every release artifact build with exactly this Go, and vulncheck
// scans it. Move it to the newest patch with `go get toolchain@patch`.
toolchain go1.26.10

require github.com/spf13/cobra v1.10.2

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
)
