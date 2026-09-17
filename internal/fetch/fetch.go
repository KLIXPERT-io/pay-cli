// Package fetch retrieves an arbitrary URL that is NOT the Payload server.
//
// §13 lets `pay upload` and `pay create` take a remote URL as the file source.
// That request must not go through internal/payload's transport: it would
// attach the profile's Authorization header and send the user's Payload
// credential to a third-party host.
//
// It therefore constructs its own *http.Request, which §3.1 otherwise confines
// to internal/payload/transport.go. scripts/arch-lint.sh exempts this package
// for the same reason it exempts internal/update: neither talks to Payload.
// The exemption is narrow on purpose — this package is a few dozen lines and
// cannot accumulate a Payload call by accident, which is exactly the risk of
// exempting a 1,600-line command file instead.
package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Result is an open body plus what the response said about it.
type Result struct {
	Body        io.ReadCloser
	ContentType string
	Length      int64
	StatusCode  int
}

// Get issues a GET for rawurl and returns the open body on 2xx.
//
// ctx is honoured: it bounds the whole exchange, so --deadline and Ctrl-C
// interrupt a slow or hanging remote host. (The previous implementation called
// http.Client.Get and discarded ctx, which made a stalled download ignore both.)
// The caller closes Body.
func Get(ctx context.Context, rt http.RoundTripper, rawurl string, timeout time.Duration) (*Result, error) {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	// No Authorization header is set, and none may be: rawurl is a host the
	// user named, not the profile's Payload server.
	client := &http.Client{Transport: rt, Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return &Result{StatusCode: resp.StatusCode}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return &Result{
		Body:        resp.Body,
		ContentType: resp.Header.Get("Content-Type"),
		Length:      resp.ContentLength,
		StatusCode:  resp.StatusCode,
	}, nil
}
