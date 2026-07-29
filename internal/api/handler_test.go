package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

func TestWriteJSONUsesUnescapedEncoding(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeJSON(recorder, http.StatusCreated, map[string]any{"html": "<b>你好</b> & ok"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusCreated)
	}
	if got, want := recorder.Body.String(), `{"html":"<b>你好</b> & ok"}`; got != want {
		t.Fatalf("body=%q, want %q", got, want)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content type=%q", got)
	}
}

func TestWriteJSONSerializationFailureDoesNotCommitRequestedStatus(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	recorder := httptest.NewRecorder()
	writeJSON(recorder, http.StatusCreated, cyclic)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"code":500`) ||
		!strings.Contains(body, "序列化失败") {
		t.Fatalf("unexpected serialization error body=%q", body)
	}
}

func TestPublicVertexErrorsRedactProjectIDsAndBoundUpstreamDetails(t *testing.T) {
	upstreamErr := vertex.NewNotFoundError(
		"Publisher model `projects/private-project-123/locations/global/models/missing` was not found",
	)
	upstreamErr.UpstreamResponse = `{"responseContext":{"token":"private-response-token"}}`

	gemini := vertexErrorToGemini(upstreamErr)
	geminiMessage := gemini["error"].(map[string]any)["message"].(string)
	if strings.Contains(geminiMessage, "private-project-123") ||
		strings.Contains(geminiMessage, "private-response-token") {
		t.Fatalf("Gemini error leaked upstream identifiers: %q", geminiMessage)
	}
	if !strings.Contains(geminiMessage, "projects/<redacted>/locations/global/models/missing") {
		t.Fatalf("Gemini error lost useful sanitized detail: %q", geminiMessage)
	}

	oai := vertexErrorToOAI(upstreamErr)
	oaiMessage := oai["error"].(map[string]any)["message"].(string)
	if strings.Contains(oaiMessage, "private-project-123") {
		t.Fatalf("OpenAI error leaked upstream project ID: %q", oaiMessage)
	}
}
