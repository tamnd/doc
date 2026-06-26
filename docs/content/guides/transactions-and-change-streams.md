---
title: "Transactions and change streams"
description: "Run multi-document transactions under snapshot or serializable isolation with a session, and tail changes to a collection or database with a change stream."
weight: 30
---

Most writes touch one document and need no extra ceremony.
A single insert or update is already atomic on its own.
You reach for a transaction when one logical change spans more than one document or more than one collection and the whole thing has to either land or not land at all.

The classic case is moving money between two accounts.
You debit one document and credit another, and there must never be a moment where the debit happened but the credit did not.
That is what a transaction buys you: a set of writes that commit together or roll back together.

## Sessions

A transaction runs inside a session.
You open a session, run your work, and close it when you are done.

```go
ctx := context.Background()

sess, err := db.StartSession()
if err != nil {
    return err
}
defer sess.EndSession(ctx)
```

The session carries the transaction state.
Every collection operation that should be part of the transaction has to run with the session's context, not the bare background context.
The helper below handles that wiring for you.

## WithTransaction

`WithTransaction` is the form you want most of the time.
You hand it a function, and it runs that function inside a transaction, commits on success, and aborts if the function returns an error.

It also retries.
doc uses first-committer-wins conflict detection, so if a concurrent transaction commits a conflicting write first, your commit fails and the whole function runs again on a fresh snapshot.
You do not write the retry loop yourself.
`WithTransaction` reruns the callback until it commits or hits an error that is not a conflict.

Here is the account transfer.

```go
ctx := context.Background()

sess, err := db.StartSession()
if err != nil {
    return err
}
defer sess.EndSession(ctx)

accounts := db.Database("bank").Collection("accounts")

_, err = sess.WithTransaction(ctx, func(sessCtx context.Context) (any, error) {
    // Debit the source account.
    _, err := accounts.UpdateOne(sessCtx,
        doc.D{{"_id", "alice"}},
        doc.D{{"$inc", doc.D{{"balance", -100}}}},
    )
    if err != nil {
        return nil, err
    }

    // Credit the destination account.
    _, err = accounts.UpdateOne(sessCtx,
        doc.D{{"_id", "bob"}},
        doc.D{{"$inc", doc.D{{"balance", 100}}}},
    )
    if err != nil {
        return nil, err
    }

    return nil, nil
})
if err != nil {
    return err
}
```

Both updates use `sessCtx`, the context passed into the callback.
That is what ties them to the transaction.
If the callback returns an error, neither update is visible to anyone else; the snapshot the work ran against is thrown away.

Keep the callback idempotent in spirit, because it may run more than once on conflict.
The transfer above is fine: each retry starts from the current committed state and applies the same delta, so a rerun is correct.

## The manual form

If you need finer control over when the transaction starts and ends, drive it by hand.

```go
ctx := context.Background()

sess, err := db.StartSession()
if err != nil {
    return err
}
defer sess.EndSession(ctx)

if err := sess.StartTransaction(); err != nil {
    return err
}

accounts := db.Database("bank").Collection("accounts")
sessCtx := doc.NewSessionContext(ctx, sess) // bind the ops to the session

_, err = accounts.UpdateOne(sessCtx,
    doc.D{{"_id", "alice"}},
    doc.D{{"$inc", doc.D{{"balance", -100}}}},
)
if err != nil {
    sess.AbortTransaction(ctx)
    return err
}

_, err = accounts.UpdateOne(sessCtx,
    doc.D{{"_id", "bob"}},
    doc.D{{"$inc", doc.D{{"balance", 100}}}},
)
if err != nil {
    sess.AbortTransaction(ctx)
    return err
}

if err := sess.CommitTransaction(ctx); err != nil {
    // On a conflict the commit fails here. You own the retry in the manual form.
    return err
}
```

The trade-off is plain.
The manual form gives you the commit and abort calls directly, but the retry on conflict is now yours to write.
That is the main reason `WithTransaction` exists.

## Snapshot or serializable

doc runs snapshot isolation by default.
Underneath is an MVCC core: each document has a version chain, a watermark oracle decides what a transaction can see, the commit path applies first-committer-wins conflict detection, and a background collector reclaims old versions.
The property that follows from this is the one you want: a reader never blocks a writer and a writer never blocks a reader.

Under snapshot isolation each transaction reads from a consistent point-in-time view of the database.
Nothing another transaction commits after your snapshot starts is visible to you.
On commit, if any document you wrote was also written by a transaction that committed after your snapshot, the first committer wins and you lose, so your commit fails and you retry.
This stops lost updates and dirty reads.

