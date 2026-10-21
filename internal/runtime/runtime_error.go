package runtime

import "errors"

type runtimeHTTPError struct {
	code int
	err  error
}

func (e runtimeHTTPError) Error() string {
	return e.err.Error()
}

func (e runtimeHTTPError) Unwrap() error {
	return e.err
}

func runtimeHTTPErrorStatus(err error) (int, bool) {
	var runtimeErr runtimeHTTPError
	if err != nil && errors.As(err, &runtimeErr) {
		return runtimeErr.code, true
	}
	return configRuntimeErrorStatus(err)
}
