package workerproto

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestAPIRequestOnlyAddressesPrivateCoopRoutes(t *testing.T) {
	for _, path := range []string{"https://example.com/v1/sessions", "//example.com/v1/sessions", "/other", "/v1/../secrets", "/v1/%2e%2e/secrets", "/v1/a//b", "/v1/a%00b", "/v1/a%5cb"} {
		if (APIRequest{Method: "GET", Path: path}).Validate() == nil {
			t.Errorf("accepted %q", path)
		}
	}
	for _, path := range []string{"/v1/sessions", "/v1/sessions/s/events?after=5&limit=2", "/v1/sess%69ons"} {
		if err := (APIRequest{Method: "GET", Path: path}).Validate(); err != nil {
			t.Errorf("refused %q: %v", path, err)
		}
	}
}

func TestAPIRequestRejectsAmbiguousOrUnboundedBodiesAndAuthorityHeaders(t *testing.T) {
	ref := &BodyReference{SHA256: strings.Repeat("a", 64), ByteSize: 4}
	for _, request := range []APIRequest{
		{Method: "CONNECT", Path: "/v1/sessions"},
		{Method: "POST", Path: "/v1/sessions", Body: json.RawMessage(`{`)},
		{Method: "POST", Path: "/v1/sessions", Body: json.RawMessage(`{}`), BodyRef: ref},
		{Method: "GET", Path: "/v1/sessions", BodyRef: ref},
		{Method: "POST", Path: "/v1/sessions", Headers: map[string]string{"authorization": "secret"}},
		{Method: "POST", Path: "/v1/sessions", Headers: map[string]string{"idempotency-key": "substitute"}},
		{Method: "POST", Path: "/v1/sessions", Headers: map[string]string{"content-type": "json\r\ninjected:value"}},
	} {
		if request.Validate() == nil {
			t.Errorf("accepted %+v", request)
		}
	}
	for _, size := range []int64{-1, 0, math.MaxInt64} {
		if (BodyReference{SHA256: ref.SHA256, ByteSize: size}).Validate() == nil {
			t.Errorf("accepted size %d", size)
		}
	}
	if _, err := DecodeAPIRequest([]byte(`{"method":"POST","path":"/v1/sessions","token":"secret"}`)); err == nil {
		t.Fatal("accepted unknown authority")
	}
}
