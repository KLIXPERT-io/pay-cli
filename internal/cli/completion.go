package cli

import (
	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
)

func init() { Register(newCompletionCmd) }

// newCompletionCmd prints a shell completion script.
//
// It deliberately does NOT emit an envelope: the output is piped straight into
// `source`, and a JSON wrapper would make it unusable. It is the only command
// besides `pay __complete` with that exemption (§9.9).
func newCompletionCmd(rt *Runtime) *cobra.Command {
	shells := []string{"bash", "zsh", "fish", "powershell"}

	cmd := &cobra.Command{
		Use:       "completion <bash|zsh|fish|powershell>",
		Short:     "Print a shell completion script for pay.",
		GroupID:   GroupAdmin,
		ValidArgs: shells,
		Args:      exactArgs(1, "pay completion bash|zsh|fish|powershell"),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			out := cmd.OutOrStdout()
			var err error
			switch args[0] {
			case "bash":
				err = root.GenBashCompletionV2(out, true)
			case "zsh":
				err = root.GenZshCompletion(out)
			case "fish":
				err = root.GenFishCompletion(out, true)
			case "powershell":
				err = root.GenPowerShellCompletionWithDesc(out)
			default:
				err = apierr.New(apierr.CodeInvalidOption,
					"%q is not a supported shell", args[0]).
					WithDidYouMean(apierr.DidYouMean(args[0], shells)...)
			}
			if err != nil {
				rt.emit(output.NewError("completion", err))
			}
			return nil
		},
	}

	SetHelp(cmd, &Help{
		Synopsis: []string{"pay completion bash|zsh|fish|powershell"},
		Long: "The script is plain shell, not an envelope — it is meant to be sourced.\n" +
			"Completion never touches the network and never blocks: on a cold or expired\n" +
			"cache it offers nothing rather than triggering discovery (§9.9).",
		Output:    OutputSpec{Kind: output.KindRaw},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitValidation},
		Examples: []Example{
			{Why: "this shell, right now", Cmd: "source <(pay completion bash)"},
			{Why: "persist for bash", Cmd: "pay completion bash > /etc/bash_completion.d/pay"},
			{Why: "persist for zsh", Cmd: "pay completion zsh > \"${fpath[1]}/_pay\""},
			{Why: "persist for fish", Cmd: "pay completion fish > ~/.config/fish/completions/pay.fish"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting collection slugs to complete before the first discovery.",
				Right: "Run `pay discover` once; completion reads the cached index only."},
			{Wrong: "Piping the script through `jq`.",
				Right: "It is a shell script, not JSON. Only envelope-producing commands emit JSON."},
			{Wrong: "Adding `--output json` to this command.",
				Right: "It has no effect here; use `pay completion bash | head` to inspect the script."},
		},
		SeeAlso: []string{"pay explain", "pay collections"},
	})
	return cmd
}
