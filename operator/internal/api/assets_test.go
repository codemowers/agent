package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedUIAssets(t *testing.T) {
	routes := (&Handler{}).routes()
	for _, path := range []string{"/", "/?namespace=team&task=task-123", "/auth/callback", "/markdown.bundle.js", "/drafts.json", "/taglines.js", "/sidebar-resize.js"} {
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("GET %s: status %d, body length %d", path, response.Code, response.Body.Len())
		}
		if path == "/" && !strings.Contains(response.Body.String(), "/markdown.bundle.js") {
			t.Fatal("UI does not load bundled Markdown renderer")
		}
	}
}
