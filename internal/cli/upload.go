package cli

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

type uploadFlags struct {
	data writeData

	filename    string
	replace     string
	contentType string
	maxSize     string
	allowRemote bool
	locale      string
	noEchoCheck bool
}

// newUploadCmd builds `pay upload <collection> <file|-|URL>` (§13).
func init() { Register(newUploadCmd) }

func newUploadCmd(rt *Runtime) *cobra.Command {
	f := &uploadFlags{}
	cmd := &cobra.Command{
		GroupID: GroupFiles,
		Use:     "upload <collection> <file|-|URL>",
		Short:   "Upload a file to an upload-enabled collection",
		Long: `Upload a file as multipart/form-data.

The file part must be named literally "file"; a JSON body with a "url" key does
NOT work over REST (that is an admin-client feature), so a URL argument is
downloaded client-side and re-sent as multipart.

Sibling fields travel in a "_payload" JSON part built by the §9.10.2 merge, with
--alt as the lowest-precedence contributor:
  --alt → --data/--data-file → --set → --set-json
so ` + "`--set alt=B --alt A`" + ` yields alt: "B". The flags are combinable, not
alternatives.

Filename collisions are auto-renamed SERVER-SIDE (paycli-probe.png becomes
paycli-probe-1.png). PayCLI echoes the name Payload actually stored and warns
when it differs, because an agent will otherwise assume its own.

--replace <id> re-uploads into an existing document. It also mints a NEW
filename rather than overwriting in place, so any hardcoded URL to the old file
breaks.

`,
		Args: cobra.ExactArgs(2),
	}
	cmd.RunE = runData(rt, safety.CmdUpload, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		return runUpload(ctx, d, f, args[0], args[1])
	})
	f.data.register(cmd)
	fl := cmd.Flags()
	fl.StringVar(&f.data.alt, "alt", "", "alt text (lowest-precedence contributor to the _payload part)")
	fl.StringVar(&f.filename, "filename", "", "filename to send (required when the source is -)")
	fl.StringVar(&f.replace, "replace", "", "replace the file of this existing document id")
	fl.StringVar(&f.contentType, "content-type", "", "MIME type of the file part (sniffed from the extension otherwise)")
	fl.StringVar(&f.maxSize, "max-size", "100MB", "refuse a file larger than this")
	fl.BoolVar(&f.allowRemote, "allow-remote", false, "allow a non-HTTPS or private-range URL source")
	fl.StringVar(&f.locale, "locale", "", "locale to write into")
	fl.BoolVar(&f.noEchoCheck, "no-echo-check", false, "skip the §10.2 echo-diff")

	SetHelp(cmd, uploadHelp())
	return cmd
}

// uploadHelp is §10.5's model for `pay upload`.
func uploadHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay upload <collection> <file|-|URL> [--alt TEXT] [--filename NAME] [--content-type MIME]",
			"                                     [--replace ID] [--set k=v ...] [--max-size 100MB]",
		},
		Collections: true,
		Discovered: func(rt *Runtime, m *discovery.Manifest) []string {
			var slugs []string
			for _, c := range m.Collections {
				if c.Flags.Upload != nil && *c.Flags.Upload {
					slugs = append(slugs, c.Slug)
				}
			}
			lines := []string{fmt.Sprintf("upload collections (%d) — the only valid first argument:", len(slugs))}
			return append(lines, wrapList("  ", slugs, 84)...)
		},
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum",
				ValuesFrom: "discovery.collections", Example: "media"},
			{Name: "source", Required: true, Type: "string",
				Example: "./hero.png  (a path, - for stdin, or an https:// URL)"},
		},
		FlagInfo: map[string]FlagInfo{
			"set":          {Grammar: "key=value", Repeatable: true},
			"set-json":     {Grammar: "key=JSON", Repeatable: true},
			"filename":     {Note: "REQUIRED when the source is -"},
			"content-type": {Note: "sniffed from the extension when omitted"},
			"max-size":     {Grammar: "100MB | 2GB | 512KB"},
			"alt":          {Note: "lowest-precedence contributor: --set alt=B --alt A yields \"B\""},
		},
		Output: OutputSpec{
			Kind:     output.KindDoc,
			Skeleton: `{"id":4,"filename":"paycli-probe-1.png","mimeType":"image/png","url":"/api/media/file/…","sizes":{…}}`,
		},
		ExitCodes: WriteExitCodes,
		Examples: []Example{
			{Why: "see the multipart request before sending any bytes",
				Cmd: "pay upload media ./hero.png --alt 'Hero image' --dry-run"},
			{Why: "the normal case: a local file plus its sibling fields",
				Cmd: "pay upload media ./hero.png --alt 'Hero image'"},
			{Why: "from stdin — --filename is then required, because there is no path to take it from",
				Cmd: "pay upload media - --filename shot.png --content-type image/png < shot.png"},
			{Why: "from a URL: PayCLI downloads it and re-sends it as multipart",
				Cmd: "pay upload media https://example.com/logo.svg --allow-remote"},
			{Why: "re-upload into an existing document",
				Cmd: "pay upload media ./hero-v2.png --replace 4"},
			{Why: "which collections accept an upload at all",
				Cmd: "pay collections --kind upload"},
		},
		Mistakes: []Mistake{
			{Wrong: "Sending a JSON body with a \"url\" key, as the admin UI does.",
				Right: "That is not a REST feature. Over REST the file part must be named literally \"file\" and the body must be multipart; pass the URL as the argument and PayCLI does the download."},
			{Wrong: "Assuming the stored filename is the one you sent.",
				Right: "Payload auto-renames collisions SERVER-SIDE (hero.png becomes hero-1.png). Read .data.filename and .data.url from the response; PayCLI warns when they differ."},
			{Wrong: "Expecting --replace to overwrite the file in place.",
				Right: "It mints a NEW filename, so any hardcoded URL to the old file breaks. Update the references, or delete and re-create deliberately."},
			{Wrong: "`pay upload <collection>` on a non-upload collection.",
				Right: "It fails locally with not_upload_collection (exit 5). `pay collections --kind upload` lists the ones that work."},
			{Wrong: "`pay create media --file ./hero.png`.",
				Right: "create cannot build a multipart body; it tells you so. Use `pay upload`."},
		},
		SeeAlso: []string{
			"pay download <collection> <id>   # the bytes back out",
			"pay collections --kind upload",
			"pay describe <collection>   # the sibling fields this collection defines",
		},
	}
}

