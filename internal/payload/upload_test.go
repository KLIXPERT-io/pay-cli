package payload

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
)

func parseUpload(t *testing.T, req recorded) (fileName, fileType, fileBody, fieldsJSON string) {
	t.Helper()
	_, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type = %q: %v", req.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(strings.NewReader(req.Body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		body, _ := io.ReadAll(part)
		switch part.FormName() {
		case UploadPartFile:
			fileName = part.FileName()
			fileType = part.Header.Get("Content-Type")
			fileBody = string(body)
		case UploadPartFields:
			fieldsJSON = string(body)
		default:
			t.Fatalf("unexpected part %q", part.FormName())
		}
	}
	return
}

func TestUploadBuildsTheVerifiedMultipartShape(t *testing.T) {
	s := newStub(t, jsonHandler(201, `{"doc":{"id":5,"filename":"paycli-probe-1.png","alt":"A"},"message":"Media successfully created."}`))
	c, _ := newTestClient(t, s, nil)

	res, err := c.Upload(context.Background(), UploadInput{
		Collection:  "media",
		Filename:    "paycli-probe.png",
		ContentType: "image/png",
		Body:        strings.NewReader("PNGBYTES"),
		Fields:      map[string]any{"alt": "A"},
		Params:      query.Params{Depth: query.IntPtr(0)},
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if idToString(res.Doc.ID()) != "5" {
		t.Fatalf("doc = %v", res.Doc)
	}

	seen := s.last()
	if seen.Method != http.MethodPost || seen.Path != "/api/media" {
		t.Fatalf("%s %s", seen.Method, seen.Path)
	}
	name, ctype, body, fields := parseUpload(t, seen)
	// The part name must be literally "file": `-F upload=@…` answers 400
	// "No files were uploaded." (verified).
	if name != "paycli-probe.png" {
		t.Fatalf("filename = %q", name)
	}
	if ctype != "image/png" {
		t.Fatalf("part content type = %q", ctype)
	}
	if body != "PNGBYTES" {
		t.Fatalf("file body = %q", body)
	}
	if fields != `{"alt":"A"}` {
		t.Fatalf("_payload = %q", fields)
	}

	// The server auto-renames on a collision, so the caller must be able to
	// see that its filename was not kept.
	got, changed := FilenameChanged("paycli-probe.png", res.Doc)
	if !changed || got != "paycli-probe-1.png" {
		t.Fatalf("FilenameChanged = %q, %v", got, changed)
	}
}

func TestUploadOmitsThePayloadPartWhenThereAreNoFields(t *testing.T) {
	s := newStub(t, jsonHandler(201, `{"doc":{"id":1}}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.Upload(context.Background(), UploadInput{
		Collection: "media", Filename: "a.bin", Body: strings.NewReader("x"),
	}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	_, ctype, _, fields := parseUpload(t, s.last())
	if fields != "" {
		t.Fatalf("_payload should be absent, got %q", fields)
	}
	if ctype != "application/octet-stream" {
		t.Fatalf("default part type = %q", ctype)
	}
}

func TestUploadReplaceIsAPatchOnTheID(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{"doc":{"id":5,"filename":"new.png"}}`))
	c, _ := newTestClient(t, s, nil)
	if _, err := c.Upload(context.Background(), UploadInput{
		Collection: "media", Filename: "new.png", Body: strings.NewReader("x"), ReplaceID: "5",
	}); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if s.last().Method != http.MethodPatch || s.last().Path != "/api/media/5" {
		t.Fatalf("%s %s", s.last().Method, s.last().Path)
	}
}

func TestUploadValidation(t *testing.T) {
	s := newStub(t, jsonHandler(200, `{}`))
	c, _ := newTestClient(t, s, nil)
	tests := []struct {
		name string
		in   UploadInput
		code apierr.Code
	}{
		{"no collection", UploadInput{Filename: "a", Body: strings.NewReader("x")}, apierr.CodeInvalidArgs},
		{"no bytes", UploadInput{Collection: "media", Filename: "a"}, apierr.CodeFileMissing},
		{"no filename", UploadInput{Collection: "media", Body: strings.NewReader("x")}, apierr.CodeInvalidArgs},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Upload(context.Background(), tc.in); !apierr.HasCode(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
	if s.count() != 0 {
		t.Fatal("a rejected upload must not reach the network")
	}
}

func TestCheckUploadCollectionIsTriState(t *testing.T) {
	yes, no := true, false
	if err := CheckUploadCollection("media", &yes); err != nil {
		t.Fatalf("upload collection: %v", err)
	}
	if err := CheckUploadCollection("pages", &no); !apierr.HasCode(err, apierr.CodeNotUploadCollection) {
		t.Fatalf("err = %v, want not_upload_collection", err)
	}
	if err := CheckUploadCollection("pages", nil); err != nil {
		t.Fatalf("an unknown flag must not reject locally: %v", err)
	}
}

func TestUploadErrorIsFileMissing(t *testing.T) {
	s := newStub(t, jsonHandler(400, `{"errors":[{"message":"No files were uploaded."}]}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.Upload(context.Background(), UploadInput{
		Collection: "media", Filename: "a.png", Body: strings.NewReader("x"),
	})
	if !apierr.HasCode(err, apierr.CodeFileMissing) {
		t.Fatalf("err = %v, want file_missing", err)
	}
}

func TestDownloadStreams(t *testing.T) {
	s := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = io.WriteString(w, "BYTES")
	})
	c, _ := newTestClient(t, s, nil)
	var out strings.Builder
	resp, err := c.Download(context.Background(), "media", "paycli-probe.png", &out)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if out.String() != "BYTES" {
		t.Fatalf("downloaded %q", out.String())
	}
	if s.last().Path != "/api/media/file/paycli-probe.png" {
		t.Fatalf("path = %s", s.last().Path)
	}
	if resp.Bytes != 5 {
		t.Fatalf("Bytes = %d", resp.Bytes)
	}
}

func TestEscapeQuotes(t *testing.T) {
	if got := escapeQuotes(`a"b\c` + "\r\n"); got != `a\"b\\c` {
		t.Fatalf("escapeQuotes = %q", got)
	}
}

func TestUploadIsNeverRetriedOrPromoted(t *testing.T) {
	s := newStub(t, jsonHandler(503, `{}`))
	c, _ := newTestClient(t, s, nil)
	_, err := c.Upload(context.Background(), UploadInput{
		Collection: "media", Filename: "a.png", Body: strings.NewReader("x"),
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if s.count() != 1 {
		t.Fatalf("a streaming multipart body was replayed %d times", s.count()-1)
	}
}

func TestLimitReader(t *testing.T) {
	r := LimitReader(strings.NewReader("0123456789"), 4)
	got, err := io.ReadAll(r)
	if !apierr.HasCode(err, apierr.CodeRequestTooLarge) {
		t.Fatalf("err = %v, want request_too_large (read %q)", err, got)
	}
	all, err := io.ReadAll(LimitReader(strings.NewReader("abc"), 0))
	if err != nil || string(all) != "abc" {
		t.Fatalf("an unlimited reader must pass everything: %q, %v", all, err)
	}
	exact, err := io.ReadAll(LimitReader(strings.NewReader("abc"), 3))
	if err != nil || string(exact) != "abc" {
		t.Fatalf("an exactly-sized file must succeed: %q, %v", exact, err)
	}
}
