package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// newDownloadCmd builds `pay download` (§13).
func init() { Register(newDownloadCmd) }

func newDownloadCmd(rt *Runtime) *cobra.Command {
	var (
		filename string
		out      string
		size     string
		force    bool
	)
	cmd := &cobra.Command{
		GroupID: GroupFiles,
		Use:     "download <collection> [id]",
		Short:   "Download the bytes of an upload document",
		Long: `Download a stored file.

The route is GET /{collection}/file/{filename}, which serves raw bytes. Give an
id and PayCLI resolves the filename from the document first; give --filename to
skip that read. --size NAME resolves sizes.<name>.filename on the document, so
--size thumbnail downloads the generated thumbnail rather than the original.

A missing file answers HTTP 500 rather than 404 on this route, so a "server
error" here usually means "no such filename".

The destination is never chosen by the server: without -o the file lands in the
working directory under the *basename* of the stored filename, a name that
already exists is refused unless --force is given, and the bytes are written
through a temp file so a failed download cannot truncate a file you already had.

`,
		Args: cobra.RangeArgs(1, 2),
	}
	cmd.RunE = runData(rt, safety.CmdDownload, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		{
			cfg := d.cfg()
			client, err := d.requireClient()
			if err != nil {
				return nil, err
			}
			t, err := d.collection(ctx, args[0])
			if err != nil {
				return nil, err
			}
			if err := payload.CheckUploadCollection(t.Slug, t.flags().Upload); err != nil {
				return nil, err
			}
			id := ""
			if len(args) == 2 {
				id = args[1]
			}
			if id == "" && filename == "" {
				return nil, apierr.New(apierr.CodeInvalidArgs,
					"give a document id or --filename NAME").
					WithHint("pay download %s <id>, or pay download %s --filename hero.png", t.Slug, t.Slug)
			}

			run := d.beginRun()
			var doc payload.Doc
			if id != "" {
				if e := d.checkID(t, t.Slug, id); e != nil {
					return nil, e
				}
				doc, _, err = client.Get(ctx, t.Slug, id, query.Params{Depth: query.IntPtr(0)},
					payload.WithClassify(classifyFor(t, cfg, nil)), payload.WithKnownRoute())
				if err != nil {
					return nil, err
				}
			}
			name, err := resolveDownloadName(doc, filename, size)
			if err != nil {
				return nil, err
			}

			// The remote name stays whole for the request path; the local
			// destination is derived from it only through safeLocalName, so a
			// server-controlled "../../.bashrc" can never become a local path
			// (§13, finding 13).
			dest := out
			toStdout := out == "-"
			derived := out == ""
			if derived {
				dest, err = safeLocalName(name)
				if err != nil {
					return nil, err
				}
			}
			if !toStdout {
				dest = resolveDownloadDest(d, dest)
				if derived && !force {
					if err := refuseToClobber(dest); err != nil {
						return nil, err
					}
				}
			}
			sink, err := openDownloadSink(d, dest, toStdout)
			if err != nil {
				return nil, err
			}

			resp, err := client.Download(ctx, t.Slug, name, sink,
				payload.WithClassify(classifyFor(t, cfg, nil)))
			if err != nil {
				sink.abort()
				return nil, err
			}
			if err := sink.commit(); err != nil {
				return nil, err
			}

			data := map[string]any{
				"filename":     name,
				"bytes":        resp.Bytes,
				"content_type": resp.ContentType,
				"path":         dest,
			}
			if toStdout {
				data["path"] = "-"
				// The file's bytes now own stdout. Appending the envelope to
				// them produces a file that is neither the download nor valid
				// JSON — a 463-byte PNG came out 1570 bytes long, with the
				// envelope glued to its tail — so the envelope is moved to the
				// error stream. The comment here used to claim --errors-to
				// already did this; it does not, because this envelope is a
				// SUCCESS.
				if d != nil && d.RT != nil && d.RT.Out != nil {
					d.RT.Out.StdoutIsData = true
				}
			}
			env := output.New(safety.CmdDownload, output.KindOpResult, data).
				WithTarget(t.envTarget(doc.ID())).
				WithMeta(run.meta())
			return env, nil
		}
	})
	cmd.Flags().StringVar(&filename, "filename", "", "download this stored filename directly")
	cmd.Flags().StringVarP(&out, "out", "o", "", "write to this path, or - for stdout")
	cmd.Flags().StringVar(&size, "size", "", "download a generated size (thumbnail, card, …) instead of the original")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file when the destination comes from the server's filename")

	SetHelp(cmd, downloadHelp())
	return cmd
}

