package sessionsvc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// sessionHTTPJSONRoutes is every route that reads a JSON body, keyed by the handler method that
// decodes it. TestSessionHTTPJSONRoutesRefuseUnknownFields checks this list against http.go, so a
// new body-reading handler cannot ship without joining it.
var sessionHTTPJSONRoutes = []struct{ handler, path string }{
	{"createSession", "/v1/sessions"},
	{"submitTurn", "/v1/sessions/unknown-field/turns"},
	{"extendBudget", "/v1/sessions/unknown-field/budget"},
	{"prepareSession", "/v1/sessions/unknown-field/prepare"},
	{"ensureWorkspaceTask", "/v1/sessions/unknown-field/workspace"},
	{"checkpointWorkspace", "/v1/sessions/unknown-field/checkpoint"},
	{"review", "/v1/sessions/unknown-field/review"},
	{"publishReview", "/v1/sessions/unknown-field/reviews/review-1/publish"},
	{"closeSession", "/v1/sessions/unknown-field/close"},
	{"planDiscard", "/v1/sessions/unknown-field/discard-plan"},
	{"discard", "/v1/sessions/unknown-field/discard"},
	{"cancelTurn", "/v1/sessions/unknown-field/turns/turn-1/cancel"},
	{"validateTurnCandidate", "/v1/sessions/unknown-field/turns/turn-1/validation"},
	{"fenceOperation", "/v1/operations/fence"},
}

// An unknown field can carry a bearer token past a caller that thinks the field was retired, so
// every JSON route refuses one before it acts. The control body {} decodes on every route and
// fails later for its own reason; only the decoder's refusal says "request body is invalid JSON",
// so a route that stops refusing unknown fields answers like the control and fails here.
func TestSessionHTTPJSONRoutesRefuseUnknownFields(t *testing.T) {
	if got, want := sessionHTTPBodyHandlers(t), sessionHTTPJSONRouteHandlers(); !slices.Equal(got, want) {
		t.Fatalf("handlers that decode a JSON body = %v, table covers %v", got, want)
	}
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	handler := NewHTTPHandler(service.Service)
	const refused = "request body is invalid JSON"
	for _, route := range sessionHTTPJSONRoutes {
		control := sessionHTTPTestRequest(t, handler, http.MethodPost, route.path, `{}`, "unknown-field-control", "application/json")
		if strings.Contains(control.Body.String(), refused) {
			t.Fatalf("%s refused an empty body as invalid JSON: %d %s", route.path, control.Code, control.Body.String())
		}
		response := sessionHTTPTestRequest(t, handler, http.MethodPost, route.path, `{"retired_token":"secret"}`, "unknown-field", "application/json")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), refused) {
			t.Fatalf("%s (%s) accepted an unknown field: %d %s", route.path, route.handler, response.Code, response.Body.String())
		}
	}
	// The fence carries the real request inside its envelope; that inner value is just as strict.
	for _, method := range []string{"CreateRemoteSession", "SubmitTurn"} {
		body := `{"method":"` + method + `","request":{"retired_token":"secret"}}`
		response := sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/operations/fence", body, "unknown-field-"+method, "application/json")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "fenced request is invalid") {
			t.Fatalf("fenced %s accepted an unknown field: %d %s", method, response.Code, response.Body.String())
		}
	}
}

func sessionHTTPJSONRouteHandlers() []string {
	var names []string
	for _, route := range sessionHTTPJSONRoutes {
		names = append(names, route.handler)
	}
	slices.Sort(names)
	return names
}

// sessionHTTPBodyHandlers lists the sessionHTTPHandler methods in http.go that decode JSON, through
// the strict helpers or encoding/json directly.
func sessionHTTPBodyHandlers(t *testing.T) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "http.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if ident, ok := star.X.(*ast.Ident); !ok || ident.Name != "sessionHTTPHandler" {
			continue
		}
		decodes := false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch callee := call.Fun.(type) {
			case *ast.Ident:
				decodes = decodes || strings.HasPrefix(callee.Name, "decodeSessionJSON")
			case *ast.SelectorExpr:
				if pkg, ok := callee.X.(*ast.Ident); ok && pkg.Name == "json" {
					decodes = decodes || callee.Sel.Name == "NewDecoder" || callee.Sel.Name == "Unmarshal"
				}
			}
			return true
		})
		if decodes {
			names = append(names, fn.Name.Name)
		}
	}
	slices.Sort(names)
	return names
}
