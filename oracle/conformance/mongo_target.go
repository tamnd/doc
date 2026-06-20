//go:build mongo

package conformance

import (
	"context"
	"errors"
	"fmt"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/oracle"

	mbson "go.mongodb.org/mongo-driver/v2/bson"
	mdriver "go.mongodb.org/mongo-driver/v2/mongo"
	moptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoTarget drives oracle operations against a live MongoDB server. It is the
// reference side of the behavior oracle: doc's results are diffed against this
// target's, so doc is measured against MongoDB's actual behavior rather than a
// hand-written spec (spec 2061 doc 19 §17).
//
// It lives in this nested module, behind the `mongo` build tag, so the doc module
// itself never depends on the MongoDB driver.
type MongoTarget struct {
	client *mdriver.Client
	dbName string
}

// NewMongoTarget connects to the MongoDB server at uri and uses dbName as the
// scratch database the harness resets between cases. The caller closes it.
func NewMongoTarget(uri, dbName string) (*MongoTarget, error) {
	client, err := mdriver.Connect(moptions.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", uri, err)
	}
	if err := client.Ping(context.Background(), nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("ping %s: %w", uri, err)
	}
	return &MongoTarget{client: client, dbName: dbName}, nil
}

// Name identifies the target in diff output.
func (m *MongoTarget) Name() string { return "mongodb" }

// Reset drops the scratch database so each case starts from an empty server.
func (m *MongoTarget) Reset() error {
	return m.client.Database(m.dbName).Drop(context.Background())
}

// Close disconnects from the server.
func (m *MongoTarget) Close() error {
	return m.client.Disconnect(context.Background())
}

// db returns a handle to the named collection in the scratch database.
func (m *MongoTarget) coll(name string) *mdriver.Collection {
	return m.client.Database(m.dbName).Collection(name)
}

// Exec runs op against MongoDB and normalizes the outcome into an oracle.Result.
// A modeled behavioral error (duplicate key) becomes a Result.ErrCode; an
// unexpected transport error is returned so the harness fails the run.
func (m *MongoTarget) Exec(op oracle.Op) (oracle.Result, error) {
	ctx := context.Background()
	c := m.coll(op.Collection)

	switch op.Kind {
	case oracle.OpInsertOne:
		if _, err := c.InsertOne(ctx, mongoDoc(op.Doc)); err != nil {
			if mdriver.IsDuplicateKeyError(err) {
				return oracle.Result{ErrCode: "DuplicateKey"}, nil
			}
			return oracle.Result{}, err
		}
		return oracle.Result{N: 1}, nil

	case oracle.OpFindOne:
		opts := moptions.FindOne()
		if len(op.Sort) > 0 {
			opts.SetSort(mongoDoc(op.Sort))
		}
		if len(op.Projection) > 0 {
			opts.SetProjection(mongoDoc(op.Projection))
		}
		if op.Skip > 0 {
			opts.SetSkip(op.Skip)
		}
		var raw mbson.Raw
		err := c.FindOne(ctx, mongoFilter(op.Filter), opts).Decode(&raw)
		if errors.Is(err, mdriver.ErrNoDocuments) {
			return oracle.Result{}, nil
		}
		if err != nil {
			return oracle.Result{}, err
		}
		return oracle.Result{Docs: []bson.Raw{toRaw(raw)}}, nil

	case oracle.OpFind:
		opts := moptions.Find()
		if len(op.Sort) > 0 {
			opts.SetSort(mongoDoc(op.Sort))
		}
		if len(op.Projection) > 0 {
			opts.SetProjection(mongoDoc(op.Projection))
		}
		if op.Skip > 0 {
			opts.SetSkip(op.Skip)
		}
		if op.Limit != 0 {
			opts.SetLimit(op.Limit)
		}
		cur, err := c.Find(ctx, mongoFilter(op.Filter), opts)
		if err != nil {
			return oracle.Result{}, err
		}
		defer func() { _ = cur.Close(ctx) }()
		var docs []bson.Raw
		for cur.Next(ctx) {
			docs = append(docs, toRaw(cur.Current))
		}
		if err := cur.Err(); err != nil {
			return oracle.Result{}, err
		}
		return oracle.Result{Docs: docs}, nil

	case oracle.OpDeleteOne:
		res, err := c.DeleteOne(ctx, mongoFilter(op.Filter))
		if err != nil {
			return oracle.Result{}, err
		}
		return oracle.Result{N: res.DeletedCount}, nil

	case oracle.OpCount:
		n, err := c.CountDocuments(ctx, mongoFilter(op.Filter))
		if err != nil {
			return oracle.Result{}, err
		}
		return oracle.Result{N: n}, nil

	case oracle.OpUpdateOne:
		res, err := c.UpdateOne(ctx, mongoFilter(op.Filter), mongoDoc(op.Update))
		return mongoUpdateResult(res, err)

	case oracle.OpUpdateMany:
		res, err := c.UpdateMany(ctx, mongoFilter(op.Filter), mongoDoc(op.Update))
		return mongoUpdateResult(res, err)

	case oracle.OpReplaceOne:
		res, err := c.ReplaceOne(ctx, mongoFilter(op.Filter), mongoDoc(op.Replacement))
		return mongoUpdateResult(res, err)

	case oracle.OpFindOneAndUpdate:
		opts := moptions.FindOneAndUpdate()
		applyFindModify(opts, op)
		return mongoSingle(c.FindOneAndUpdate(ctx, mongoFilter(op.Filter), mongoDoc(op.Update), opts))

	case oracle.OpFindOneAndReplace:
		opts := moptions.FindOneAndReplace()
		applyFindModifyReplace(opts, op)
		return mongoSingle(c.FindOneAndReplace(ctx, mongoFilter(op.Filter), mongoDoc(op.Replacement), opts))

	case oracle.OpFindOneAndDelete:
		opts := moptions.FindOneAndDelete()
		if len(op.Sort) > 0 {
			opts.SetSort(mongoDoc(op.Sort))
		}
		if len(op.Projection) > 0 {
			opts.SetProjection(mongoDoc(op.Projection))
		}
		return mongoSingle(c.FindOneAndDelete(ctx, mongoFilter(op.Filter), opts))

	case oracle.OpDistinct:
		res := c.Distinct(ctx, op.Field, mongoFilter(op.Filter))
		if err := res.Err(); err != nil {
			return oracle.Result{}, err
		}
		var vals []mbson.RawValue
		if err := res.Decode(&vals); err != nil {
			return oracle.Result{}, err
		}
		docs := make([]bson.Raw, 0, len(vals))
		for _, v := range vals {
			b, err := mbson.Marshal(mbson.D{{Key: "v", Value: v}})
			if err != nil {
				return oracle.Result{}, err
			}
			docs = append(docs, toRaw(b))
		}
		return oracle.Result{Docs: oracle.NormalizeDistinctDocs(docs)}, nil

	default:
		return oracle.Result{}, fmt.Errorf("conformance: unsupported op kind %q", op.Kind)
	}
}

// mongoUpdateResult normalizes a driver UpdateResult, mapping a modeled write
// error to a Result.ErrCode and other errors to a transport failure.
func mongoUpdateResult(res *mdriver.UpdateResult, err error) (oracle.Result, error) {
	if err != nil {
		if code, ok := mongoErrCode(err); ok {
			return oracle.Result{ErrCode: code}, nil
		}
		return oracle.Result{}, err
	}
	return oracle.Result{Matched: res.MatchedCount, Modified: res.ModifiedCount}, nil
}

// mongoSingle normalizes a findAndModify SingleResult into the returned document,
// an empty result when nothing matched, or a modeled error code.
func mongoSingle(sr *mdriver.SingleResult) (oracle.Result, error) {
	var raw mbson.Raw
	err := sr.Decode(&raw)
	if errors.Is(err, mdriver.ErrNoDocuments) {
		return oracle.Result{}, nil
	}
	if err != nil {
		if code, ok := mongoErrCode(err); ok {
			return oracle.Result{ErrCode: code}, nil
		}
		return oracle.Result{}, err
	}
	return oracle.Result{Docs: []bson.Raw{toRaw(raw)}}, nil
}

// applyFindModify sets the sort, projection, and return-document options shared by
// findOneAndUpdate.
func applyFindModify(opts *moptions.FindOneAndUpdateOptionsBuilder, op oracle.Op) {
	if len(op.Sort) > 0 {
		opts.SetSort(mongoDoc(op.Sort))
	}
	if len(op.Projection) > 0 {
		opts.SetProjection(mongoDoc(op.Projection))
	}
	if op.ReturnAfter {
		opts.SetReturnDocument(moptions.After)
	} else {
		opts.SetReturnDocument(moptions.Before)
	}
}

// applyFindModifyReplace is applyFindModify for the findOneAndReplace builder.
func applyFindModifyReplace(opts *moptions.FindOneAndReplaceOptionsBuilder, op oracle.Op) {
	if len(op.Sort) > 0 {
		opts.SetSort(mongoDoc(op.Sort))
	}
	if len(op.Projection) > 0 {
		opts.SetProjection(mongoDoc(op.Projection))
	}
	if op.ReturnAfter {
		opts.SetReturnDocument(moptions.After)
	} else {
		opts.SetReturnDocument(moptions.Before)
	}
}

// mongoErrCode maps a driver write error to the oracle's normalized error
// category by MongoDB's numeric error code, matching the doc target's categories.
func mongoErrCode(err error) (string, bool) {
	if mdriver.IsDuplicateKeyError(err) {
		return "DuplicateKey", true
	}
	code := 0
	var we mdriver.WriteException
	if errors.As(err, &we) && len(we.WriteErrors) > 0 {
		code = we.WriteErrors[0].Code
	}
	var ce mdriver.CommandError
	if errors.As(err, &ce) {
		code = int(ce.Code)
	}
	switch code {
	case 66:
		return "ImmutableField", true
	case 40:
		return "ConflictingUpdateOperators", true
	case 14:
		return "TypeMismatch", true
	case 28:
		return "PathNotViable", true
	}
	return "", false
}
