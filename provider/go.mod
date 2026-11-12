module github.com/colespringer/waxseal/provider

go 1.26.0

require (
	// What a consumer outside this checkout resolves, since the replace below
	// is ignored there: keep it at a pushed commit carrying every root member
	// this module uses, and move it to the tag after each root release.
	github.com/colespringer/waxseal v1.6.0
	github.com/colespringer/waxtap/v3 v3.7.0
	google.golang.org/protobuf v1.36.12 // the e2e suite decodes xtags with it
)

require (
	github.com/colespringer/waxflow v0.0.0-20261111201239-c9d965942277 // indirect
	github.com/colespringer/waxlabel v1.8.0 // indirect
	github.com/dlclark/regexp2/v2 v2.12.0 // indirect
	github.com/dop251/goja v0.0.0-20261103214539-f5d1d0025744 // indirect
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible // indirect
	github.com/google/pprof v0.0.0-20261103193413-97e8293de3c4 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/colespringer/waxseal => ../
