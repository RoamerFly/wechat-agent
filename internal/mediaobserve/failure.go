package mediaobserve

// NewFailure returns a safe tool error without exposing the underlying diagnostic.
func NewFailure(code, message string, cause error) error {
	return &failure{code, message, cause}
}

type failure struct {
	code, message string
	cause         error
}

func (e *failure) Error() string                     { return e.code }
func (e *failure) Unwrap() error                     { return e.cause }
func (e *failure) SafeToolFailure() (string, string) { return e.code, e.message }
