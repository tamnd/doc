#!/usr/bin/env python3
"""Drive the official PyMongo driver against a running `doc serve`.

This is the Python arm of the wire-protocol compatibility suite (spec 2061 doc 19 appendix G).
It boots a `doc serve` instance on an ephemeral loopback port over an in-memory database,
connects PyMongo to it, runs a CRUD, aggregation, and transaction suite, then shuts the server
down. It exits non-zero if any check fails, so a CI runner can gate on it.

The doc binary is taken from the DOC_BIN environment variable, or built from source with
`go build` if that is not set. PyMongo is the only third-party requirement (pip install pymongo).
"""

import os
import re
import subprocess
import sys
import tempfile
import time

from pymongo import ASCENDING, MongoClient
from pymongo.errors import DuplicateKeyError

LISTEN_RE = re.compile(r"mongodb://([0-9.]+:[0-9]+)")


def build_doc():
    """Return (binary_path, cleanup). Reuse DOC_BIN when set, otherwise build a temp binary."""
    pre = os.environ.get("DOC_BIN")
    if pre:
        return pre, lambda: None
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
    tmpdir = tempfile.mkdtemp(prefix="doc-compat-py")
    binary = os.path.join(tmpdir, "doc")
    subprocess.run(
        ["go", "build", "-o", binary, "github.com/tamnd/doc/cmd/doc"],
        cwd=repo_root,
        check=True,
    )

    def cleanup():
        try:
            os.remove(binary)
            os.rmdir(tmpdir)
        except OSError:
            pass

    return binary, cleanup


def start_serve(binary):
    """Start `doc serve` and return (address, stop). The address is parsed from its stderr."""
    proc = subprocess.Popen(
        [binary, ":memory:", "serve", "--bind", "127.0.0.1", "--port", "0"],
        stderr=subprocess.PIPE,
        text=True,
    )
    addr = None
    deadline = time.time() + 15
    while time.time() < deadline:
        line = proc.stderr.readline()
        if not line:
            break
        match = LISTEN_RE.search(line)
        if match:
            addr = match.group(1)
            break

    def stop():
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()

    if addr is None:
        stop()
        raise RuntimeError("doc serve never announced a listening address")
    return addr, stop


class Suite:
    """A tiny test registry so each check reports pass or fail without pytest."""

    def __init__(self, db):
        self.db = db
        self.failures = []
        self.passed = 0

    def run(self, name, fn):
        coll = self.db[name]
        coll.drop()
        try:
            fn(coll)
            self.passed += 1
            print(f"PASS {name}")
        except Exception as exc:  # noqa: BLE001 - report every failure, keep going
            self.failures.append((name, exc))
            print(f"FAIL {name}: {exc}")


def check(cond, msg):
    if not cond:
        raise AssertionError(msg)


def crud_insert_find(coll):
    res = coll.insert_one({"name": "ada", "age": 36})
    check(res.inserted_id is not None, "insert_one returned no id")
    got = coll.find_one({"name": "ada"})
    check(got is not None and got["age"] == 36, f"find_one returned {got}")


def crud_insert_many_count(coll):
    coll.insert_many([{"n": 1}, {"n": 2}, {"n": 3}])
    check(coll.count_documents({}) == 3, "count after insert_many")
    rows = list(coll.find({"n": {"$gte": 2}}).sort("n", ASCENDING))
    check([r["n"] for r in rows] == [2, 3], f"find($gte:2) sorted = {rows}")


def crud_update_inc(coll):
    coll.insert_one({"k": "a", "hits": 1})
    res = coll.update_one({"k": "a"}, {"$set": {"label": "first"}, "$inc": {"hits": 4}})
    check(res.matched_count == 1 and res.modified_count == 1, "update counts")
    got = coll.find_one({"k": "a"})
    check(got["hits"] == 5 and got["label"] == "first", f"after update = {got}")


def crud_upsert(coll):
    res = coll.update_one({"k": "new"}, {"$set": {"v": 9}}, upsert=True)
    check(res.upserted_id is not None, "upsert produced no id")