// downloadHelp is §10.5's model for `pay download`.
func downloadHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay download <collection> <id> [-o PATH|-] [--size NAME] [--force]",
			"pay download <collection> --filename NAME [-o PATH|-]",
		},
		Collections: true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "media"},
			{Name: "id", Required: false, Type: "string", Example: "4  (omit it when you pass --filename)"},
		},
		FlagInfo: map[string]FlagInfo{
			"out":      {Grammar: "a path, or - for stdout"},
			"size":     {Note: "resolves sizes.<name>.filename on the document (thumbnail, card, …)"},
			"filename": {Note: "skips the document read and hits /{collection}/file/{filename} directly"},
			"force":    {Note: "only needed when the destination came from the server's filename"},
		},
		Output: OutputSpec{
			Kind:     output.KindOpResult,
			Skeleton: `{"bytes":463,"content_type":"image/png","filename":"paycli-probe-1.png","path":"./paycli-probe-1.png"}`,
		},
		ExitCodes: ReadExitCodes,
		Examples: []Example{
			{Why: "find something to download first",
				Cmd: "pay find media --limit 3 --select id,filename,mimeType"},
			{Why: "by id; the file lands under its stored basename in the working directory",
				Cmd: "pay download media 4"},
			{Why: "choose the destination explicitly",
				Cmd: "pay download media 4 -o ./hero.png"},
			{Why: "a generated size; exit 10 when that size was never generated for this file",
				Cmd: "pay download media 4 --size thumbnail -o ./thumb.png"},
			{Why: "write the bytes to stdout; the envelope goes to stderr, so a redirect is exactly the file",
				Cmd: "pay download media 4 -o - > hero.png"},
			{Why: "skip the document read when you already know the stored filename",
				Cmd: "pay download media --filename paycli-probe-1.png -o ./hero.png"},
		},
		Mistakes: []Mistake{
			{Wrong: "Reading a \"server error\" on this route as a server problem.",
				Right: "A missing file answers HTTP 500 rather than 404 here. It usually means no such filename — check with `pay get <collection> <id> --select filename,sizes`."},
			{Wrong: "Passing the filename you uploaded instead of the one that was stored.",
				Right: "Payload renames collisions server-side. The document's `filename` field is authoritative; --size reads sizes.<name>.filename."},
			{Wrong: "Expecting an existing file to be overwritten silently.",
				Right: "A destination taken from the server's filename is refused if it already exists; pass --force. An explicit -o path is yours to choose."},
			{Wrong: "Parsing the envelope from stdout when you passed `-o -`.",
				Right: "With -o - the raw bytes own stdout and the op_result envelope is written to STDERR instead, so `-o - > file` yields exactly the stored bytes. Read the envelope with 2>meta.json, or download to a real path with -o PATH."},
		},
		SeeAlso: []string{
			"pay upload <collection> <file>   # the other direction",
			"pay get <collection> <id> --select filename,url,sizes",
			"pay collections --kind upload",
		},
	}
}

// resolveDownloadName picks the filename, honouring --size via sizes.<n>.filename.
func resolveDownloadName(doc payload.Doc, filename, size string) (string, error) {
	if filename != "" && size == "" {
		return filename, nil
	}
	if size != "" {
		if doc == nil {
			return "", apierr.New(apierr.CodeInvalidArgs,
				"--size needs a document id: the generated sizes live on the document").
				WithHint("pay download <collection> <id> --size %s", size)
		}
		sizes, ok := doc["sizes"].(map[string]any)
		if !ok {
			return "", apierr.New(apierr.CodeFeatureUnavailable,
				"this document has no generated sizes").
				WithHint("drop --size to download the original")
		}
		entry, ok := sizes[size].(map[string]any)
		if !ok {
			return "", apierr.New(apierr.CodeInvalidOption,
				"%q is not one of this document's sizes", size).
				WithDidYouMean(apierr.DidYouMean(size, sortedKeysOf(sizes))...).
				WithHint("%s", "sizes: "+strings.Join(sortedKeysOf(sizes), ", "))
		}
		name, _ := entry["filename"].(string)
		if name == "" {
			return "", apierr.New(apierr.CodeFeatureUnavailable,
				"size %q exists on this document but has no filename (the size was never generated)", size).
				WithHint("drop --size to download the original")
		}
		return name, nil
	}
	name, _ := doc["filename"].(string)
	if name == "" {
		return "", apierr.New(apierr.CodeFileMissing,
			"this document has no stored filename").
			WithHint("pass --filename NAME explicitly")
	}
	return name, nil
}

