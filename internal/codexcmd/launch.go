package codexcmd

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"strings"
)

const (
	defaultApprovalPolicy = "never"
	defaultSandboxMode    = "danger-full-access"
)

var managedHomeSharedEntries = []string{
	"auth.json",
	"auth.gmn.json",
	"auth.proaiapi.json",
	"installation_id",
	"version.json",
	"AGENTS.md",
	"skills",
	"memories",
	"mcp",
	"plugins",
}

// LaunchCommand constructs a shell one-liner that:
//  1. Attempts to resume the last Codex session via "codex resume --last".
//     This relies on the codex CLI exit-code contract:
//     exit 0  — session resumed successfully, nothing more to do.
//     non-zero (e.g. exit 23) — no session to resume, fall through.
//     Verified against @openai/codex >=0.1.x. If the contract changes in a
//     future CLI version this fallback logic will need to be revisited.
//  2. Falls through to a fresh "codex" launch when resume returns non-zero.
func LaunchCommand(cwd string) string {
	return LaunchCommandForSession(cwd, "")
}

// LaunchCommandForSession scopes Codex persistence to an imcodex-managed home
// so each tmux session keeps its own resumable state instead of inheriting the
// user's global interactive drafts.
func LaunchCommandForSession(cwd string, sessionName string) string {
	resumeCommand := shellJoin(
		"codex",
		"resume",
		"--last",
		"-a", defaultApprovalPolicy,
		"-s", defaultSandboxMode,
		"--no-alt-screen",
		"-C", cwd,
	)
	return strings.Join([]string{
		managedHomeBootstrap(cwd, sessionName, false),
		resumeCommand,
		"CODEX_RESUME_STATUS=$?",
		`if [ "$CODEX_RESUME_STATUS" -eq 0 ]; then exit 0; fi`,
		FreshLaunchCommandForSession(cwd, sessionName),
	}, "; ")
}

// FreshLaunchCommand launches a new Codex session without attempting resume.
// Use this only after resume has already been proven stale for the current
// tmux session recovery path.
func FreshLaunchCommand(cwd string) string {
	return FreshLaunchCommandForSession(cwd, "")
}

// FreshLaunchCommandForSession starts a clean Codex session after clearing the
// managed state directory while preserving login/config/skills shared from the
// user's primary CODEX_HOME.
func FreshLaunchCommandForSession(cwd string, sessionName string) string {
	return strings.Join([]string{
		managedHomeBootstrap(cwd, sessionName, true),
		"exec " + shellJoin(
			"codex",
			"-a", defaultApprovalPolicy,
			"-s", defaultSandboxMode,
			"--no-alt-screen",
			"-C", cwd,
		),
	}, "; ")
}

func managedHomeBootstrap(cwd string, sessionName string, clearState bool) string {
	lines := []string{
		`export NO_UPDATE_NOTIFIER="${NO_UPDATE_NOTIFIER:-1}"`,
		`IMCODEX_SOURCE_CODEX_HOME=${CODEX_HOME:-"${HOME}/.codex"}`,
		`IMCODEX_MANAGED_CODEX_HOME="${IMCODEX_SOURCE_CODEX_HOME%/*}/.imcodex/codex/` + managedHomeKey(cwd, sessionName) + `"`,
		`mkdir -p "${IMCODEX_MANAGED_CODEX_HOME}"`,
	}
	if clearState {
		lines = append(lines, managedHomeClearCommand())
	}
	for _, entry := range managedHomeSharedEntries {
		lines = append(lines, managedHomeLinkCommand(entry))
	}
	lines = append(lines, managedHomeConfigCommand(cwd))
	lines = append(lines, `export CODEX_HOME="${IMCODEX_MANAGED_CODEX_HOME}"`)
	return strings.Join(lines, "; ")
}

func managedHomeClearCommand() string {
	parts := []string{`find "${IMCODEX_MANAGED_CODEX_HOME}" -mindepth 1 -maxdepth 1`}
	for _, entry := range managedHomeSharedEntries {
		parts = append(parts, "! -name "+shellQuote(entry))
	}
	parts = append(parts, `-exec rm -rf {} +`)
	return strings.Join(parts, " ")
}

func managedHomeLinkCommand(entry string) string {
	return `if [ -e "${IMCODEX_SOURCE_CODEX_HOME}/` + entry + `" ] && [ ! -e "${IMCODEX_MANAGED_CODEX_HOME}/` + entry + `" ]; then ln -s "${IMCODEX_SOURCE_CODEX_HOME}/` + entry + `" "${IMCODEX_MANAGED_CODEX_HOME}/` + entry + `" 2>/dev/null || cp -a "${IMCODEX_SOURCE_CODEX_HOME}/` + entry + `" "${IMCODEX_MANAGED_CODEX_HOME}/` + entry + `"; fi`
}

func managedHomeConfigCommand(cwd string) string {
	command := `if [ -f "${IMCODEX_SOURCE_CODEX_HOME}/config.toml" ]; then awk '` +
		`/^\[projects\./ { skip=1; next } ` +
		`skip && /^\[/ { skip=0 } ` +
		`!skip { print }` +
		`' "${IMCODEX_SOURCE_CODEX_HOME}/config.toml" > "${IMCODEX_MANAGED_CODEX_HOME}/config.toml"; else : > "${IMCODEX_MANAGED_CODEX_HOME}/config.toml"; fi`
	if strings.TrimSpace(cwd) == "" {
		return command
	}
	return command + `; printf '%s' ` + shellQuote(managedHomeTrustConfig(cwd)) + ` >> "${IMCODEX_MANAGED_CODEX_HOME}/config.toml"`
}

func managedHomeTrustConfig(cwd string) string {
	return "\n[projects." + tomlBasicString(cwd) + "]\ntrust_level = \"trusted\"\n"
}

func tomlBasicString(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"\"", "\\\"",
		"\b", "\\b",
		"\f", "\\f",
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
	)
	return `"` + replacer.Replace(value) + `"`
}

func managedHomePath(sourceHome string, cwd string, sessionName string) string {
	sourceHome = strings.TrimSpace(sourceHome)
	if sourceHome == "" {
		sourceHome = filepath.Join(".codex")
	}
	return filepath.Join(filepath.Dir(sourceHome), ".imcodex", "codex", managedHomeKey(cwd, sessionName))
}

func managedHomeKey(cwd string, sessionName string) string {
	base := strings.TrimSpace(sessionName)
	if base == "" {
		base = strings.TrimSpace(cwd)
	}
	if base == "" {
		base = "default"
	}
	base = sanitizeManagedHomeLabel(base)
	sum := sha1.Sum([]byte(sessionName + "\x00" + cwd))
	return base + "-" + hex.EncodeToString(sum[:6])
}

func sanitizeManagedHomeLabel(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "default"
	}
	if len(out) > 32 {
		out = strings.Trim(out[:32], "-")
	}
	if out == "" {
		return "default"
	}
	return out
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
