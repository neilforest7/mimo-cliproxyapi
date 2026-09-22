//go:build !cgo

// A plugin is loaded as a shared library, so the real build needs cgo. Everything except this
// stub stays cgo-free, which keeps plain `go build`, `go test` and editors working; building
// without cgo only produces a binary that cannot be loaded as a plugin, so it says so.
package main

func main() {
	panic("build this plugin with CGO_ENABLED=1 go build -buildmode=c-shared")
}
