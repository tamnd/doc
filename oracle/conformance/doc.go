// Package conformance runs doc's behavior oracle against a live MongoDB server.
//
// It is a separate Go module so the doc module itself never depends on the
// MongoDB driver: the driver and this test live here, behind the `mongo` build
// tag, and are pulled in only when the conformance suite is run. The suite drives
// the shared oracle.Corpus against both a real MongoDB (the reference) and the
// doc engine (the subject) and fails on any behavioral diff.
//
// Run it against a server (spec 2061 doc 19 §17):
//
//	cd oracle/conformance
//	MONGO_URL=mongodb://localhost:27017 go test -tags mongo ./...
package conformance
