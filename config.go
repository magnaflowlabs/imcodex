package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	larksdk "github.com/larksuite/oapi-sdk-go/v3"
	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath     = "imcodex.yaml"
	defaultUserConfigName = ".imcodex.yaml"
	defaultPlatform       = "lark"
	defaultTelegramAPIURL = "https://api.telegram.org"
	defaultRuntime        = runtimeDockerCodex
	runtimeHostCodex      = "host-codex"
	runtimeDockerCodex    = "docker-codex"
)

type config struct {
	path                  string
	platform              string
	runtime               string
	larkAppID             string
	larkAppSecret         string
	larkBaseURL           string
	telegramBotToken      string
	telegramBaseURL       string
	codexConfigDir        string
	interruptOnNewMessage bool
	groups                []groupConfig
}

type groupConfig struct {
	GroupID     string      `yaml:"group_id"`
	CWD         string      `yaml:"cwd"`
	SessionName string      `yaml:"session_name"`
	Jobs        []jobConfig `yaml:"jobs"`
}

type jobConfig struct {
	Name         string `yaml:"name"`
	Schedule     string `yaml:"schedule"`
	PromptFile   string `yaml:"prompt_file"`
	Command      string `yaml:"command"`
	ArtifactsDir string `yaml:"artifacts_dir"`
	SummaryFile  string `yaml:"summary_file"`
	SessionName  string `yaml:"session_name"`
}

type fileConfig struct {
	Platform              string        `yaml:"platform"`
	LarkAppID             string        `yaml:"lark_app_id"`
	LarkAppSecret         string        `yaml:"lark_app_secret"`
	LarkBaseURL           string        `yaml:"lark_base_url"`
	TelegramBotToken      string        `yaml:"telegram_bot_token"`
	TelegramBaseURL       string        `yaml:"telegram_base_url"`
	Runtime               string        `yaml:"runtime"`
	RuntimeConfigDir      string        `yaml:"runtime_config_dir"`
	SessionCommand        string        `yaml:"session_command"`
	InterruptOnNewMessage *bool         `yaml:"interrupt_on_new_message"`
	Groups                []groupConfig `yaml:"groups"`
}

