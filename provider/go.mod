module github.com/colespringer/waxseal/provider

go 1.26

require (
	// What a consumer outside this checkout resolves, since the replace below
	// is ignored there: keep it at a pushed commit carrying every root member
	// this module uses, and move it to the tag after each root release.
	github.com/colespringer/waxseal v1.3.1-0.20260920185927-956b3f286494
	github.com/colespringer/waxtap/v3 v3.4.0
)

require (
	github.com/colespringer/waxflow v0.0.0-20260921034528-05f3032900a3 // indirect
	github.com/colespringer/waxlabel v1.8.0 // indirect
	github.com/dlclark/regexp2/v2 v2.5.2 // indirect
	github.com/dop251/goja v0.0.0-20260723142020-b4aef50fa347 // indirect
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/colespringer/waxseal => ../
