package wire

import (
	"context"
	"encoding/base64"
	"errors"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
)

// User management stores credentials in admin.system.users, the single user collection
// MongoDB keys every account by, regardless of the database the account authenticates
// against (spec 2061 doc 16 §8.2). The _id is "<authdb>.<user>" and the db field records
// the authentication database. doc reaches this collection through the same public
// Collection API a client would, so the credential store is an ordinary collection with no
// special engine path.

// scramMechName is the credential sub-document key under which a SCRAM-SHA-256 credential
// is stored.
const scramMechName = "SCRAM-SHA-256"

// usersCollection returns the handle to admin.system.users.
func (c *conn) usersCollection() *doc.Collection {
	return c.collection("admin", "system.users")
}

// hasUsers reports whether any user exists, which decides the localhost exception. It runs
// off admin.system.users so it sees users from every authentication database at once.
func (s *Server) hasUsers(ctx context.Context) bool {
	n, err := s.db.Database("admin").Collection("system.users").
		CountDocuments(ctx, bson.NewBuilder().Build())
	return err == nil && n > 0
}

// userID is the _id a user is stored under: the authentication database and user name.
func userID(db, user string) string { return db + "." + user }

// lookupCredential loads a user's stored SCRAM credential and role grants. found is false
// when no such user exists, which the caller turns into a decoy conversation so the wire
// never reveals which usernames are real.
func (c *conn) lookupCredential(ctx context.Context, db, user string) (cred scramCredential, roles []roleRef, found bool) {
	filter := bson.NewBuilder().AppendString("_id", userID(db, user)).Build()
	raw, err := c.usersCollection().FindOne(ctx, filter).Raw()
	if err != nil {
		return scramCredential{}, nil, false
	}
	cred, ok := readStoredCredential(raw)
	if !ok {
		return scramCredential{}, nil, false
	}
	if v, ok := raw.Lookup("roles"); ok {
		roles = parseRoles(v, db)
	}
	return cred, roles, true
}

// readStoredCredential decodes the SCRAM-SHA-256 credential out of a stored user document.
func readStoredCredential(raw bson.Raw) (scramCredential, bool) {
	credsVal, ok := raw.Lookup("credentials")
	if !ok || credsVal.Type != bson.TypeDocument {
		return scramCredential{}, false
	}
	scramVal, ok := credsVal.Document().Lookup(scramMechName)
	if !ok || scramVal.Type != bson.TypeDocument {
		return scramCredential{}, false
	}
	d := scramVal.Document()
	iter := defaultIterationCount
	if v, ok := d.Lookup("iterationCount"); ok {
		if n, ok := v.Int32OK(); ok {
			iter = int(n)
		}
	}
	salt, err1 := base64.StdEncoding.DecodeString(lookupString(d, "salt"))
	storedKey, err2 := base64.StdEncoding.DecodeString(lookupString(d, "storedKey"))
	serverKey, err3 := base64.StdEncoding.DecodeString(lookupString(d, "serverKey"))
	if err1 != nil || err2 != nil || err3 != nil {
		return scramCredential{}, false
	}
	return scramCredential{
		iterationCount: iter,
		salt:           salt,
		storedKey:      storedKey,
		serverKey:      serverKey,
	}, true
}

// credentialDoc renders a derived credential into its stored BSON sub-document, with the
// salt and keys base64-encoded as MongoDB stores them.
func credentialDoc(cred scramCredential) bson.Raw {
	scram := bson.NewBuilder().
		AppendInt32("iterationCount", int32(cred.iterationCount)).
		AppendString("salt", base64.StdEncoding.EncodeToString(cred.salt)).
		AppendString("storedKey", base64.StdEncoding.EncodeToString(cred.storedKey)).
		AppendString("serverKey", base64.StdEncoding.EncodeToString(cred.serverKey)).
		Build()
	return bson.NewBuilder().AppendDocument(scramMechName, scram).Build()
}

// userDoc builds a full stored user document.
func userDoc(db, user string, cred scramCredential, roles []roleRef) bson.Raw {
	return bson.NewBuilder().
		AppendString("_id", userID(db, user)).
		AppendObjectID("userId", doc.NewObjectID()).
		AppendString("user", user).
		AppendString("db", db).
		AppendDocument("credentials", credentialDoc(cred)).
		AppendArray("roles", rolesArray(roles)).
		Build()
}

