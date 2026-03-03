package tui

import (
	"fmt"
	"strings"

	"github.com/makewand/makewand/internal/engine"
)

// System prompt constants used across the build pipeline.
const codeReviewSystemPrompt = "You are an expert code reviewer. Be concise. Only flag real issues, not style preferences."

const autoFixSystemPrompt = "You are an expert programmer fixing build/test errors. Be concise. Only output files that need changes."

const wizardPlanSystemPrompt = "You are a friendly project planner. Explain things simply for non-programmers."

const wizardBuildSystemPrompt = "You are an expert programmer. Generate a complete, working project. " +
	"Output each file with its path and content in this format:\n\n" +
	"--- FILE: path/to/file ---\n```\nfile content here\n```\n\n" +
	"Generate ALL files needed for a working project."

// buildSystemPrompt constructs the system prompt for chat mode, including the current
// project's file tree when available.
func buildSystemPrompt(project *engine.Project) string {
	prompt := `You are makewand, a friendly AI coding assistant for non-programmers.

Guidelines:
- Explain everything in simple, non-technical language
- When creating or modifying files, use this format:
  --- FILE: path/to/file ---
  ` + "```" + `
  file content here
  ` + "```" + `
- When something goes wrong, explain what happened and fix it
- Always confirm before making major changes
- Be encouraging and supportive`

	if project != nil {
		prompt += fmt.Sprintf("\n\nCurrent project: %s\n", project.Name)

		if tree := project.FileTree(); tree != "" {
			prompt += "\nProject files:\n" + tree
		}
	}

	return prompt
}

// buildReviewUserPrompt builds the user-facing code review prompt from the provided
// file contents string. maxBytes is used only for the truncation notice embedded
// by the caller; the string passed in is already bounded.
func buildReviewUserPrompt(fileContents string) string {
	return fmt.Sprintf(
		"Review the following code for bugs, security issues, and improvements. "+
			"If you find issues that need fixing, output the corrected files using this format:\n\n"+
			"--- FILE: path/to/file ---\n```\nfile content here\n```\n\n"+
			"If the code looks good, respond with exactly: LGTM\n\n"+
			"Files to review:\n\n%s",
		fileContents,
	)
}

// buildAutoFixSystemPrompt returns the auto-fix system prompt, augmented with the
// project's file tree when the project is available.
func buildAutoFixSystemPrompt(proj *engine.Project) string {
	sys := autoFixSystemPrompt
	if proj != nil {
		if tree := proj.FileTree(); tree != "" {
			sys += "\n\nProject files:\n" + tree
		}
	}
	return sys
}

// buildAutoFixUserPrompt returns the user message asking the AI to fix an error.
func buildAutoFixUserPrompt(errOutput string) string {
	return fmt.Sprintf(
		"The following error occurred in the project:\n\n```\n%s\n```\n\n"+
			"Please fix the issue. Output the corrected files using this format:\n\n"+
			"--- FILE: path/to/file ---\n```\nfile content here\n```\n\n"+
			"Only output files that need to be changed.",
		strings.TrimSpace(errOutput),
	)
}

// buildWizardPlanUserPrompt builds the planning request prompt for a project template.
func buildWizardPlanUserPrompt(tplName, tplPrompt string) string {
	return fmt.Sprintf(
		"I want to create a project using this template: %s\n\n"+
			"Requirements:\n%s\n\n"+
			"Please provide a brief project plan with:\n"+
			"1. Tech stack choices\n"+
			"2. Main features\n"+
			"3. File structure\n"+
			"4. Estimated cost\n\n"+
			"Keep it concise and non-technical.",
		tplName, tplPrompt,
	)
}
