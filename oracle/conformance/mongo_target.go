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
		var raw mbson.Raw
		err := c.FindOne(ctx, mongoFilter(op.Filter)).Decode(&raw)
		if errors.Is(err, mdriver.ErrNoDocuments) {
			return oracle.Result{}, nil
		}
		if err != nil {
			return oracle.Result{}, err
		}
		return oracle.Result{Docs: []bson.Raw{toRaw(raw)}}, nil

	case oracle.OpFind:
		cur, err := c.Find(ctx, mongoFilter(op.Filter))
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

	default:
		return oracle.Result{}, fmt.Errorf("conformance: unsupported op kind %q", op.Kind)
	}
}
