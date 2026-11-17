package sessionsvc

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The protocol fixture is the example a controller author copies, so its turn body must be one the
// turns route accepts. Its session does not exist here: decoding passes, and the lookup refuses it.
func TestProtocolFixtureTurnBodyDecodes(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "coop-worker-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Response struct {
			Commands []struct {
				Payload struct {
					Path string          `json:"path"`
					Body json.RawMessage `json:"body"`
				} `json:"payload"`
			} `json:"commands"`
		} `json:"response"`
	}
	if err := json.Unmarshal(document, &fixture); err != nil {
		t.Fatal(err)
	}
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	handler := NewHTTPHandler(service.Service)
	turns := 0
	for _, command := range fixture.Response.Commands {
		if !strings.HasSuffix(command.Payload.Path, "/turns") {
			continue
		}
		turns++
		response := sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions/fixture-missing/turns",
			string(command.Payload.Body), "fixture-turn", "application/json")
		if response.Code != http.StatusNotFound {
			t.Errorf("fixture turn body = %d %s, want the missing session's 404", response.Code, response.Body.String())
		}
	}
	if turns == 0 {
		t.Fatal("the fixture has no turn command")
	}
}