func parseConfig(args []string, lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) (config, error) {
	fs := flag.NewFlagSet("imcodex", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var path string
	var runtime string
	var codexConfigDir string
	fs.StringVar(&path, "config", "", "Config file path")
	fs.StringVar(&runtime, "runtime", defaultRuntime, "Runtime mode: docker-codex or host-codex")
	fs.StringVar(&codexConfigDir, "codex-config-dir", "", "Codex config directory override for docker-codex")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if readFile == nil {
		readFile = os.ReadFile
	}
	path, err := resolveCLIPath(path, lookupEnv)
	if err != nil {
		return config{}, err
	}
	path, data, err := loadConfigFile(path, lookupEnv, readFile)
	if err != nil {
		return config{}, err
	}

	var file fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := rejectDeprecatedRuntimeFields(file); err != nil {
		return config{}, err
	}
	runtime, err = normalizeRuntime(runtime)
	if err != nil {
		return config{}, err
	}
	codexConfigDir, err = resolveCLIPath(codexConfigDir, lookupEnv)
	if err != nil {
		return config{}, err
	}

	cfg := config{
		path:                  path,
		platform:              firstNonEmpty(file.Platform, envValue(lookupEnv, "IMCODEX_PLATFORM"), defaultPlatform),
		runtime:               runtime,
		larkAppID:             firstNonEmpty(file.LarkAppID, envValue(lookupEnv, "LARK_APP_ID")),
		larkAppSecret:         firstNonEmpty(file.LarkAppSecret, envValue(lookupEnv, "LARK_APP_SECRET")),
		larkBaseURL:           firstNonEmpty(file.LarkBaseURL, envValue(lookupEnv, "LARK_BASE_URL"), larksdk.LarkBaseUrl),
		telegramBotToken:      firstNonEmpty(file.TelegramBotToken, envValue(lookupEnv, "TELEGRAM_BOT_TOKEN")),
		telegramBaseURL:       firstNonEmpty(file.TelegramBaseURL, envValue(lookupEnv, "TELEGRAM_BASE_URL"), defaultTelegramAPIURL),
		codexConfigDir:        codexConfigDir,
		interruptOnNewMessage: boolValue(file.InterruptOnNewMessage, true),
		groups:                normalizeGroups(file.Groups, path, lookupEnv),
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func loadConfigFile(path string, lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) (string, []byte, error) {
	if path != "" {
		data, err := readFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("read config %s: %w", path, err)
		}
		return path, data, nil
	}

	candidates := []string{defaultConfigPath}
	if home := firstNonEmpty(envValue(lookupEnv, "HOME"), envValue(lookupEnv, "USERPROFILE")); home != "" {
		candidates = append(candidates, filepath.Join(home, defaultUserConfigName))
	}

	var missing []string
	for _, candidate := range candidates {
		data, err := readFile(candidate)
		if err == nil {
			return candidate, data, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, candidate)
			continue
		}
		return "", nil, fmt.Errorf("read config %s: %w", candidate, err)
	}

	return "", nil, fmt.Errorf("config not found; tried %s", strings.Join(missing, ", "))
}

func normalizeGroups(groups []groupConfig, configPath string, lookupEnv func(string) (string, bool)) []groupConfig {
	out := make([]groupConfig, 0, len(groups))
	for _, group := range groups {
		cwd := expandPathValue(group.CWD, lookupEnv)
		jobs := make([]jobConfig, 0, len(group.Jobs))
		for _, job := range group.Jobs {
			jobs = append(jobs, jobConfig{
				Name:         strings.TrimSpace(job.Name),
				Schedule:     strings.TrimSpace(job.Schedule),
				PromptFile:   resolveConfigRelativePath(configPath, job.PromptFile, lookupEnv),
				Command:      strings.TrimSpace(job.Command),
				ArtifactsDir: resolveWorkingDirRelativePath(cwd, job.ArtifactsDir, lookupEnv),
				SummaryFile:  resolveWorkingDirRelativePath(cwd, job.SummaryFile, lookupEnv),
				SessionName:  strings.TrimSpace(job.SessionName),
			})
		}
		out = append(out, groupConfig{
			GroupID:     strings.TrimSpace(group.GroupID),
			CWD:         cwd,
			SessionName: strings.TrimSpace(group.SessionName),
			Jobs:        jobs,
		})
	}
	return out
}

func (c config) validate() error {
	c.platform = strings.ToLower(strings.TrimSpace(c.platform))
	if c.platform == "" {
		c.platform = defaultPlatform
	}
	switch c.platform {
	case "lark":
		if c.larkAppID == "" {
			return errors.New("required: lark_app_id or LARK_APP_ID")
		}
		if c.larkAppSecret == "" {
			return errors.New("required: lark_app_secret or LARK_APP_SECRET")
		}
		if c.larkBaseURL == "" {
			return errors.New("required: lark_base_url or LARK_BASE_URL")
		}
	case "telegram":
		if c.telegramBotToken == "" {
			return errors.New("required: telegram_bot_token or TELEGRAM_BOT_TOKEN")
		}
		if c.telegramBaseURL == "" {
			return errors.New("required: telegram_base_url or TELEGRAM_BASE_URL")
		}
	default:
		return fmt.Errorf("unsupported platform: %s", c.platform)
	}
	if len(c.groups) == 0 {
		return errors.New("required: groups")
	}

	seen := make(map[string]struct{}, len(c.groups))
	for i, group := range c.groups {
		switch {
		case group.GroupID == "":
			return fmt.Errorf("groups[%d].group_id is required", i)
		case group.CWD == "":
			return fmt.Errorf("groups[%d].cwd is required", i)
		}
		if _, ok := seen[group.GroupID]; ok {
			return fmt.Errorf("duplicate group_id: %s", group.GroupID)
		}
		seen[group.GroupID] = struct{}{}

		jobSeen := make(map[string]struct{}, len(group.Jobs))
		for j, job := range group.Jobs {
			hasPrompt := job.PromptFile != ""
			hasCommand := job.Command != ""
			switch {
			case job.Name == "":
				return fmt.Errorf("groups[%d].jobs[%d].name is required", i, j)
			case job.Schedule == "":
				return fmt.Errorf("groups[%d].jobs[%d].schedule is required", i, j)
			case hasPrompt && hasCommand:
				return fmt.Errorf("groups[%d].jobs[%d] must set only one of prompt_file or command", i, j)
			case !hasPrompt && !hasCommand:
				return fmt.Errorf("groups[%d].jobs[%d] must set one of prompt_file or command", i, j)
			case !hasCommand && job.ArtifactsDir != "":
				return fmt.Errorf("groups[%d].jobs[%d].artifacts_dir requires command", i, j)
			case !hasCommand && job.SummaryFile != "":
				return fmt.Errorf("groups[%d].jobs[%d].summary_file requires command", i, j)
			}
			if _, ok := jobSeen[job.Name]; ok {
				return fmt.Errorf("duplicate job name in group %s: %s", group.GroupID, job.Name)
			}
			jobSeen[job.Name] = struct{}{}
		}
	}
	return nil
}

func envValue(lookupEnv func(string) (string, bool), key string) string {
	if lookupEnv == nil {
		return ""
	}
	value, ok := lookupEnv(key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func boolValue(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func normalizeRuntime(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return defaultRuntime, nil
	}
	switch value {
	case runtimeHostCodex, runtimeDockerCodex:
		return value, nil
	default:
		return "", fmt.Errorf("unsupported runtime: %s", value)
	}
}

func shellQuote(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func resolveCLIPath(value string, lookupEnv func(string) (string, bool)) (string, error) {
	value = expandPathValue(value, lookupEnv)
	if value == "" {
		return "", nil
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", value, err)
	}
	return filepath.Clean(abs), nil
}

func rejectDeprecatedRuntimeFields(file fileConfig) error {
	switch {
	case strings.TrimSpace(file.Runtime) != "":
		return errors.New("config field runtime was removed in v2.2; use --runtime docker-codex or --runtime host-codex")
	case strings.TrimSpace(file.RuntimeConfigDir) != "":
		return errors.New("config field runtime_config_dir was removed in v2.2; use --codex-config-dir instead")
	case strings.TrimSpace(file.SessionCommand) != "":
		return errors.New("config field session_command was removed in v2.2; imcodex now manages launch commands internally")
	default:
		return nil
	}
}

func expandPathValue(value string, lookupEnv func(string) (string, bool)) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if home := firstNonEmpty(envValue(lookupEnv, "HOME"), envValue(lookupEnv, "USERPROFILE")); home != "" {
		switch {
		case value == "~":
			value = home
		case strings.HasPrefix(value, "~/"), strings.HasPrefix(value, "~\\"):
			value = filepath.Join(home, value[2:])
		}
	}
	value = os.Expand(value, func(key string) string {
		return envValue(lookupEnv, key)
	})
	return strings.TrimSpace(value)
}

func resolveConfigRelativePath(configPath string, value string, lookupEnv func(string) (string, bool)) string {
	value = expandPathValue(value, lookupEnv)
	if value == "" || filepath.IsAbs(value) {
		return value
	}

	configDir := filepath.Dir(configPath)
	if absDir, err := filepath.Abs(configDir); err == nil {
		configDir = absDir
	}
	return filepath.Clean(filepath.Join(configDir, value))
}

func resolveWorkingDirRelativePath(cwd string, value string, lookupEnv func(string) (string, bool)) string {
	value = expandPathValue(value, lookupEnv)
	if value == "" || filepath.IsAbs(value) || strings.TrimSpace(cwd) == "" {
		return value
	}
	return filepath.Clean(filepath.Join(strings.TrimSpace(cwd), value))
}
