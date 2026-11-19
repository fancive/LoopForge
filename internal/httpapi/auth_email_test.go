package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"agentflow/internal/auth"
	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
)

func TestCompanyEmailRegistration(t *testing.T) {
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	t.Setenv("AGENTFLOW_ALLOW_SIGNUP", "true")
	t.Setenv("AGENTFLOW_SIGNUP_EMAIL_DOMAINS", "company.example")
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	deps := &deps{repo: store, authClient: auth.NewClientFromEnv()}
	handler := &authHandlers{deps: deps}
	server := gin.New()
	server.Use((&tokenHandlers{deps: deps}).userAccessAuth())
	server.GET("/api/auth/setup", handler.authSetup)
	server.POST("/api/auth/password", handler.authPassword)
	server.POST("/api/auth/register", handler.authRegister)
	server.POST("/api/auth/email-code", handler.authEmailCode)
	codes := map[string]string{}
	handler.sendSignupEmail = func(_ context.Context, address, code string) error {
		if strings.HasPrefix(address, "failed@") {
			return errors.New("SMTP failed")
		}
		codes[address] = code
		return nil
	}
	nextIP := 0
	call := func(method, path, origin string, body map[string]string, status int) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://localhost/api/auth/"+path, bytes.NewReader(data))
		nextIP++
		r.RemoteAddr = "192.0.2." + strconv.Itoa(nextIP) + ":12345"
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s: HTTP %d, want %d: %s", path, w.Code, status, w.Body.String())
		}
		return w
	}
	post := func(path string, body map[string]string, status int) *httptest.ResponseRecorder {
		t.Helper()
		return call("POST", path, "http://localhost", body, status)
	}
	const address = "owner@company.example"
	post("email-code", map[string]string{"email": address}, http.StatusForbidden)
	post("register", map[string]string{"login": "OWNER", "password": "initial-admin-password"}, http.StatusOK)
	setup := call("GET", "setup", "", nil, http.StatusOK)
	if !strings.Contains(setup.Body.String(), `"emailSignup":true`) {
		t.Fatal("setup did not advertise verified email signup")
	}
	req := map[string]string{"email": "Owner@COMPANY.EXAMPLE"}
	call("POST", "email-code", "https://wrong.example", req, http.StatusForbidden)
	post("email-code", map[string]string{"email": "developer@other.company.example"}, http.StatusBadRequest)
	post("email-code", map[string]string{"email": "failed@company.example"}, http.StatusServiceUnavailable)
	post("email-code", map[string]string{"email": "failed@company.example"}, http.StatusServiceUnavailable)
	post("email-code", req, http.StatusAccepted)
	code := codes[address]
	if !emailCodePattern.MatchString(code) {
		t.Fatal("fixture did not receive a six-digit code")
	}
	delete(codes, address)
	post("email-code", req, http.StatusAccepted)
	if len(codes) != 0 {
		t.Fatal("resend cooldown did not suppress a duplicate delivery")
	}
	body := map[string]string{"password": "new-local-password", "email": address}
	post("register", body, http.StatusBadRequest)
	body["code"] = "000000"
	if body["code"] == code {
		body["code"] = "000001"
	}
	post("register", body, http.StatusBadRequest)
	body["code"] = code
	registered := post("register", body, http.StatusOK)
	if len(registered.Result().Cookies()) != 1 || !strings.Contains(registered.Body.String(), `"admin":false`) {
		t.Fatal("verified registration did not create a regular signed-in member")
	}
	post("register", body, http.StatusBadRequest)
	member, found := store.UserByVerifiedEmail(address)
	owner, ownerFound := store.UserByLoginName("OWNER")
	if !found || !ownerFound || member.ID == owner.ID || store.IsPlatformAdmin(member.LoginName) {
		t.Fatal("a verified email must create a regular account even if its prefix matches an existing username")
	}
	post("password", map[string]string{"login": "OWNER@COMPANY.EXAMPLE", "password": "new-local-password"}, http.StatusOK)
	post("password", map[string]string{"login": "OWNER", "password": "initial-admin-password"}, http.StatusOK)

	req["email"] = "limited@company.example"
	post("email-code", req, http.StatusAccepted)
	body["email"] = req["email"]
	body["code"] = "000000"
	if body["code"] == codes[req["email"]] {
		body["code"] = "000001"
	}
	for range 5 {
		post("register", body, http.StatusBadRequest)
	}
	body["code"] = codes[req["email"]]
	post("register", body, http.StatusBadRequest)
}