def crud_delete(coll):
    coll.insert_many([{"g": "x"}, {"g": "x"}, {"g": "y"}])
    check(coll.delete_one({"g": "x"}).deleted_count == 1, "delete_one")
    check(coll.delete_many({"g": "x"}).deleted_count == 1, "delete_many")
    check(coll.count_documents({}) == 1, "count after deletes")


def agg_group_sum(coll):
    coll.insert_many(
        [
            {"region": "west", "amount": 100},
            {"region": "west", "amount": 50},
            {"region": "east", "amount": 75},
            {"region": "north", "amount": 200},
        ]
    )
    rows = list(
        coll.aggregate(
            [
                {"$group": {"_id": "$region", "total": {"$sum": "$amount"}}},
                {"$sort": {"_id": 1}},
            ]
        )
    )
    got = {r["_id"]: r["total"] for r in rows}
    check(got == {"east": 75, "north": 200, "west": 150}, f"group sum = {got}")


def agg_match_project_limit(coll):
    coll.insert_many([{"amount": a} for a in (100, 50, 75, 25, 200)])
    rows = list(
        coll.aggregate(
            [
                {"$match": {"amount": {"$gte": 75}}},
                {"$sort": {"amount": -1}},
                {"$project": {"_id": 0, "amount": 1}},
                {"$limit": 2},
            ]
        )
    )
    check([r["amount"] for r in rows] == [200, 100], f"pipeline rows = {rows}")
    check("_id" not in rows[0], "$project _id:0 did not drop _id")


def index_unique(coll):
    coll.create_index([("sku", ASCENDING)], unique=True)
    coll.insert_one({"sku": "A1"})
    try:
        coll.insert_one({"sku": "A1"})
    except DuplicateKeyError:
        return
    raise AssertionError("duplicate insert under unique index did not raise")


def index_list_drop(coll):
    name = coll.create_index([("email", ASCENDING)])
    names = [ix["name"] for ix in coll.list_indexes()]
    check(name in names, f"created index {name} not listed in {names}")
    coll.drop_index(name)
    names = [ix["name"] for ix in coll.list_indexes()]
    check(name not in names, f"index {name} still present after drop")


def txn_commit(coll, client):
    with client.start_session() as session:
        with session.start_transaction():
            coll.insert_one({"n": 1}, session=session)
            coll.insert_one({"n": 2}, session=session)
            inside = coll.count_documents({}, session=session)
            check(inside == 2, f"read-your-writes inside txn = {inside}")
    check(coll.count_documents({}) == 2, "count after commit")


def txn_abort(coll, client):
    with client.start_session() as session:
        session.start_transaction()
        coll.insert_one({"n": 99}, session=session)
        session.abort_transaction()
    check(coll.count_documents({}) == 0, "count after abort should be 0")


def main():
    binary, cleanup_bin = build_doc()
    try:
        addr, stop = start_serve(binary)
    except Exception as exc:  # noqa: BLE001
        cleanup_bin()
        print(f"could not start doc serve: {exc}", file=sys.stderr)
        return 1

    try:
        client = MongoClient(f"mongodb://{addr}/?directConnection=true", serverSelectionTimeoutMS=10000)
        client.admin.command("ping")
        db = client["compat"]
        suite = Suite(db)

        suite.run("crud_insert_find", crud_insert_find)
        suite.run("crud_insert_many_count", crud_insert_many_count)
        suite.run("crud_update_inc", crud_update_inc)
        suite.run("crud_upsert", crud_upsert)
        suite.run("crud_delete", crud_delete)
        suite.run("agg_group_sum", agg_group_sum)
        suite.run("agg_match_project_limit", agg_match_project_limit)
        suite.run("index_unique", index_unique)
        suite.run("index_list_drop", index_list_drop)
        suite.run("txn_commit", lambda c: txn_commit(c, client))
        suite.run("txn_abort", lambda c: txn_abort(c, client))

        print(f"\n{suite.passed} passed, {len(suite.failures)} failed")
        return 1 if suite.failures else 0
    finally:
        stop()
        cleanup_bin()


if __name__ == "__main__":
    sys.exit(main())
