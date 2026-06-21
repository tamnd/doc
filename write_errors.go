package doc

import (
	"fmt"
	"strings"
)

// MongoDB error codes surfaced by the typed write-error path (spec 2061 doc 14
// §17.5). Only the codes doc can actually raise are enumerated.
const (
	codeBadValue           = 2
	codeNamespaceNotFound  = 26
	codeConversionFailure  = 40
	codeNamespaceExists    = 48
	codeMaxTimeMSExpired   = 50
	codeWriteConflict      = 112
	codeDocumentValidation = 121
	codeNoSuchTransaction  = 251
	codeDuplicateKey       = 11000
	codeCappedDelete       = 10101
	codeInvalidIndexOption = 73
)

// WriteError describes a single failed document write within a larger operation,
// carrying the MongoDB error code so callers can branch on, say, a duplicate
// key (spec 2061 doc 14 §17.2).
type WriteError struct {
	Index   int
	Code    int
	Message string
}

func (e WriteError) Error() string {
	return fmt.Sprintf("write error at index %d (code %d): %s", e.Index, e.Code, e.Message)
}

// WriteConcernError reports that a write succeeded locally but did not satisfy
// the requested durability. doc is single-node, so this is reserved and only
// populated by future replicated configurations.
type WriteConcernError struct {
	Code    int
	Message string
}

func (e WriteConcernError) Error() string {
	return fmt.Sprintf("write concern error (code %d): %s", e.Code, e.Message)
}

// WriteException is returned by the single-document and *Many write methods when
// one or more individual writes fail. Inspect WriteErrors for the per-document
// codes (spec 2061 doc 14 §17.2).
type WriteException struct {
	WriteConcernError *WriteConcernError
	WriteErrors       []WriteError
	Labels            []string

	// cause carries an unexported sentinel so errors.Is can match the typed
	// exception against, say, ErrDocumentValidation without exposing a field.
	cause error
}

func (e WriteException) Error() string {
	parts := make([]string, 0, len(e.WriteErrors)+1)
	for _, we := range e.WriteErrors {
		parts = append(parts, we.Error())
	}
	if e.WriteConcernError != nil {
		parts = append(parts, e.WriteConcernError.Error())
	}
	if len(parts) == 0 {
		return "write exception"
	}
	return "write exception: " + strings.Join(parts, "; ")
}

// HasErrorCode reports whether any contained write error carries code.
func (e WriteException) HasErrorCode(code int) bool {
	for _, we := range e.WriteErrors {
		if we.Code == code {
			return true
		}
	}
	return false
}

// Unwrap exposes the underlying sentinel (if any) so callers can match the typed
// exception with errors.Is against names like ErrDocumentValidation.
func (e WriteException) Unwrap() error { return e.cause }

// BulkWriteError is one failed operation in a bulkWrite, tying the WriteError to
// the model that produced it (spec 2061 doc 14 §17.3).
type BulkWriteError struct {
	WriteError
	Request WriteModel
}

// BulkWriteException is returned by BulkWrite when one or more operations fail.
type BulkWriteException struct {
	WriteConcernError *WriteConcernError
	WriteErrors       []BulkWriteError
	Labels            []string
}

func (e BulkWriteException) Error() string {
	parts := make([]string, 0, len(e.WriteErrors)+1)
	for _, we := range e.WriteErrors {
		parts = append(parts, we.Error())
	}
	if e.WriteConcernError != nil {
		parts = append(parts, e.WriteConcernError.Error())
	}
	if len(parts) == 0 {
		return "bulk write exception"
	}
	return "bulk write exception: " + strings.Join(parts, "; ")
}

// HasErrorCode reports whether any contained write error carries code.
func (e BulkWriteException) HasErrorCode(code int) bool {
	for _, we := range e.WriteErrors {
		if we.Code == code {
			return true
		}
	}
	return false
}

// CommandError reports the failure of a database command run through RunCommand
// or of a transaction commit. The Labels carry retryability hints such as
// TransientTransactionError (spec 2061 doc 14 §17.4).
type CommandError struct {
	Code    int32
	Message string
	Labels  []string
	Name    string
}

func (e CommandError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("(%s) %s", e.Name, e.Message)
	}
	return e.Message
}

// HasLabel reports whether the error carries the given error label.
func (e CommandError) HasLabel(label string) bool {
	for _, l := range e.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// duplicateKeyException wraps a unique-index violation from the engine as a
// WriteException with the standard 11000 code at the given operation index.
func duplicateKeyException(index int, err error) WriteException {
	return WriteException{
		WriteErrors: []WriteError{{
			Index:   index,
			Code:    codeDuplicateKey,
			Message: err.Error(),
		}},
	}
}

// validationException wraps a schema-validation failure from the engine as a
// WriteException with MongoDB's DocumentValidationFailure code 121 (spec 2061 doc 09
// §10.4). The error also satisfies errors.Is(err, ErrDocumentValidation).
func validationException(err error) error {
	return WriteException{
		WriteErrors: []WriteError{{
			Index:   0,
			Code:    codeDocumentValidation,
			Message: err.Error(),
		}},
		cause: ErrDocumentValidation,
	}
}
