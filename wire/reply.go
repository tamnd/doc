package wire

import (
	"errors"
	"strconv"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
)

// errorDoc builds the MongoDB command error reply shape: {ok: 0, code, codeName,
// errmsg} (spec 2061 doc 16 §13.1).
func errorDoc(code int32, codeName, errmsg string) bson.Raw {
	return bson.NewBuilder().
		AppendDouble("ok", 0).
		AppendInt32("code", code).
		AppendString("codeName", codeName).
		AppendString("errmsg", errmsg).
		Build()
}

// errorDocLabeled builds an error reply that carries an errorLabels array, the hint a
// driver reads to decide whether to retry the whole transaction (spec 2061 doc 16
// §13.4). With no labels it is the plain error shape.
func errorDocLabeled(code int32, codeName, errmsg string, labels ...string) bson.Raw {
	b := bson.NewBuilder().
		AppendDouble("ok", 0).
		AppendInt32("code", code).
		AppendString("codeName", codeName).
		AppendString("errmsg", errmsg)
	if len(labels) > 0 {
		b.AppendArray("errorLabels", stringArray(labels))
	}
	return b.Build()
}

// stringArray builds a BSON array payload from a slice of strings, keyed by position.
func stringArray(vals []string) bson.Raw {
	b := bson.NewBuilder()
	for i, v := range vals {
		b.AppendString(strconv.Itoa(i), v)
	}
	return b.Build()
}

// errorReplyFrom turns a library error into a wire error document. A CommandError keeps
// its code and name; a write exception maps to its first write error's code; anything
// else falls back to a generic internal error.
func errorReplyFrom(err error) bson.Raw {
	var ce doc.CommandError
	if errors.As(err, &ce) {
		name := ce.Name
		if name == "" {
			name = "Error"
		}
		return errorDocLabeled(ce.Code, name, ce.Message, ce.Labels...)
	}

	var we doc.WriteException
	if errors.As(err, &we) && len(we.WriteErrors) > 0 {
		first := we.WriteErrors[0]
		return errorDoc(int32(first.Code), codeNameFor(int32(first.Code)), first.Message)
	}

	switch {
	case errors.Is(err, doc.ErrNamespaceNotFound):
		return errorDoc(26, "NamespaceNotFound", err.Error())
	case errors.Is(err, doc.ErrNamespaceExists):
		return errorDoc(48, "NamespaceExists", err.Error())
	case errors.Is(err, doc.ErrReadOnly):
		return errorDoc(166, "IllegalOperation", err.Error())
	case errors.Is(err, doc.ErrClosed):
		return errorDoc(11600, "InterruptedAtShutdown", err.Error())
	default:
		return errorDoc(1, "InternalError", err.Error())
	}
}

// codeNameFor maps the MongoDB error codes the engine can raise to their codeName so
// the wire reply carries the pair a driver expects.
func codeNameFor(code int32) string {
	switch code {
	case 2:
		return "BadValue"
	case 26:
		return "NamespaceNotFound"
	case 48:
		return "NamespaceExists"
	case 50:
		return "MaxTimeMSExpired"
	case 112:
		return "WriteConflict"
	case 121:
		return "DocumentValidationFailure"
	case 11000:
		return "DuplicateKey"
	default:
		return "Error"
	}
}
