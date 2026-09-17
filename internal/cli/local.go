package cli

import (
	"context"
	"sort"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

// This file is the runtime half of §9.11's edit pipeline: the variants of
// dataDeps and runData used by a command that edits a document instead of
// fetching one.
//
// The difference is one rule. dataDeps treats a missing profile, an unreachable
// base_url or a rejected credential as fatal, which is right for every command
// that is about to make a request. A `pay blocks` verb makes none: it reads a
// document from stdin, rewrites an array and prints the result. Failing it on
// auth_missing would make the family unusable in the two places it is most
// useful — against a document saved to a file, and on a machine that has never
// run `pay auth login`.
//
// Discovery is still used when it is there. It is what turns `blockType: ctaa`
// into a named local failure instead of a row Payload silently drops.

// localDeps builds the dependency set for a command that performs no I/O
// against Payload. It never fails: the client and the manifest are both
// optional, and a command that needs neither runs unvalidated rather than
// refusing to start.
func localDeps(ctx context.Context, rt *Runtime) *Deps {
	d := &Deps{RT: rt, FetchURL: remoteFetcher(rt)}

	client, err := rt.Client(ctx)
	if err != nil {
		// Recorded at debug level and not warned about: a caller editing a
		// JSON file has no profile on purpose, and a warning on every run
		// would train them to ignore warnings. The one case where the absence
		// matters — a document that names a collection PayCLI could have
		// validated against — is warned about by localValidationWarning.
		rt.Log.Debug("no Payload client for a local edit", "err", err.Error())
		return d
	}
	d.Client = client

	m, err := rt.Discovery(ctx)
	if err != nil {
		rt.Log.Debug("no discovery for a local edit", "err", err.Error())
		return d
	}
	d.Manifest = m
	return d
}

// localValidationWarning is raised when a piped envelope names a collection but
// no schema for it could be loaded. It matters because it changes what the
// command guarantees: with a schema, an unknown blockType is rejected here;
// without one, it reaches Payload, which drops the row and answers 201.
func localValidationWarning(target *output.Target, shard *discovery.Shard) *output.Warning {
	if target == nil || target.Slug == "" || shard != nil {
		return nil
	}
	return &output.Warning{
		Code: "local_validation_skipped",
		Message: "no field schema for " + target.Slug + " was available, so blockType values were not checked; " +
			"Payload DROPS a row whose blockType it does not recognise and still answers 201",
		Hint: "run `pay discover --refresh` once to make that a local failure instead",
	}
}

// runLocalData is runData for a command that never touches the network.
func runLocalData(rt *Runtime, name string, h dataHandler) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		rt.Command = name
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		d := localDeps(ctx, rt)
		env, err := h(ctx, cmd, d, args)
		if err != nil {
			env = output.NewError(name, err)
		} else if env == nil {
			env = output.NewError(name, apierr.New(apierr.CodeInternal,
				"command %q produced no result", name))
		}
		rt.emit(env)
		return nil
	}
}

// contextWithStageFlags carries a verb's shared --field value into runStage.
func contextWithStageFlags(ctx context.Context, f *stageFlags) context.Context {
	return context.WithValue(ctx, stageFlagsKey{}, f)
}

// sortStrings is sort.Strings under a name that says it is for output ordering,
// so a reader is not left wondering whether the order is load-bearing. It is:
// every list PayCLI prints is deterministic.
func sortStrings(s []string) { sort.Strings(s) }
