package codexcmd

import "strings"

const (
	defaultApprovalPolicy = "never"
	defaultSandboxMode    = "danger-full-access"
)

// LaunchCommand constructs a shell one-liner that:
//  1. Attempts to resume the last Codex session via "codex resume --last".
//     This relies on the codex CLI exit-code contract:
//       exit 0  — session resumed successfully, nothing more to do.
//       non-zero (e.g. exit 23) — no session to resume, fall through.
//     Verified against @openai/codex >=0.1.x. If the contract changes in a
//     future CLI version this fallback logic will need to be revisited.
//  2. Falls through to a fresh "codex" launch when resume returns non-zero.
func LaunchCommand(cwd string) string {
	resumeCommand := shellJoin(
		"codex",
		"resume",
		"--last",
		"-a", defaultApprovalPolicy,
		"-s", defaultSandboxMode,
		"--no-alt-screen",
		"-C", cwd,
	)
	freshCommand := "exec " + shellJoin(
		"codex",
		"-a", defaultApprovalPolicy,
		"-s", defaultSandboxMode,
		"--no-alt-screen",
		"-C", cwd,
	)
	return strings.Join([]string{
		resumeCommand,
		"CODEX_RESUME_STATUS=$?",
		`if [ "$CODEX_RESUME_STATUS" -eq 0 ]; then exit 0; fi`,
		freshCommand,
	}, "; ")
}

// ShellJoin builds a shell command string by single-quote-escaping each
// argument and joining them with spaces.
func ShellJoin(args ...string) string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		out = append(out, ShellQuote(arg))
	}
	return strings.Join(out, " ")
}

// ShellQuote wraps a string in single quotes and escapes any embedded single
// quotes, producing output safe for POSIX sh word splitting and globbing.
func ShellQuote(in string) string {
	if in == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(in, "'", `'\''`) + "'"
}

// shellJoin and shellQuote are package-internal aliases kept for call sites
// in this file that pre-date the exported names.
func shellJoin(args ...string) string { return ShellJoin(args...) }
func shellQuote(in string) string     { return ShellQuote(in) }