// safeLocalName reduces a *server-controlled* filename to something that can
// only ever name a file in the working directory.
//
// GET /{coll}/file/{filename} echoes whatever the document says, so `filename`
// is attacker-controlled input for any project where a user can upload or a
// proxy can rewrite. Refusing anything that is not already a bare basename —
// a separator, "..", an absolute path, a Windows volume — is what keeps
// `pay download`, an L0 command that never prompts, from creating or
// truncating a file outside the directory the user is standing in (§13).
func safeLocalName(remote string) (string, error) {
	unusable := func() error {
		return apierr.New(apierr.CodeInvalidArgs,
			"the server returned an unusable filename %q", remote).
			WithHint("pass -o PATH to choose the destination yourself")
	}
	// A path separator is *rejected*, not quietly stripped: a real Payload
	// filename never contains one, so silently rewriting "../../../.bashrc"
	// into ".bashrc" would hide the attack instead of reporting it. The
	// backslash is included because a hostile server can send Windows
	// separators to a PayCLI running on Unix, where filepath would not treat
	// them as separators at all.
	if remote == "" || strings.ContainsRune(remote, 0) ||
		strings.ContainsAny(remote, `/\`) ||
		filepath.IsAbs(remote) || filepath.VolumeName(remote) != "" {
		return "", unusable()
	}
	base := path.Base(remote)
	if base != remote || base == "." || base == ".." {
		return "", unusable()
	}
	return base, nil
}

// resolveDownloadDest anchors a relative destination to the injected working
// directory (§3.1: the process CWD is read once, in app.go), so the file lands
// where the user is standing and a test can point the whole command at a temp
// directory.
func resolveDownloadDest(d *Deps, dest string) string {
	if filepath.IsAbs(dest) || d == nil || d.RT == nil || d.RT.App.WorkDir == "" {
		return dest
	}
	return filepath.Join(d.RT.App.WorkDir, dest)
}

// refuseToClobber stops a server-derived name from silently replacing a local
// file. Lstat, not Stat, so a symlink is seen as the symlink it is.
func refuseToClobber(dest string) error {
	if _, err := os.Lstat(dest); err == nil {
		return apierr.New(apierr.CodeInvalidArgs,
			"%s already exists and the destination came from the server's filename", dest).
			WithHint("pass -o PATH to write somewhere else, or --force to overwrite")
	}
	return nil
}

// downloadSink is where the bytes go. A file destination is written through
// internal/fsatomic (§3.1): the bytes land in a temp file in the target
// directory and are renamed over the destination only once the whole download
// succeeded, so a 403/500 — or a cut-short body — leaves an existing file
// byte-identical, and a symlink at the destination is replaced rather than
// followed.
type downloadSink struct {
	io.Writer
	pipe *io.PipeWriter
	done chan error
}

// openDownloadSink resolves the destination. toStdout is decided by the caller
// from -o, never from a server-supplied name.
func openDownloadSink(d *Deps, dest string, toStdout bool) (*downloadSink, error) {
	if toStdout {
		if d == nil || d.RT == nil || d.RT.App.Stdout == nil {
			return &downloadSink{Writer: io.Discard}, nil
		}
		return &downloadSink{Writer: d.RT.App.Stdout}, nil
	}
	if info, err := os.Lstat(dest); err == nil && info.IsDir() {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"-o %s is a directory", dest).
			WithHint("give the full destination path, e.g. -o %s", filepath.Join(dest, "file.png"))
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- fsatomic.WriteFrom(dest, pr, downloadPerm) }()
	return &downloadSink{Writer: pw, pipe: pw, done: done}, nil
}

// downloadPerm is the mode of a downloaded file: ordinary user content, not a
// credential, but never group- or world-writable.
const downloadPerm = 0o644

// commit finishes the durable write and reports its error.
func (s *downloadSink) commit() error {
	if s == nil || s.pipe == nil {
		return nil
	}
	_ = s.pipe.Close()
	if err := <-s.done; err != nil {
		return apierr.Wrap(err, apierr.CodeFileMissing, "the download could not be written")
	}
	return nil
}

// abort throws the partial download away, leaving any existing destination
// untouched.
func (s *downloadSink) abort() {
	if s == nil || s.pipe == nil {
		return
	}
	_ = s.pipe.CloseWithError(errDownloadAborted)
	<-s.done
}

var errDownloadAborted = errors.New("download aborted")
