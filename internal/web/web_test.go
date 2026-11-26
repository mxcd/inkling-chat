package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

func TestTheFrontendAnswersDeepLinksAndRefusesLostAssets(t *testing.T) {
	t.Parallel()
	root := fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><title>app</title>")},
		"assets/main-1a.js":  {Data: []byte("console.log(1)")},
		"favicon.svg":        {Data: []byte("<svg/>")},
		"assets/nested/x.js": {Data: []byte("x")},
	}
	serve := handler(root)
	for _, test := range []struct {
		name, path   string
		status       int
		body, caches string
	}{
		{"the root", "/", http.StatusOK, "<!doctype html>", "no-cache"},
		{"a deep link", "/some/deep/link?item=2", http.StatusOK, "<!doctype html>", "no-cache"},
		{"a hashed asset", "/assets/main-1a.js", http.StatusOK, "console.log(1)", "immutable"},
		{"an asset of an older build", "/assets/main-0f.js", http.StatusNotFound, "404", ""},
		{"a file at the root", "/favicon.svg", http.StatusOK, "<svg/>", ""},
		{"a climb out of the root", "/../../etc/passwd", http.StatusOK, "<!doctype html>", "no-cache"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			recorder := httptest.NewRecorder()
			serve.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, test.path, nil))
			require.Equal(t, test.status, recorder.Code)
			require.Contains(t, recorder.Body.String(), test.body)
			require.Contains(t, recorder.Header().Get("Cache-Control"), test.caches)
		})
	}
}

func TestTheBinaryServesAFrontendBuiltOrNot(t *testing.T) {
	t.Parallel()
	serve, err := Handler()
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	serve.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "<!doctype html>")
}
