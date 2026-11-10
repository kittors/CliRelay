package bodyutil

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/jsonstrict"
)

// checkedJSONBodyKey holds the stored body CheckJSONRequestBody last passed.
const checkedJSONBodyKey = "cliproxy.request_body_json_checked"

// CheckJSONRequestBody reports whether the request body is one strict JSON
// text (see jsonstrict): a *jsonstrict.Error when it is not, or the error of
// reading it. An empty body passes, for the handler to answer as it always has.
//
// A model request is read by several parsers before it leaves: the request
// stages decode it with encoding/json, the handler, the request guard and the
// translators read it with gjson, and the executors send it on to an upstream
// that parses it its own way. They disagree on a repeated member name and on
// text that is not quite JSON, so such a body is refused before any of them
// reads it, and every reader sees the same request.
//
// The answer is kept with the stored body: a body checked once is not read
// again, and one a stage replaced with SetRequestBody is checked anew. A body
// spilled to disk is read from there in pieces, never whole.
func CheckJSONRequestBody(c *gin.Context, limit int64) error {
	storage, err := GetRequestBodyStorage(c, limit)
	if err != nil || storage == nil || storage.Size() == 0 {
		return err
	}
	if checked, ok := c.Get(checkedJSONBodyKey); ok && checked == storage {
		return nil
	}
	if storage.IsDisk() {
		if _, err = storage.Seek(0, io.SeekStart); err != nil {
			return err
		}
		err = jsonstrict.CheckReader(storage)
		if restoreErr := restoreRequestBody(c, storage); err == nil {
			err = restoreErr
		}
	} else {
		var data []byte
		if data, err = storage.Bytes(); err == nil {
			err = jsonstrict.Check(data)
		}
	}
	if err != nil {
		return err
	}
	c.Set(checkedJSONBodyKey, storage)
	return nil
}

// RequestBodyProblem is how a model request is answered whose body could not
// be read or is not one strict JSON text: an HTTP status and the fields of the
// OpenAI-shaped error every model endpoint answers such a request with.
type RequestBodyProblem struct {
	Status  int
	Type    string
	Code    string
	Message string
}

// DescribeRequestBodyError says how to answer a model request whose body
// failed CheckJSONRequestBody or ReadRequestBody.
func DescribeRequestBodyError(err error) RequestBodyProblem {
	var invalid *jsonstrict.Error
	switch {
	case errors.As(err, &invalid) && invalid.Duplicate:
		// The name in the message is the client's own, cut short; nothing of a
		// string value is ever quoted (see jsonstrict.Error).
		return RequestBodyProblem{http.StatusBadRequest, "invalid_request_error", "duplicate_json_member", "Invalid request: " + invalid.Error()}
	case errors.As(err, &invalid):
		return RequestBodyProblem{http.StatusBadRequest, "invalid_request_error", "invalid_json", "Invalid request: the body is not valid JSON: " + invalid.Error()}
	case IsTooLarge(err):
		return RequestBodyProblem{http.StatusRequestEntityTooLarge, "invalid_request_error", "request_body_too_large", "Request body too large"}
	case IsStorageUnavailable(err):
		return RequestBodyProblem{http.StatusInsufficientStorage, "server_error", "request_body_storage_unavailable", "Request body temporary storage unavailable"}
	case IsTimeout(err):
		return RequestBodyProblem{http.StatusRequestTimeout, "request_timeout", "request_timeout", "Request timed out while reading the request body"}
	}
	return RequestBodyProblem{http.StatusBadRequest, "invalid_request_error", "", fmt.Sprintf("Invalid request: %v", err)}
}
