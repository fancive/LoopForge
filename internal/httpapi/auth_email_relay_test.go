package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSignupEmailHTTPSRelay(t *testing.T) {
	ctx := context.Background()
	relay := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.ParseMultipartForm(4096) != nil {
			t.Error("expected a multipart POST")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.FormValue("mailFrom") != "test-send-token" ||
			r.FormValue("receiver") != "runner@company.example" ||
			r.FormValue("channel") != "email" ||
			r.FormValue("mailHtml") != "false" ||
			r.FormValue("mailSubject") != signupEmailSubject ||
			r.FormValue("content") != signupEmailBody("034827") {
			t.Error("relay did not receive the expected mail fields")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		replies := map[string]string{
			"/message/send": `{"code":200,"msg":"success","result":null}`,
			"/flag-only":    `{"success":true}`,
			"/failed":       `{"success":false}`,
			"/bad-request":  `{"code":400,"success":true}`,
		}
		_, _ = io.WriteString(w, replies[r.URL.Path])
	}))
	defer relay.Close()
	send := func(path string) error {
		return sendSignupCodeByRelay(ctx, relay.Client(), relay.URL+path, "test-send-token", "runner@company.example", "034827")
	}
	for _, path := range []string{"/message/send", "/flag-only"} {
		if err := send(path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	for _, path := range []string{"/failed", "/bad-request"} {
		if err := send(path); err == nil {
			t.Fatalf("%s: HTTP 200 with a failed delivery was accepted", path)
		}
	}
	if err := sendSignupCodeByRelay(ctx, relay.Client(), "http://relay.example/message/send", "test-send-token", "runner@company.example", "034827"); err == nil {
		t.Fatal("cleartext relay was accepted")
	}
}
