package payload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

// LoginInput is `pay auth login --jwt`'s credential (§5.0).
//
// Exactly one identifier is sent. Which one the collection wants is
// discoverable from mutation{Singular}Input, and when a collection accepts
// both (loginWithUsername) PayCLI refuses to guess.
type LoginInput struct {
	Collection string
	Email      string
	Username   string
	// Password is sent once and never stored, never logged and never written
	// to an error: only {token, token_exp} is persisted (§5.0).
	Password string
}

// LoginResult is the 200 body of POST /{collection}/login.
type LoginResult struct {
	Token string `json:"token"`
	// Exp is the JWT expiry in Unix seconds, as Payload reports it.
	Exp     int64  `json:"exp"`
	User    Doc    `json:"user"`
	Message string `json:"message"`

	HTTP *Response `json:"-"`
}

// ExpiresAt converts Exp to a time. A zero Exp yields the zero time.
func (l *LoginResult) ExpiresAt() time.Time {
	if l == nil || l.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(l.Exp, 0).UTC()
}

// Expired reports whether the token is expired, or expires within the given
// skew. `now` is passed in so this package never reads a clock; §5.0 uses a
// 60 s skew and re-logs-in BEFORE sending, which is what makes a refresh safe
// even for a write.
func (l *LoginResult) Expired(now time.Time, skew time.Duration) bool {
	if l == nil || l.Token == "" {
		return true
	}
	exp := l.ExpiresAt()
	if exp.IsZero() {
		return false // an expiry PayCLI never learned is not an expiry it invents
	}
	return !now.Add(skew).Before(exp)
}

// Login POSTs {api_path}/{collection}/login.
func (c *Client) Login(ctx context.Context, in LoginInput, opts ...Option) (*LoginResult, error) {
	in.Collection = resolvedAuthCollection(in.Collection)
	if in.Collection == "" {
		in.Collection = c.AuthCollection()
	}
	if in.Collection == "" {
		return nil, apierr.New(apierr.CodeAuthCollectionUnknown,
			"login needs the auth collection slug").
			WithHint("pass --auth-collection SLUG")
	}
	hasEmail := strings.TrimSpace(in.Email) != ""
	hasUser := strings.TrimSpace(in.Username) != ""
	switch {
	case hasEmail && hasUser:
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"--email and --username are mutually exclusive; %s accepts one identifier per login", in.Collection)
	case !hasEmail && !hasUser:
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"login needs an identifier: --email X or --username X").
			WithHint("when the collection accepts both (loginWithUsername), PayCLI will not guess — " +
				"a wrong guess is a clean 400 ValidationError naming the missing field")
	case in.Password == "":
		return nil, apierr.New(apierr.CodeAuthMissing, "no password was supplied").
			WithHint("pipe it in with --password-stdin; PayCLI never stores the password, only {token, token_exp}")
	}

	payload := map[string]any{"password": in.Password}
	if hasEmail {
		payload["email"] = in.Email
	} else {
		payload["username"] = in.Username
	}
	body, err := jsonBody(payload)
	if err != nil {
		return nil, err
	}

	req := applyOptions(&Request{
		Method:      http.MethodPost,
		Path:        "/" + url.PathEscape(in.Collection) + "/login",
		Body:        body,
		ContentType: "application/json",
		NoOverride:  true,
		// The request body holds a password: it is never echoed into an error
		// and never retried, because a login is not idempotency-safe.
		Classify: ClassifyContext{Collection: in.Collection},
	}, opts)

	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Token   string      `json:"token"`
		Exp     json.Number `json:"exp"`
		User    Doc         `json:"user"`
		Message string      `json:"message"`
	}
	if err := decodeJSON(resp, &parsed); err != nil {
		return nil, err
	}
	if parsed.Token == "" {
		return nil, apierr.New(apierr.CodeAuthInvalid,
			"the server accepted the login request but returned no token").
			WithHint("check the identifier field: a collection with loginWithUsername wants --username, not --email")
	}
	out := &LoginResult{Token: parsed.Token, User: parsed.User, Message: parsed.Message, HTTP: resp}
	if parsed.Exp != "" {
		if n, convErr := parsed.Exp.Int64(); convErr == nil {
			out.Exp = n
		}
	}
	return out, nil
}

// RefreshToken POSTs {api_path}/{collection}/refresh-token using the client's
// current JWT, which is how §5.0's single re-login is performed without a
// password when the token is merely stale.
func (c *Client) RefreshToken(ctx context.Context, collection string, opts ...Option) (*LoginResult, error) {
	collection = resolvedAuthCollection(collection)
	if collection == "" {
		collection = c.AuthCollection()
	}
	if collection == "" {
		return nil, apierr.New(apierr.CodeAuthCollectionUnknown,
			"refreshing a token needs the auth collection slug").
			WithHint("pass --auth-collection SLUG")
	}
	req := applyOptions(&Request{
		Method:     http.MethodPost,
		Path:       "/" + url.PathEscape(collection) + "/refresh-token",
		NoOverride: true,
	}, opts)
	req.Classify.Collection = collection

	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	// Payload names the field refreshedToken; decode generically so a version
	// that renames it still works.
	var generic map[string]any
	if err := decodeJSON(resp, &generic); err != nil {
		return nil, err
	}
	out := &LoginResult{HTTP: resp}
	for _, key := range []string{"refreshedToken", "token"} {
		if s, ok := generic[key].(string); ok && s != "" {
			out.Token = s
			break
		}
	}
	if n, ok := generic["exp"].(json.Number); ok {
		if v, convErr := n.Int64(); convErr == nil {
			out.Exp = v
		}
	}
	if u, ok := generic["user"].(map[string]any); ok {
		out.User = Doc(u)
	}
	if out.Token == "" {
		return nil, apierr.New(apierr.CodeAuthInvalid, "the server did not refresh the token").
			WithHint("log in again: pay auth login --jwt --email … --password-stdin")
	}
	return out, nil
}

// Logout POSTs {api_path}/{collection}/logout. Failing to log out server-side
// is not fatal — the local credential is what PayCLI controls — so callers
// treat the error as a warning.
func (c *Client) Logout(ctx context.Context, collection string, opts ...Option) error {
	collection = resolvedAuthCollection(collection)
	if collection == "" {
		collection = c.AuthCollection()
	}
	if collection == "" {
		return apierr.New(apierr.CodeAuthCollectionUnknown,
			"logging out needs the auth collection slug").
			WithHint("pass --auth-collection SLUG")
	}
	req := applyOptions(&Request{
		Method:     http.MethodPost,
		Path:       "/" + url.PathEscape(collection) + "/logout",
		NoOverride: true,
	}, opts)
	req.Classify.Collection = collection
	_, err := c.Do(ctx, req)
	return err
}
