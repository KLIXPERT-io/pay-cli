package payload

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

func TestLoginPostsTheIdentifierAndPassword(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"exp":1789000000,"token":"jwt.token.value","user":{"id":1},"message":"Auth Passed"}`))
	c, _ := newTestClient(t, s, nil)
	res, err := c.Login(context.Background(), LoginInput{Collection: "users", Email: "a@b.test", Password: "hunter2"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.Token != "jwt.token.value" || res.Exp != 1789000000 {
		t.Fatalf("result = %+v", res)
	}
	if !res.ExpiresAt().Equal(time.Unix(1789000000, 0).UTC()) {
		t.Fatalf("ExpiresAt = %s", res.ExpiresAt())
	}
	seen := s.last()
	if seen.Method != http.MethodPost || seen.Path != "/api/users/login" {
		t.Fatalf("%s %s", seen.Method, seen.Path)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(seen.Body), &body); err != nil {
		t.Fatalf("body = %s", seen.Body)
	}
	if body["email"] != "a@b.test" || body["password"] != "hunter2" {
		t.Fatalf("body = %v", body)
	}
	if _, hasUsername := body["username"]; hasUsername {
		t.Fatal("only one identifier may be sent")
	}
}

func TestLoginUsername(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"token":"t","exp":1}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.Login(context.Background(), LoginInput{Collection: "users", Username: "ada", Password: "p"}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !strings.Contains(s.last().Body, `"username":"ada"`) {
		t.Fatalf("body = %s", s.last().Body)
	}
}

func TestLoginRefusesToGuessTheIdentifier(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"token":"t"}`))
	c, _ := newTestClient(t, s, nil)
	tests := []struct {
		name string
		in   LoginInput
		code apierr.Code
	}{
		{"both", LoginInput{Collection: "users", Email: "a", Username: "b", Password: "p"}, apierr.CodeInvalidArgs},
		{"neither", LoginInput{Collection: "users", Password: "p"}, apierr.CodeInvalidArgs},
		{"no password", LoginInput{Collection: "users", Email: "a"}, apierr.CodeAuthMissing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Login(context.Background(), tc.in); !apierr.HasCode(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
	if s.count() != 0 {
		t.Fatal("a rejected login must not reach the network")
	}
}

func TestLoginWithoutATokenIsAuthInvalid(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"user":null,"message":"Account"}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.Login(context.Background(), LoginInput{Collection: "users", Email: "a", Password: "p"})
	if !apierr.HasCode(err, apierr.CodeAuthInvalid) {
		t.Fatalf("err = %v, want auth_invalid", err)
	}
}

func TestLoginErrorNeverEchoesThePassword(t *testing.T) {
	s := newStub(t, jsonHandler(400, `{"errors":[{"name":"ValidationError","data":{"collection":"users","errors":[`+
		`{"label":"Email","message":"This field is required.","path":"email"}]},"message":"x"}]}`))
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.IdentityVerified = true })
	_, err := c.Login(context.Background(), LoginInput{Collection: "users", Email: "a@b.test", Password: "hunter2"},
		WithClassify(ClassifyContext{Collection: "users", IncludeRaw: true}))
	if err == nil {
		t.Fatal("want an error")
	}
	e, _ := apierr.As(err)
	blob, _ := json.Marshal(e)
	if strings.Contains(string(blob), "hunter2") {
		t.Fatalf("the password leaked into the error: %s", blob)
	}
}

func TestLoginIsNeverRetried(t *testing.T) {
	s := newStub(t, jsonHandler(503, `{}`))
	c, _ := newTestClient(t, s, nil)
	_, _ = c.Login(context.Background(), LoginInput{Collection: "users", Email: "a", Password: "p"})
	if s.count() != 1 {
		t.Fatalf("a login was retried %d times", s.count()-1)
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		res  *LoginResult
		want bool
	}{
		{"nil", nil, true},
		{"no token", &LoginResult{}, true},
		{"fresh", &LoginResult{Token: "t", Exp: now.Add(time.Hour).Unix()}, false},
		{"expired", &LoginResult{Token: "t", Exp: now.Add(-time.Hour).Unix()}, true},
		{"inside the skew", &LoginResult{Token: "t", Exp: now.Add(30 * time.Second).Unix()}, true},
		{"unknown expiry", &LoginResult{Token: "t"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.Expired(now, 60*time.Second); got != tc.want {
				t.Fatalf("Expired = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRefreshToken(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"refreshedToken":"new.jwt.value","exp":1789000000,"user":{"id":1}}`))
	c, _ := newTestClient(t, s, func(cfg *Config) { cfg.AuthMode = AuthModeJWT; cfg.Credential = "old" })
	res, err := c.RefreshToken(context.Background(), "users")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if res.Token != "new.jwt.value" || res.Exp != 1789000000 {
		t.Fatalf("result = %+v", res)
	}
	if s.last().Path != "/api/users/refresh-token" {
		t.Fatalf("path = %s", s.last().Path)
	}
}

func TestRefreshTokenWithoutAToken(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"message":"no"}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.RefreshToken(context.Background(), "users"); !apierr.HasCode(err, apierr.CodeAuthInvalid) {
		t.Fatalf("err = %v, want auth_invalid", err)
	}
}

func TestLogout(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"message":"You have been logged out successfully."}`))
	c, _ := newTestClient(t, s, nil)
	if err := c.Logout(context.Background(), "users"); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if s.last().Path != "/api/users/logout" || s.last().Method != http.MethodPost {
		t.Fatalf("%s %s", s.last().Method, s.last().Path)
	}
}
