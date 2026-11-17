package httpmail_test

import (
	"net/http"
	"net/http/httptest"
)

func recorder(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
