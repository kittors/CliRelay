package bodyutil

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/jsonstrict"
)

func jsonCheckContext(body string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	return c
}

// A stage that rewrites the body (the model policy, the system prompt) hands
// on a new body, and the new body is checked, not taken for the one checked
// before.
func TestCheckJSONRequestBodyChecksAReplacedBodyAgain(t *testing.T) {
	c := jsonCheckContext(`{"model":"m","messages":[]}`)
	if err := CheckJSONRequestBody(c, ModelRequestBodyLimit()); err != nil {
		t.Fatal(err)
	}
	if err := CheckJSONRequestBody(c, ModelRequestBodyLimit()); err != nil {
		t.Fatalf("checked again: %v", err)
	}
	SetRequestBody(c, []byte(`{"model":"m","model":"n","messages":[]}`))
	err := CheckJSONRequestBody(c, ModelRequestBodyLimit())
	if !jsonstrict.IsDuplicate(err) {
		t.Fatalf("a replaced body passed unchecked: %v", err)
	}
	// What was checked is still there to be read, for the error and the log.
	if body, readErr := ReadRequestBody(c, ModelRequestBodyLimit()); readErr != nil || string(body) != `{"model":"m","model":"n","messages":[]}` {
		t.Fatalf("body = %q, %v", body, readErr)
	}
}

func TestCheckJSONRequestBodyLetsAnEmptyBodyThrough(t *testing.T) {
	if err := CheckJSONRequestBody(jsonCheckContext(""), ModelRequestBodyLimit()); err != nil {
		t.Fatalf("an empty body: %v", err)
	}
}

func TestCheckJSONRequestBodyReportsAReadError(t *testing.T) {
	c := jsonCheckContext(`{"model":"` + strings.Repeat("x", 64) + `"}`)
	if err := CheckJSONRequestBody(c, 16); !IsTooLarge(err) {
		t.Fatalf("got %v", err)
	}
}

func TestDescribeRequestBodyError(t *testing.T) {
	duplicate := jsonstrict.Check([]byte(`{"messages":[],"messages":[]}`))
	invalid := jsonstrict.Check([]byte(`{"temperature":NaN}`))
	for _, tc := range []struct {
		err          error
		status       int
		code, inText string
	}{
		{duplicate, http.StatusBadRequest, "duplicate_json_member", `two members named "messages"`},
		{invalid, http.StatusBadRequest, "invalid_json", "not valid JSON"},
		{ErrBodyTooLarge, http.StatusRequestEntityTooLarge, "request_body_too_large", "too large"},
		{errors.New("boom"), http.StatusBadRequest, "", "boom"},
	} {
		got := DescribeRequestBodyError(tc.err)
		if got.Status != tc.status || got.Code != tc.code || !strings.Contains(got.Message, tc.inText) {
			t.Errorf("%v: %+v", tc.err, got)
		}
	}
}
