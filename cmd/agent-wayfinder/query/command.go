package query

import (
	"io"

	cmd "agent-wayfinder/cmd/agent-wayfinder/internal/command"

	"github.com/spf13/cobra"
)

func New(standardOutput, standardError io.Writer, exitCode *int) *cobra.Command {
	return cmd.NewLeaf("query WORKSPACE (QUESTION | TERM...)", "Query a published graph", Configure, run, standardOutput, standardError, exitCode)
}

func Configure(command *cobra.Command) {
	cmd.DatabaseAndFormatFlags(command)
	command.Flags().Bool("question", false, "interpret one argument as an architecture question")
	command.Flags().Bool("terms", false, "treat all arguments as literal lookup terms")
	command.Flags().Bool("show-plan", false, "show the interpreted query plan in text output")
	command.Flags().Bool("claude", false, "enable Claude CLI query planning")
	command.Flags().String("claude-path", "", "Claude CLI path override")
	command.Flags().String("claude-model", "", "Claude model override")
	command.Flags().String("claude-fallback-model", "", "Claude fallback model override")
	command.Flags().Float64("claude-max-budget-usd", 0, "Claude maximum budget in USD")
	command.Flags().String("claude-effort", "", "Claude effort override")
	command.Flags().String("claude-timeout", "", "Claude planner timeout override")
	command.Flags().Bool("copilot", false, "enable Copilot query planning")
	command.Flags().String("copilot-model", "", "Copilot model override")
	command.Flags().Int("copilot-max-ai-credits", 0, "Copilot maximum AI credit override")
	command.Flags().Int("copilot-token-budget", 0, "Copilot token budget override")
	command.Flags().String("copilot-timeout", "", "Copilot timeout override")
	command.Flags().Int("max-depth", 2, "maximum traversal depth")
	command.Flags().Int("max-nodes", 100, "maximum traversed nodes")
	command.Flags().StringArray("project", nil, "project scope ID")
	command.Flags().StringArray("relation", nil, "allowed relation")
}