// dispatchUsers handles the user-management commands. It returns (reply, true) for a
// command it owns and (nil, false) otherwise so dispatch falls through to the data and
// configuration surfaces.
func (c *conn) dispatchUsers(ctx context.Context, db, name string, body bson.Raw) (bson.Raw, bool) {
	switch name {
	case "createuser":
		return c.handleCreateUser(ctx, db, body), true
	case "dropuser":
		return c.handleDropUser(ctx, db, body), true
	case "updateuser":
		return c.handleUpdateUser(ctx, db, body), true
	case "usersinfo":
		return c.handleUsersInfo(ctx, db, body), true
	case "grantrolestouser":
		return c.handleRoleChange(ctx, db, body, "grantRolesToUser", true), true
	case "revokerolesfromuser":
		return c.handleRoleChange(ctx, db, body, "revokeRolesFromUser", false), true
	default:
		return nil, false
	}
}

// handleCreateUser stores a new account. The password is salted into a SCRAM credential and
// discarded; only the credential is persisted (spec 2061 doc 16 §8.2).
func (c *conn) handleCreateUser(ctx context.Context, db string, body bson.Raw) bson.Raw {
	if guard := c.readOnlyGuard(); guard != nil {
		return guard
	}
	user := lookupString(body, "createUser")
	pwd := lookupString(body, "pwd")
	if user == "" {
		return errorDoc(2, "BadValue", "createUser requires a user name")
	}
	if pwd == "" {
		return errorDoc(2, "BadValue", "createUser requires a password")
	}
	var roles []roleRef
	if v, ok := body.Lookup("roles"); ok {
		roles = parseRoles(v, db)
	}
	cred, err := newCredential(pwd)
	if err != nil {
		return errorDoc(1, "InternalError", "deriving credential: "+err.Error())
	}
	if _, err := c.usersCollection().InsertOne(ctx, userDoc(db, user, cred, roles)); err != nil {
		if isDuplicateKey(err) {
			return errorDoc(51003, "Location51003", "User \""+user+"@"+db+"\" already exists")
		}
		return errorReplyFrom(err)
	}
	return okReply()
}

// handleDropUser removes an account.
func (c *conn) handleDropUser(ctx context.Context, db string, body bson.Raw) bson.Raw {
	if guard := c.readOnlyGuard(); guard != nil {
		return guard
	}
	user := lookupString(body, "dropUser")
	if user == "" {
		return errorDoc(2, "BadValue", "dropUser requires a user name")
	}
	filter := bson.NewBuilder().AppendString("_id", userID(db, user)).Build()
	res, err := c.usersCollection().DeleteOne(ctx, filter)
	if err != nil {
		return errorReplyFrom(err)
	}
	if res.DeletedCount == 0 {
		return errorDoc(11, "UserNotFound", "User \""+user+"@"+db+"\" not found")
	}
	return okReply()
}

// handleUpdateUser changes a user's password, roles, or both. Only the provided fields
// change; an absent field is left as stored.
func (c *conn) handleUpdateUser(ctx context.Context, db string, body bson.Raw) bson.Raw {
	if guard := c.readOnlyGuard(); guard != nil {
		return guard
	}
	user := lookupString(body, "updateUser")
	if user == "" {
		return errorDoc(2, "BadValue", "updateUser requires a user name")
	}
	cred, roles, found := c.lookupCredential(ctx, db, user)
	if !found {
		return errorDoc(11, "UserNotFound", "User \""+user+"@"+db+"\" not found")
	}
	if pwd := lookupString(body, "pwd"); pwd != "" {
		newCred, err := newCredential(pwd)
		if err != nil {
			return errorDoc(1, "InternalError", "deriving credential: "+err.Error())
		}
		cred = newCred
	}
	if v, ok := body.Lookup("roles"); ok {
		roles = parseRoles(v, db)
	}
	if err := c.replaceUser(ctx, db, user, cred, roles); err != nil {
		return errorReplyFrom(err)
	}
	return okReply()
}

// handleRoleChange grants or revokes roles on an existing user.
func (c *conn) handleRoleChange(ctx context.Context, db string, body bson.Raw, field string, grant bool) bson.Raw {
	if guard := c.readOnlyGuard(); guard != nil {
		return guard
	}
	user := lookupString(body, field)
	if user == "" {
		return errorDoc(2, "BadValue", field+" requires a user name")
	}
	cred, roles, found := c.lookupCredential(ctx, db, user)
	if !found {
		return errorDoc(11, "UserNotFound", "User \""+user+"@"+db+"\" not found")
	}
	var delta []roleRef
	if v, ok := body.Lookup("roles"); ok {
		delta = parseRoles(v, db)
	}
	if grant {
		roles = mergeRoles(roles, delta)
	} else {
		roles = removeRoles(roles, delta)
	}
	if err := c.replaceUser(ctx, db, user, cred, roles); err != nil {
		return errorReplyFrom(err)
	}
	return okReply()
}

