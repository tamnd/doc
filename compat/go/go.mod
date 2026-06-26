// This module is deliberately separate from the root github.com/tamnd/doc module so the core
// stays free of external dependencies. It exists only to drive the official MongoDB Go driver
// against a running `doc serve` and prove wire-protocol compatibility.
module github.com/tamnd/doc/compat/go

go 1.26

require go.mongodb.org/mongo-driver/v2 v2.7.0

require (
	github.com/klauspost/compress v1.17.6 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.2.0 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	golang.org/x/crypto v0.33.0 // indirect
	golang.org/x/sync v0.11.0 // indirect
	golang.org/x/text v0.22.0 // indirect
)
