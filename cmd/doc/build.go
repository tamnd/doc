package main

import "github.com/tamnd/doc"

// Build identity. These default to the empty string and are overwritten at release
// time by the linker (-X main.version=..., -X main.commit=..., -X main.date=...).
// A plain `go build` or `go install` leaves them empty, in which case the binary
// falls back to the doc.Version constant baked into the library.
var (
	version = ""
	commit  = ""
	date    = ""
)

// buildVersion returns the version string the CLI reports: the linker-stamped tag
// when present, otherwise the library constant.
func buildVersion() string {
	if version != "" {
		return version
	}
	return doc.Version
}

// buildDetails returns the commit and build date when the linker stamped them, for
// the longer `doc info` and `doc version --verbose` style output. Empty strings
// mean a non-release build.
func buildDetails() (string, string) {
	return commit, date
}
