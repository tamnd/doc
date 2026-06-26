---
title: "Stability"
description: "What is frozen at v1: the library API, the PRAGMA catalogue, and the on-disk file format."
weight: 40
---

doc is at v1.
This page states what that means in practice, so you can depend on it.

## The library API is frozen at v1

The exported Go API follows semantic versioning.
Within the v1 line, no exported name changes its meaning or its signature.
That covers `doc.Open` and its options, the `DB`, `Database`, and `Collection` types and their methods, the result types, the cursor and single-result types, the session and change-stream surface, the `options` package builders, and the exported error values.

New functionality arrives as new exported names, never as a changed one.
A v1.x release may add a method or an option; it will not rename or remove one, or change what an existing one does.
If something must change incompatibly, that is a v2, with its own import path.

## The PRAGMA catalogue is stable

The PRAGMA names, their accepted values, and their scopes (runtime, open-time, create-time, or fixed) are stable across v1.
The full list is on the [configuration](/reference/configuration/) page.
A name that is not in the catalogue is rejected, so a typo or a knob that does not exist surfaces as an error rather than a silent no-op, and that behaviour is part of the contract.

New PRAGMAs may be added in a v1.x release.
Existing ones keep their names, values, and meanings.

## The file format is stable

The on-disk format is at major version 1.
Every v1 build reads every file any other v1 build wrote.

The compatibility rule is explicit in the header.
A file records the format major and minor version.
A build opens a file whose major version it understands, and rejects a file whose major version is newer than the build with a clear error, instead of misreading it.
A later v1.x release may bump the minor version to record a new optional feature, and an older v1 build still opens the file, ignoring what it does not know about.

This means an upgrade is safe in both directions within v1: a newer doc opens an older file, and an older doc opens a newer v1 file.
A format change that would break that is a v2 format, and doc would offer a migration path rather than silently change the bytes.

## What is not covered

Stability applies to the public surface above, not to internals.
The exact bytes of the WAL, the page-cache eviction policy, the planner's cost numbers, the columnar segment encodings, and anything in an unexported package may change in a v1.x release, because none of it is something you write code against.
The CLI's human-readable output (table formatting, banners) may also be refined; scripts should parse the JSON output modes, which are stable, rather than the pretty output.