Snapshot isolation does not stop write-skew.
Write-skew happens when two transactions each read an overlapping set of rows, then each writes a different row based on what it read, and the two writes together break an invariant that neither write broke on its own.

The standard example is an on-call rule that says at least one doctor must stay on call.
Two doctors are on call.
Both transactions read the count, both see two on call, both conclude it is safe to go off call, and both write their own row off call.
Under snapshot isolation neither write conflicts with the other, since they touch different rows, so both commit and now zero doctors are on call.

Serializable isolation closes that gap.
doc's serializable mode is Serializable Snapshot Isolation (SSI).
It keeps the same snapshot reads but adds read/write dependency tracking, so it detects the dangerous read/write cycle behind write-skew and aborts one of the two transactions before it can commit.
The cost is a higher abort rate under contention, because some transactions that would have committed under snapshot now get rejected and have to retry.

Pick snapshot when your transactions write what they read and you mainly need point-in-time consistency, which is the common case.
Pick serializable when correctness depends on a check-then-act over rows you do not write, like the on-call rule, a unique-constraint scan, or a balance-spanning invariant across documents.

### Setting the default

The default isolation level is controlled by a pragma, `default_isolation`, whose values are `snapshot` (the default) and `serializable`.

Set it from Go:

```go
db.Pragma("default_isolation", "serializable")
```

Or from the shell:

```
.pragma default_isolation=serializable
```

After that, transactions opened on the database use serializable isolation unless you change it back.

## Interactive transactions in the shell

The shell has an explicit transaction flow for poking at data by hand.

```
> .begin
[session] > db.accounts.updateOne({_id: "alice"}, {$inc: {balance: -100}})
[session] > db.accounts.updateOne({_id: "bob"}, {$inc: {balance: 100}})
[session] > .commit
```

`.begin` opens a transaction and the prompt changes to show `[session]` so you know you are inside one.
Run as many statements as you need.
`.commit` lands them all together.
If you change your mind, `.rollback` throws the whole thing away and leaves the database as it was before `.begin`.

## Change streams

A change stream lets you tail the writes happening on a collection or the whole database.
You open the stream, then iterate as events arrive: inserts, updates, replaces, and deletes.

Open one on a collection with `Watch`.
The pipeline is a `doc.A` of aggregation stages.
An empty `doc.A{}` means you want every change.

```go
ctx := context.Background()

c := db.Database("bank").Collection("accounts")

stream, err := c.Watch(ctx, doc.A{})
if err != nil {
    return err
}
defer stream.Close(ctx)

for stream.Next(ctx) {
    var event doc.M
    if err := stream.Decode(&event); err != nil {
        return err
    }

    fmt.Println(event["operationType"], event["documentKey"])
}
```

`stream.Next` blocks until the next event is ready, then `stream.Decode` unmarshals it.
Each event carries an `operationType` (`insert`, `update`, `replace`, or `delete`), a `documentKey` with the `_id` of the affected document, and either the full document (on insert and replace) or an update description (on update).
Before the first event has arrived, the read-without-blocking path (`TryNext`, and `Decode` when there is nothing to decode) reports `doc.ErrNoDocuments`, so treat that as "nothing yet" rather than a failure.

### Filtering with a pipeline

You usually do not care about every change.
Push a `$match` into the pipeline so the stream only delivers the events you want.
This filters deletes out and keeps inserts and updates:

```go
pipeline := doc.A{
    doc.D{{"$match", doc.D{
        {"operationType", doc.D{{"$in", doc.A{"insert", "update"}}}},
    }}},
}

stream, err := c.Watch(ctx, pipeline)
if err != nil {
    return err
}
defer stream.Close(ctx)
```

The match runs against each change event, so you can filter on `operationType`, on fields of the changed document, or on the document key.

### Watching the whole database

`db.Watch` opens a stream over every collection at once.
The events look the same, with one extra field telling you which collection each change came from.

```go
ctx := context.Background()

stream, err := db.Watch(ctx, doc.A{})
if err != nil {
    return err
}
defer stream.Close(ctx)

for stream.Next(ctx) {
    var event doc.M
    if err := stream.Decode(&event); err != nil {
        return err
    }

    fmt.Println(event["ns"], event["operationType"])
}
```

This is handy for cache invalidation, audit logs, or fanning changes out to subscribers without watching each collection by hand.

## Next

The same transactions and change streams are available over the wire when you run doc as a server.
See [The wire server](/guides/the-wire-server/) for that, and [Operations](/guides/operations/) for the full catalog of collection methods you can run inside a transaction or watch with a stream.

