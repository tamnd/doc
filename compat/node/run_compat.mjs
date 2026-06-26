// Drive the official Node.js mongodb driver against a running `doc serve`.
//
// This is the Node arm of the wire-protocol compatibility suite (spec 2061 doc 19 appendix G).
// It boots a `doc serve` instance on an ephemeral loopback port over an in-memory database,
// connects the driver to it, runs a CRUD and aggregation suite, then shuts the server down. It
// exits non-zero when any check fails so a CI runner can gate on it.
//
// The doc binary comes from the DOC_BIN environment variable, or is built with `go build` when
// that is unset. The only third-party requirement is the mongodb package (npm install mongodb).

import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { MongoClient } from "mongodb";

const here = dirname(fileURLToPath(import.meta.url));
const listenRE = /mongodb:\/\/([0-9.]+:[0-9]+)/;

function buildDoc() {
  const pre = process.env.DOC_BIN;
  if (pre) return { bin: pre, cleanup: () => {} };
  const repoRoot = resolve(here, "..", "..");
  const dir = mkdtempSync(join(tmpdir(), "doc-compat-node"));
  const bin = join(dir, "doc");
  execFileSync("go", ["build", "-o", bin, "github.com/tamnd/doc/cmd/doc"], {
    cwd: repoRoot,
    stdio: ["ignore", "ignore", "inherit"],
  });
  return { bin, cleanup: () => rmSync(dir, { recursive: true, force: true }) };
}

function startServe(bin) {
  const proc = spawn(bin, [":memory:", "serve", "--bind", "127.0.0.1", "--port", "0"]);
  return new Promise((resolvePromise, rejectPromise) => {
    let settled = false;
    let buf = "";
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        proc.kill();
        rejectPromise(new Error("doc serve never announced a listening address"));
      }
    }, 15000);

    proc.stderr.on("data", (chunk) => {
      buf += chunk.toString();
      const m = buf.match(listenRE);
      if (m && !settled) {
        settled = true;
        clearTimeout(timer);
        const stop = () =>
          new Promise((done) => {
            proc.on("exit", () => done());
            proc.kill("SIGINT");
            setTimeout(() => proc.kill("SIGKILL"), 10000);
          });
        resolvePromise({ addr: m[1], stop });
      }
    });
  });
}

const failures = [];
let passed = 0;

async function run(db, name, fn) {
  const coll = db.collection(name);
  try {
    await coll.drop().catch(() => {});
    await fn(coll);
    passed += 1;
    console.log(`PASS ${name}`);
  } catch (err) {
    failures.push([name, err]);
    console.log(`FAIL ${name}: ${err.message}`);
  }
}

function check(cond, msg) {
  if (!cond) throw new Error(msg);
}

async function crudInsertFind(coll) {
  const res = await coll.insertOne({ name: "ada", age: 36 });
  check(res.insertedId != null, "insertOne returned no id");
  const got = await coll.findOne({ name: "ada" });
  check(got && got.age === 36, `findOne returned ${JSON.stringify(got)}`);
}

async function crudInsertManyCount(coll) {
  await coll.insertMany([{ n: 1 }, { n: 2 }, { n: 3 }]);
  check((await coll.countDocuments({})) === 3, "count after insertMany");
  const rows = await coll.find({ n: { $gte: 2 } }).sort({ n: 1 }).toArray();
  check(JSON.stringify(rows.map((r) => r.n)) === "[2,3]", `find sorted = ${JSON.stringify(rows)}`);
}

async function crudUpdateInc(coll) {
  await coll.insertOne({ k: "a", hits: 1 });
  const res = await coll.updateOne({ k: "a" }, { $set: { label: "first" }, $inc: { hits: 4 } });
  check(res.matchedCount === 1 && res.modifiedCount === 1, "update counts");
  const got = await coll.findOne({ k: "a" });
  check(got.hits === 5 && got.label === "first", `after update = ${JSON.stringify(got)}`);
}

