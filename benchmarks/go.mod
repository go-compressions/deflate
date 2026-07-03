// Module for the deflate benchmark harness. It is a SEPARATE module from
// github.com/go-compressions/deflate so `go test ./...` and the coverage gate at
// the repo root never descend into it — the harness is a measurement tool, not
// part of the library's tested surface.
module github.com/go-compressions/deflate/benchmarks

go 1.26.4

require github.com/go-compressions/deflate v0.0.0

require github.com/go-compressions/matchlen v0.1.1 // indirect

replace github.com/go-compressions/deflate => ../
