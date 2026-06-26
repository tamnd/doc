package wire

import (
	"errors"

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
		b := bson.NewBuilder().
			AppendDouble("ok", 0).
			AppendInt32("code", ce.Code).
			AppendString("codeName", name).
			AppendString("errmsg", ce.Message)
		return b.Build()
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