func runUpload(ctx context.Context, d *Deps, f *uploadFlags, slug, source string) (*output.Envelope, error) {
	cfg := d.cfg()
	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	t, err := d.collection(ctx, slug)
	if err != nil {
		return nil, err
	}
	// §13 preflight: an ordinary collection answers a multipart body with an
	// opaque 500, so the manifest's answer is checked first.
	if err := payload.CheckUploadCollection(t.Slug, t.flags().Upload); err != nil {
		return nil, err
	}
	if f.replace != "" {
		if e := d.checkID(t, t.Slug, f.replace); e != nil {
			return nil, e
		}
	}

	maxSize, err := parseSize(f.maxSize)
	if err != nil {
		return nil, err
	}
	fields, warnings, err := f.data.build(d, t, t.Shard)
	if err != nil {
		return nil, err
	}

	body, filename, contentType, closer, err := openUploadSource(ctx, d, f, source)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
	}
	if f.contentType != "" {
		contentType = f.contentType
	}
	if f.filename != "" {
		filename = f.filename
	}
	if filename == "" {
		return nil, apierr.New(apierr.CodeInvalidArgs,
			"a filename is required when the file comes from stdin").
			WithHint("pass --filename NAME.ext so Payload can store and serve it")
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))
	}

	// §7.9a: the upload response is the document, localized fields and all.
	uploadParams := query.Params{}
	if warn, err := applyWriteLocale(d, t, f.locale, &uploadParams); err != nil {
		return nil, err
	} else if warn != nil {
		d.RT.Warn(*warn)
	}
	in := payload.UploadInput{
		Collection:  t.Slug,
		Filename:    filename,
		ContentType: contentType,
		Body:        payload.LimitReader(body, maxSize),
		Fields:      fields,
		ReplaceID:   f.replace,
		Params:      uploadParams,
	}

	method, path := "POST", "/"+t.Slug
	if f.replace != "" {
		method, path = "PATCH", "/"+t.Slug+"/"+f.replace
	}
	op := safety.Op{Command: safety.CmdUpload, Selector: safety.SelectorNone}
	if f.replace != "" {
		op.Selector = safety.SelectorID
	}
	w := d.newWriteOp(op, t.Slug, method, path)
	if f.replace != "" {
		w = w.withIDs([]any{f.replace})
	}

	if cfg.DryRun {
		preview := map[string]any{"file": filename, "content_type": contentType, "_payload": fields}
		return emitDryRun(d, w, safety.CmdUpload, method,
			client.URLFor(&payload.Request{Method: method, Path: path}), preview, 1, nil, warnings...)
	}
	if err := w.confirm(1); err != nil {
		return nil, err
	}
	if err := w.pre(1); err != nil {
		return nil, err
	}

	res, err := client.Upload(ctx, in, payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(fields))))
	if err != nil {
		w.post(false, statusOf(res), 0, 1, err.Error(), "")
		return nil, err
	}

	changed := &output.Changed{Created: 1, IDs: []any{res.Doc.ID()}}
	if f.replace != "" {
		changed = &output.Changed{Updated: 1, IDs: []any{res.Doc.ID()}}
	}
	env := output.New(safety.CmdUpload, output.KindDoc, res.Doc).
		WithTarget(t.envTarget(res.Doc.ID())).
		WithChanged(changed).
		WithMeta(withLocaleMeta(w.run.meta(), uploadParams)).
		WithRawBody(res.Raw, !cfg.Redact).
		WithNext(&output.Next{
			Reason: output.ReasonVerifyWrite,
			Cmd:    fmt.Sprintf("pay get %s %s --depth 0%s", t.Slug, res.Doc.IDString(), d.profileFlag()),
		})
	for _, warn := range warnings {
		env.AddWarning(warn)
	}
	if stored, changedName := payload.FilenameChanged(filename, res.Doc); changedName {
		env.AddWarning(output.Warning{
			Code: warnFilenameChanged,
			Message: fmt.Sprintf("Payload stored the file as %q, not %q. Filename collisions are auto-renamed server-side.",
				stored, filename),
			Sent:     map[string]any{"filename": filename},
			Returned: map[string]any{"filename": stored},
			Hint:     "use the returned filename for `pay download` and for any URL you build",
		})
	}
	if f.replace != "" {
		env.AddWarning(output.Warning{
			Code:    warnFilenameChanged,
			Message: "--replace mints a NEW filename rather than overwriting in place, so any hardcoded URL to the old file now 404s.",
			Hint:    "re-read the document and update whatever referenced the old URL",
		})
	}
	for _, warn := range echoDiff(t.Slug, fields, res.Doc, echoConfig{
		Shard:     t.Shard,
		Ignore:    cfg.EchoCheckIgnore,
		LocaleAll: f.locale == "all",
		Disabled:  f.noEchoCheck,
	}) {
		env.AddWarning(warn)
	}
	return w.finish(env, res.HTTP, 1, 0, "")
}

