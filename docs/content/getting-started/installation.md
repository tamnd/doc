---
title: "Installation"
description: "Install the doc binary from Go, Homebrew, Scoop, a Linux package, or the container image, or add the doc library to a Go module."
weight: 20
---

doc ships as a single binary, `doc`, and as a Go library, `github.com/tamnd/doc`.
Pick whichever channel suits you.
If you only want to use the engine from Go code, skip to [as a Go library](#as-a-go-library).

## Go

```bash
go install github.com/tamnd/doc/cmd/doc@latest
```

This puts `doc` in `$(go env GOPATH)/bin`.
The core is pure Go, so a `CGO_ENABLED=0` install works and cross-compiles like any other Go binary.

## Homebrew (macOS and Linux)

```bash
brew install tamnd/tap/doc
```

The formula installs the prebuilt `doc` binary.

## Scoop (Windows)

```bash
scoop bucket add tamnd https://github.com/tamnd/scoop-bucket
scoop install doc
```

## Linux (apt and dnf)

A signed apt and dnf repository tracks every release, so `apt upgrade` and `dnf upgrade` keep `doc` current.

```bash
# Debian, Ubuntu
curl -fsSL https://tamnd.github.io/linux-repo/gpg.key \
  | sudo gpg --dearmor -o /usr/share/keyrings/tamnd.gpg
echo "deb [signed-by=/usr/share/keyrings/tamnd.gpg] https://tamnd.github.io/linux-repo/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/tamnd.list
sudo apt update && sudo apt install doc

# Fedora, RHEL
sudo dnf config-manager --add-repo https://tamnd.github.io/linux-repo/dnf/tamnd.repo
sudo dnf install doc
```

## Release archives and Linux packages

Every release attaches prebuilt archives (`tar.gz`, and `zip` on Windows) and Linux packages (`deb`, `rpm`, `apk`) for Linux, macOS, Windows, and FreeBSD on amd64 and arm64.
Download them from the [releases page](https://github.com/tamnd/doc/releases), verify the `checksums.txt` (it is signed with a keyless cosign signature), and put the `doc` binary on your `PATH`.

## Container image

A multi-arch image is published to GitHub Container Registry:

```bash
docker run --rm -v "$PWD:/data" ghcr.io/tamnd/doc app.doc --eval 'db.users.count()'
```

The `/data` volume is the working directory inside the image, so a `.doc` file in the mounted directory is reachable by name.
To serve the wire protocol, publish the port:

```bash
docker run --rm -p 27017:27017 -v "$PWD:/data" ghcr.io/tamnd/doc app.doc serve --bind 0.0.0.0
```

## As a Go library

Add the module to your project:

```bash
go get github.com/tamnd/doc@latest
```

Then open a database and use it:

```go
import "github.com/tamnd/doc"

db, err := doc.Open("app.doc")
if err != nil {
	log.Fatal(err)
}
defer db.Close()
```

The core module has no third-party dependencies, so it does not pull anything else into your build graph.

## Verify the install

```bash
doc version
```

This prints the version, and on a release build the commit and build date.

## What is next

Run your first query on the [quick start](/getting-started/quick-start/).
