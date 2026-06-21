package main

// Exit codes are normative (spec 2061 doc 15 §17). Scripts test them numerically, so
// the values are fixed and a new code may only be appended in a minor version.
const (
	exitOK              = 0
	exitNotFound        = 1
	exitUsage           = 2
	exitCannotOpen      = 3
	exitCorruption      = 4
	exitQueryError      = 5
	exitInterrupted     = 6
	exitAuthError       = 7
	exitIOError         = 8
	exitLockTimeout     = 9
	exitSchemaViolation = 10
	exitVersionMismatch = 11
)

// cliError pairs a message with the exit code the process should carry when the error
// reaches the top level. A bare error coming back from the engine is mapped to a code
// by classifyError; a cliError says the code outright.
type cliError struct {
	code int
	msg  string
}

func (e cliError) Error() string { return e.msg }

func usageError(msg string) cliError { return cliError{code: exitUsage, msg: msg} }
func openError(msg string) cliError  { return cliError{code: exitCannotOpen, msg: msg} }
func queryError(msg string) cliError { return cliError{code: exitQueryError, msg: msg} }