// openUploadSource resolves the file|-|URL argument.
func openUploadSource(ctx context.Context, d *Deps, f *uploadFlags, source string) (io.Reader, string, string, io.Closer, error) {
	switch {
	case source == "-":
		return d.inStream(), f.filename, f.contentType, nil, nil

	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		if err := checkRemoteAllowed(source, f.allowRemote); err != nil {
			return nil, "", "", nil, err
		}
		if d == nil || d.FetchURL == nil {
			return nil, "", "", nil, apierr.New(apierr.CodeOperationUnsupported,
				"this build cannot download a remote upload source").
				WithHint("download it yourself and pass the local path")
		}
		body, ctype, _, err := d.FetchURL(ctx, source)
		if err != nil {
			return nil, "", "", nil, apierr.Wrap(err, apierr.CodeNetworkUnreachable,
				"could not download %s", source)
		}
		name := f.filename
		if name == "" {
			if u, err := url.Parse(source); err == nil {
				name = filepath.Base(u.Path)
			}
		}
		return body, name, ctype, body, nil

	default:
		file, err := os.Open(source)
		if err != nil {
			return nil, "", "", nil, apierr.Wrap(err, apierr.CodeFileMissing,
				"%s could not be opened", source)
		}
		return file, filepath.Base(source), f.contentType, file, nil
	}
}

// checkRemoteAllowed enforces §13's --allow-remote gate: plain HTTP and
// private-range hosts need an explicit opt-in.
func checkRemoteAllowed(raw string, allowed bool) error {
	if allowed {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return apierr.New(apierr.CodeInvalidArgs, "%q is not a valid URL", raw)
	}
	private := isPrivateHost(u.Hostname())
	if u.Scheme == "https" && !private {
		return nil
	}
	why := "it is not HTTPS"
	if private {
		why = "it points at a private-range or loopback host"
	}
	return apierr.New(apierr.CodeInvalidArgs,
		"refusing to fetch %s: %s", raw, why).
		WithHint("pass --allow-remote to fetch it anyway, or download it yourself and pass the local path")
}

func isPrivateHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1", "":
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// parseSize accepts 104857600, "100MB", "10 MiB", "2g".
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return payload.DefaultMaxUploadSize, nil
	}
	upper := strings.ToUpper(strings.ReplaceAll(s, " ", ""))
	units := []struct {
		suffix string
		mult   int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"B", 1},
	}
	for _, u := range units {
		if !strings.HasSuffix(upper, u.suffix) {
			continue
		}
		num := strings.TrimSuffix(upper, u.suffix)
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			break
		}
		return int64(v * float64(u.mult)), nil
	}
	if v, err := strconv.ParseInt(upper, 10, 64); err == nil {
		return v, nil
	}
	return 0, apierr.New(apierr.CodeInvalidArgs,
		"--max-size %q is not a size", s).
		WithHint("examples: 100MB, 10MiB, 2g, 104857600")
}
