---
title: "Stability"
description: "What pre-1.0 means for the library API, the PRAGMA catalogue, and the on-disk file format."
weight: 40
---

doc is pre-1.0.
The engine is complete and the test suite is thorough, but the project has not committed to a frozen public surface yet.
This page is honest about what you can lean on today and what may still move before 1.0.

## The library API is still settling

The exported Go API works and is exercised by the whole test suite, but it is not frozen.
While doc is on a 0.x version, a minor release may rename a type, change a method signature, or move a helper, if doing so makes the 1.0 surface cleaner.

In practice the shape is already close to the MongoDB Go driver and is unlikely to churn much.
The risk is real but small, and the way to insulate yourself from it is the usual one: pin a version in your `go.mod` and read the release notes before you bump.
When the API is declared stable, that is 1.0, and from then on semantic versioning applies in full: no exported name changes meaning or signature within the 1.x line.

## The PRAGMA catalogue

The PRAGMA names, their accepted values, and their scopes (runtime, open-time, create-time, or fixed) are listed on the [configuration](/reference/configuration/) page.
A name that is not in the catalogue is rejected, so a typo or a knob that does not exist surfaces as an error rather than a silent no-op.
That rejection behaviour is deliberate and is not going to change.

The catalogue itself may still grow or be adjusted before 1.0, in step with the library API.

## The file format

The on-disk format is the most conservative part of the project, because it is the part you cannot re-run.
The format is at major version 1, minor version 0.

The compatibility rule is explicit in the header.
A file records the format major and minor version.
A build opens a file whose major version it understands, and rejects a file whose major version is newer than the build with a clear error, instead of misreading it.
A later release may bump the minor version to record a new optional feature, and an older build still opens the file, ignoring what it does not know about.

The aim is that a file you write today keeps opening in later doc builds.
If a change ever has to break that, it raises the format major version and doc ships a migration path rather than silently changing the bytes.

## What is explicitly not covered

None of the internals are a stable surface, at any version.
The exact bytes of the WAL, the page-cache eviction policy, the planner's cost numbers, the columnar segment encodings, and anything in an unexported package may change in any release, because none of it is something you write code against.
The CLI's human-readable output (table formatting, banners) may also be refined; scripts should parse the JSON output modes rather than the pretty output.
