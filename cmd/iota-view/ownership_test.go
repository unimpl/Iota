package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	iota "github.com/unimpl/Iota"
)

func TestViewerCannotDeleteAnOpenSession(t *testing.T) {
	folder := t.TempDir()
	session, err := iota.NewSession(folder, iota.SessionInfo{Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	handler := newHandler(folder)
	for _, status := range []int{http.StatusConflict, http.StatusNoContent} {
		if status == http.StatusNoContent {
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
		}
		request := httptest.NewRequest(http.MethodDelete, "/api/sessions?name="+filepath.Base(session.Path()), nil)
		request.Header.Set("X-Iota-Delete", "1")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("delete status=%d want=%d body=%s", response.Code, status, response.Body.String())
		}
	}
}