async function crudUpsert(coll) {
  const res = await coll.updateOne({ k: "new" }, { $set: { v: 9 } }, { upsert: true });
  check(res.upsertedId != null, "upsert produced no id");
}

async function crudDelete(coll) {
  await coll.insertMany([{ g: "x" }, { g: "x" }, { g: "y" }]);
  check((await coll.deleteOne({ g: "x" })).deletedCount === 1, "deleteOne");
  check((await coll.deleteMany({ g: "x" })).deletedCount === 1, "deleteMany");
  check((await coll.countDocuments({})) === 1, "count after deletes");
}

async function aggGroupSum(coll) {
  await coll.insertMany([
    { region: "west", amount: 100 },
    { region: "west", amount: 50 },
    { region: "east", amount: 75 },
    { region: "north", amount: 200 },
  ]);
  const rows = await coll
    .aggregate([
      { $group: { _id: "$region", total: { $sum: "$amount" } } },
      { $sort: { _id: 1 } },
    ])
    .toArray();
  const got = Object.fromEntries(rows.map((r) => [r._id, r.total]));
  check(
    JSON.stringify(got) === JSON.stringify({ east: 75, north: 200, west: 150 }),
    `group sum = ${JSON.stringify(got)}`,
  );
}

async function aggMatchProjectLimit(coll) {
  await coll.insertMany([100, 50, 75, 25, 200].map((amount) => ({ amount })));
  const rows = await coll
    .aggregate([
      { $match: { amount: { $gte: 75 } } },
      { $sort: { amount: -1 } },
      { $project: { _id: 0, amount: 1 } },
      { $limit: 2 },
    ])
    .toArray();
  check(JSON.stringify(rows.map((r) => r.amount)) === "[200,100]", `pipeline rows = ${JSON.stringify(rows)}`);
  check(!("_id" in rows[0]), "$project _id:0 did not drop _id");
}

async function indexUnique(coll) {
  await coll.createIndex({ sku: 1 }, { unique: true });
  await coll.insertOne({ sku: "A1" });
  try {
    await coll.insertOne({ sku: "A1" });
  } catch (err) {
    check(err.code === 11000, `duplicate insert error code = ${err.code}, want 11000`);
    return;
  }
  throw new Error("duplicate insert under unique index did not raise");
}

async function indexListDrop(coll) {
  const name = await coll.createIndex({ email: 1 });
  let names = (await coll.listIndexes().toArray()).map((ix) => ix.name);
  check(names.includes(name), `created index ${name} not in ${JSON.stringify(names)}`);
  await coll.dropIndex(name);
  names = (await coll.listIndexes().toArray()).map((ix) => ix.name);
  check(!names.includes(name), `index ${name} still present after drop`);
}

async function main() {
  const { bin, cleanup } = buildDoc();
  let server;
  try {
    server = await startServe(bin);
  } catch (err) {
    cleanup();
    console.error(`could not start doc serve: ${err.message}`);
    process.exit(1);
  }

  const client = new MongoClient(`mongodb://${server.addr}/?directConnection=true`, {
    serverSelectionTimeoutMS: 10000,
  });
  try {
    await client.connect();
    await client.db("admin").command({ ping: 1 });
    const db = client.db("compat");

    await run(db, "crud_insert_find", crudInsertFind);
    await run(db, "crud_insert_many_count", crudInsertManyCount);
    await run(db, "crud_update_inc", crudUpdateInc);
    await run(db, "crud_upsert", crudUpsert);
    await run(db, "crud_delete", crudDelete);
    await run(db, "agg_group_sum", aggGroupSum);
    await run(db, "agg_match_project_limit", aggMatchProjectLimit);
    await run(db, "index_unique", indexUnique);
    await run(db, "index_list_drop", indexListDrop);

    console.log(`\n${passed} passed, ${failures.length} failed`);
  } finally {
    await client.close();
    await server.stop();
    cleanup();
  }
  process.exit(failures.length ? 1 : 0);
}

main();
