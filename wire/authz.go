package wire

import (
	"context"

	"github.com/tamnd/doc/bson"
)

// Authorization is the gate every command passes through once a server runs with auth
// required (spec 2061 doc 16 §19). A connection is either authenticated to an identity
// carrying a set of role grants, or it is anonymous and only the loopback exception lets
// it through. This file owns the role-to-command mapping and the per-command check; the
// SCRAM handshake that produces an identity lives in auth.go.

// roleRef is a single role grant: a built-in role name scoped to a database. root on admin
// is cluster-wide; the database-scoped roles apply only to their own database.
type roleRef struct {
	role string
	db   string
}

// identity is the authenticated principal on a connection: the user, the database it
// authenticated against, and the roles it was granted (spec 2061 doc 16 §19.2).
type identity struct {
	user  string
	db    string
	roles []roleRef
}

// capability bits group commands by the privilege they need. A role grants one or more of
// these, and a command requires exactly one (or none, for commands any authenticated user
// may run).
const (
	capRead      = 1 << iota // find, aggregate, count, distinct, list*, explain, getMore
	capWrite                 // insert, update, delete, findAndModify, index and DDL writes
	capDBAdmin               // collMod, renameCollection, dbStats, collStats, validate
	capUserAdmin             // createUser, dropUser, updateUser, usersInfo, grant/revoke
	capCluster               // serverStatus, currentOp, killOp, getLog, get/setParameter
)

// roleCapabilities is the built-in role table from spec 2061 doc 16 §19.2. dbAdmin builds
// on readWrite, which builds on read; userAdmin and clusterAdmin are separate axes; root
// is handled as an all-grant short-circuit, not a bitmask.
var roleCapabilities = map[string]int{
	"read":         capRead,
	"readWrite":    capRead | capWrite,
	"dbAdmin":      capRead | capWrite | capDBAdmin,
	"userAdmin":    capUserAdmin,
	"clusterAdmin": capCluster,
}

// commandCapability returns the capability a command needs, or 0 when any authenticated
// user may run it. The handshake and SASL commands never reach this table; they are
// answered before the authorization gate.
func commandCapability(name string) int {
	switch name {
	case "find", "aggregate", "count", "distinct", "listcollections",
		"listindexes", "explain", "getmore", "killcursors":
		return capRead
	case "insert", "update", "delete", "findandmodify", "createindexes",
		"dropindexes", "create", "drop":
		return capWrite
	case "collmod", "renamecollection", "dbstats", "collstats", "validate":
		return capDBAdmin
	case "createuser", "dropuser", "updateuser", "usersinfo",
		"grantrolestouser", "revokerolesfromuser":
		return capUserAdmin
	case "serverstatus", "currentop", "killop", "getlog", "getparameter", "setparameter":
		return capCluster
	default:
		return 0
	}
}

// freeCommands run without authentication even when auth is required, so a driver can
// complete the handshake and authenticate (spec 2061 doc 16 §19.1).
func isFreeCommand(name string) bool {
	switch name {
	case "hello", "ismaster", "ping", "buildinfo",
		"saslstart", "saslcontinue", "logout":
		return true
	default:
		return false
	}
}

// permits reports whether the identity may run a command needing capability cap against
// targetDB. root grants everything; a cluster command is satisfied by any cluster grant
// regardless of database; every other capability requires a matching grant on the same
// database (spec 2061 doc 16 §19.2).
func (id *identity) permits(need int, targetDB string) bool {
	for _, r := range id.roles {
		if r.role == "root" {
			return true
		}
		caps, ok := roleCapabilities[r.role]
		if !ok || caps&need == 0 {
			continue
		}
		if need == capCluster || r.db == targetDB {
			return true
		}
	}
	return false
}

// authorize is the gate dispatch calls before routing a command. It returns nil to allow,
// or an error reply to send back. With auth disabled everything is allowed; with auth
// required an anonymous connection gets the loopback exception only while no users exist,
// and an authenticated connection is checked against its roles (spec 2061 doc 16 §19).
func (c *conn) authorize(ctx context.Context, db, name string) bson.Raw {
	if !c.srv.authConfigured() {
		return nil
	}
	if isFreeCommand(name) {
		return nil
	}
	if c.auth == nil {
		if c.localhostException(ctx) {
			return nil
		}
		return errorDoc(13, "Unauthorized", "command "+name+" requires authentication")
	}
	need := commandCapability(name)
	if need == 0 || c.auth.permits(need, db) {
		return nil
	}
	return errorDoc(13, "Unauthorized",
		"not authorized on "+db+" to execute command "+name)
}

// localhostException grants an anonymous loopback connection full access while the server
// has no users, so an operator can create the first user (spec 2061 doc 16 §8.6, §19.5).
// It is revoked the moment a user exists.
func (c *conn) localhostException(ctx context.Context) bool {
	return c.isLoopback() && !c.srv.hasUsers(ctx)
}

// parseRoles reads a roles array as it appears in a createUser/updateUser command or a
// stored user document. An entry is either a bare string (a role on defaultDB) or a
// document {role, db}.
func parseRoles(v bson.RawValue, defaultDB string) []roleRef {
	if v.Type != bson.TypeArray {
		return nil
	}
	var roles []roleRef
	for _, e := range arrayElements(v) {
		switch e.Type {
		case bson.TypeString:
			roles = append(roles, roleRef{role: e.StringValue(), db: defaultDB})
		case bson.TypeDocument:
			d := e.Document()
			role := lookupString(d, "role")
			db := lookupString(d, "db")
			if db == "" {
				db = defaultDB
			}
			if role != "" {
				roles = append(roles, roleRef{role: role, db: db})
			}
		}
	}
	return roles
}

// rolesArray renders role grants back into the BSON array shape a stored user document and
// a usersInfo reply use: [{role, db}, ...].
func rolesArray(roles []roleRef) bson.Raw {
	vals := make([]bson.RawValue, 0, len(roles))
	for _, r := range roles {
		d := bson.NewBuilder().
			AppendString("role", r.role).
			AppendString("db", r.db).
			Build()
		vals = append(vals, bson.RawValue{Type: bson.TypeDocument, Data: d})
	}
	return bson.BuildArray(vals...)
}
