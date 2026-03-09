package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/makewand/makewand/internal/engine"
	"github.com/makewand/makewand/internal/model"
)

// System prompt constants used across the build pipeline.
const codeReviewSystemPrompt = "You are an expert code reviewer. Be concise. Only flag real issues, not style preferences."

const autoFixSystemPrompt = "You are an expert programmer fixing build/test errors. Be concise. Only output files that need changes."

const wizardPlanSystemPrompt = "You are a friendly project planner. Explain things simply for non-programmers."

const wizardBuildSystemPrompt = "You are an expert programmer. Generate a complete, working project. " +
	"Output each file with its path and content in this format:\n\n" +
	"--- FILE: path/to/file ---\n```\nfile content here\n```\n\n" +
	"Generate ALL files needed for a working project."

const wizardBuildRetryRules = "Important output rules:\n" +
	"1. Output ONLY file blocks in this exact format:\n" +
	"   --- FILE: path/to/file ---\n" +
	"   ```\n" +
	"   file content here\n" +
	"   ```\n" +
	"2. Do NOT include explanations, bullet lists, or shell commands.\n" +
	"3. Include all files required to run the project."

const maxSystemPromptFileTreeLines = 220
const maxSystemPromptFileTreeChars = 10000

// buildSystemPrompt constructs the system prompt for chat mode, including the current
// project's file tree when available.
func buildSystemPrompt(project *engine.Project, task model.TaskType) string {
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
		if includeProjectTreeForTask(task) {
			if tree := compactProjectTreeForPrompt(project.Files, maxSystemPromptFileTreeLines, maxSystemPromptFileTreeChars); tree != "" {
				prompt += "\nProject files:\n" + tree
			}
		} else {
			prompt += fmt.Sprintf("\nProject entries: %d (full tree omitted for this request type)\n", projectEntryCount(project.Files))
		}
	}

	return prompt
}

func includeProjectTreeForTask(task model.TaskType) bool {
	switch task {
	case model.TaskCode, model.TaskFix, model.TaskReview:
		return true
	default:
		return false
	}
}

func projectEntryCount(files []engine.FileEntry) int {
	count := 0
	for _, f := range files {
		if f.Path == "." || f.Path == "" {
			continue
		}
		count++
	}
	return count
}

func compactProjectTreeForPrompt(files []engine.FileEntry, maxLines, maxChars int) string {
	if maxLines <= 0 || maxChars <= 0 || len(files) == 0 {
		return ""
	}

	entries := make([]engine.FileEntry, 0, len(files))
	for _, f := range files {
		if f.Path == "." || f.Path == "" {
			continue
		}
		entries = append(entries, f)
	}
	if len(entries) == 0 {
		return "(empty project)"
	}

	var b strings.Builder
	lineCount := 0
	for i, f := range entries {
		if lineCount >= maxLines {
			remaining := len(entries) - i
			b.WriteString(fmt.Sprintf("- ... %d more files not shown ...\n", remaining))
			break
		}

		depth := strings.Count(f.Path, string(filepath.Separator))
		indent := strings.Repeat("  ", depth)
		name := filepath.Base(f.Path)
		if f.IsDir {
			name += "/"
		}
		line := fmt.Sprintf("%s- %s\n", indent, name)

		if b.Len()+len(line) > maxChars {
			remaining := len(entries) - i
			if remaining > 0 {
				b.WriteString(fmt.Sprintf("- ... %d more files not shown ...\n", remaining))
			}
			break
		}

		b.WriteString(line)
		lineCount++
	}

	return strings.TrimSpace(b.String())
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

// buildWizardCodeFormatRetryPrompt asks the model to regenerate project output
// strictly as file blocks when the previous response did not include writable
// files.
func buildWizardCodeFormatRetryPrompt(originalPrompt, previousOutput string) string {
	return fmt.Sprintf(
		"The previous response did not provide writable files.\n\n"+
			"Original project request:\n%s\n\n"+
			"Previous response (for context):\n%s\n\n"+
			"Regenerate the full project now.\n\n%s",
		trimPromptContext(originalPrompt, 4000),
		trimPromptContext(previousOutput, 3000),
		wizardBuildRetryRules,
	)
}

func trimPromptContext(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 32 {
		return s[:max]
	}
	return s[:max-16] + "\n...[truncated]"
}