// replaceUser rewrites a stored user document in place, preserving its _id.
func (c *conn) replaceUser(ctx context.Context, db, user string, cred scramCredential, roles []roleRef) error {
	filter := bson.NewBuilder().AppendString("_id", userID(db, user)).Build()
	_, err := c.usersCollection().ReplaceOne(ctx, filter, userDoc(db, user, cred, roles))
	return err
}

// handleUsersInfo returns user documents without their credentials, the shape the
// usersInfo command reports (spec 2061 doc 16 §8.2). The selector is 1 (every user on the
// database), a name, or a {user, db} document.
func (c *conn) handleUsersInfo(ctx context.Context, db string, body bson.Raw) bson.Raw {
	var ids []string
	var listAll bool
	if v, ok := body.Lookup("usersInfo"); ok {
		switch v.Type {
		case bson.TypeString:
			ids = append(ids, userID(db, v.StringValue()))
		case bson.TypeDocument:
			d := v.Document()
			u := lookupString(d, "user")
			udb := lookupString(d, "db")
			if udb == "" {
				udb = db
			}
			ids = append(ids, userID(udb, u))
		case bson.TypeArray:
			for _, e := range arrayElements(v) {
				switch e.Type {
				case bson.TypeString:
					ids = append(ids, userID(db, e.StringValue()))
				case bson.TypeDocument:
					d := e.Document()
					udb := lookupString(d, "db")
					if udb == "" {
						udb = db
					}
					ids = append(ids, userID(udb, lookupString(d, "user")))
				}
			}
		default:
			listAll = true
		}
	}

	var found []bson.RawValue
	if listAll {
		cur, err := c.usersCollection().Find(ctx,
			bson.NewBuilder().AppendString("db", db).Build())
		if err == nil {
			for cur.Next(ctx) {
				found = append(found, publicUserValue(cur.Current()))
			}
			_ = cur.Close(ctx)
		}
	} else {
		for _, id := range ids {
			filter := bson.NewBuilder().AppendString("_id", id).Build()
			if raw, err := c.usersCollection().FindOne(ctx, filter).Raw(); err == nil {
				found = append(found, publicUserValue(raw))
			}
		}
	}
	return bson.NewBuilder().
		AppendArray("users", bson.BuildArray(found...)).
		AppendDouble("ok", 1).
		Build()
}

// publicUserValue strips the credentials from a stored user document, leaving the public
// view usersInfo returns.
func publicUserValue(raw bson.Raw) bson.RawValue {
	b := bson.NewBuilder().
		AppendString("_id", lookupString(raw, "_id")).
		AppendString("user", lookupString(raw, "user")).
		AppendString("db", lookupString(raw, "db"))
	if v, ok := raw.Lookup("userId"); ok && v.Type == bson.TypeObjectID {
		b.AppendObjectID("userId", v.ObjectID())
	}
	if v, ok := raw.Lookup("roles"); ok && v.Type == bson.TypeArray {
		b.AppendArray("roles", v.Document())
	}
	return bson.RawValue{Type: bson.TypeDocument, Data: b.Build()}
}

// mergeRoles adds delta roles to base, skipping ones already present.
func mergeRoles(base, delta []roleRef) []roleRef {
	for _, d := range delta {
		if !containsRole(base, d) {
			base = append(base, d)
		}
	}
	return base
}

// removeRoles drops every delta role from base.
func removeRoles(base, delta []roleRef) []roleRef {
	out := base[:0]
	for _, r := range base {
		if !containsRole(delta, r) {
			out = append(out, r)
		}
	}
	return out
}

func containsRole(set []roleRef, r roleRef) bool {
	for _, s := range set {
		if s.role == r.role && s.db == r.db {
			return true
		}
	}
	return false
}

// isDuplicateKey reports whether err is a duplicate-key write error, used to map a repeat
// createUser to the right code.
func isDuplicateKey(err error) bool {
	var we doc.WriteException
	if errors.As(err, &we) {
		for _, w := range we.WriteErrors {
			if w.Code == 11000 {
				return true
			}
		}
	}
	return false
}
