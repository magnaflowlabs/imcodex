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
)

type config struct {
	path                  string
	platform              string
	larkAppID             string
	larkAppSecret         string
	larkBaseURL           string
	telegramBotToken      string
	telegramBaseURL       string
	interruptOnNewMessage bool
	groups                []groupConfig
}

type groupConfig struct {
	GroupID string `yaml:"group_id"`
	CWD     string `yaml:"cwd"`
}

type fileConfig struct {
	Platform              string        `yaml:"platform"`
	LarkAppID             string        `yaml:"lark_app_id"`
	LarkAppSecret         string        `yaml:"lark_app_secret"`
	LarkBaseURL           string        `yaml:"lark_base_url"`
	TelegramBotToken      string        `yaml:"telegram_bot_token"`
	TelegramBaseURL       string        `yaml:"telegram_base_url"`
	InterruptOnNewMessage *bool         `yaml:"interrupt_on_new_message"`
	Groups                []groupConfig `yaml:"groups"`
}

func parseConfig(args []string, lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) (config, error) {
	fs := flag.NewFlagSet("imcodex", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var path string
	fs.StringVar(&path, "config", "", "Config file path")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if readFile == nil {
		readFile = os.ReadFile
	}
	path = strings.TrimSpace(path)
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

	cfg := config{
		path:                  path,
		platform:              firstNonEmpty(file.Platform, envValue(lookupEnv, "IMCODEX_PLATFORM"), defaultPlatform),
		larkAppID:             firstNonEmpty(file.LarkAppID, envValue(lookupEnv, "LARK_APP_ID")),
		larkAppSecret:         firstNonEmpty(file.LarkAppSecret, envValue(lookupEnv, "LARK_APP_SECRET")),
		larkBaseURL:           firstNonEmpty(file.LarkBaseURL, envValue(lookupEnv, "LARK_BASE_URL"), larksdk.LarkBaseUrl),
		telegramBotToken:      firstNonEmpty(file.TelegramBotToken, envValue(lookupEnv, "TELEGRAM_BOT_TOKEN")),
		telegramBaseURL:       firstNonEmpty(file.TelegramBaseURL, envValue(lookupEnv, "TELEGRAM_BASE_URL"), defaultTelegramAPIURL),
		interruptOnNewMessage: boolValue(file.InterruptOnNewMessage, true),
		groups:                normalizeGroups(file.Groups),
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

func normalizeGroups(groups []groupConfig) []groupConfig {
	out := make([]groupConfig, 0, len(groups))
	for _, group := range groups {
		out = append(out, groupConfig{
			GroupID: strings.TrimSpace(group.GroupID),
			CWD:     strings.TrimSpace(group.CWD),
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
