# doc is a pure-Go module (CGO_ENABLED=0). There is no binary yet: the milestones
# build the storage engine first, so the targets operate over the whole module.
export CGO_ENABLED := 0

.PHONY: build test race bench vet fmt fmtcheck lint tidy cover clean

build:
	go build ./...

# Default test run with the race detector, matching CI.
test race:
	go test -race -count=1 ./...

# Run every benchmark once: a smoke check that the bench code compiles and runs,
# not a performance gate.
bench:
	go test -run '^$$' -bench . -benchtime 1x ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

fmtcheck:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

# Quick local quality gate. golangci-lint runs in CI; this is the fast subset.
lint: fmtcheck vet

tidy:
	go mod tidy

cover:
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

clean:
	rm -f coverage.out
	go clean ./...
