package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	larksdk "github.com/larksuite/oapi-sdk-go/v3"
	"gopkg.in/yaml.v3"
)

const defaultConfigPath = "imcodex.yaml"

type config struct {
	path          string
	larkAppID     string
	larkAppSecret string
	larkBaseURL   string
	groups        []groupConfig
}

type groupConfig struct {
	GroupID string `yaml:"group_id"`
	CWD     string `yaml:"cwd"`
}

type fileConfig struct {
	LarkAppID     string        `yaml:"lark_app_id"`
	LarkAppSecret string        `yaml:"lark_app_secret"`
	LarkBaseURL   string        `yaml:"lark_base_url"`
	Groups        []groupConfig `yaml:"groups"`
}

func parseConfig(args []string, lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) (config, error) {
	fs := flag.NewFlagSet("imcodex", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	path := defaultConfigPath
	fs.StringVar(&path, "config", defaultConfigPath, "Config file path")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	if readFile == nil {
		readFile = os.ReadFile
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return config{}, errors.New("required: -config")
	}

	data, err := readFile(path)
	if err != nil {
		return config{}, fmt.Errorf("read config %s: %w", path, err)
	}

	var file fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return config{}, fmt.Errorf("decode config %s: %w", path, err)
	}

	cfg := config{
		path:          path,
		larkAppID:     firstNonEmpty(file.LarkAppID, envValue(lookupEnv, "LARK_APP_ID")),
		larkAppSecret: firstNonEmpty(file.LarkAppSecret, envValue(lookupEnv, "LARK_APP_SECRET")),
		larkBaseURL:   firstNonEmpty(file.LarkBaseURL, envValue(lookupEnv, "LARK_BASE_URL"), larksdk.LarkBaseUrl),
		groups:        normalizeGroups(file.Groups),
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
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
	if c.larkAppID == "" {
		return errors.New("required: lark_app_id or LARK_APP_ID")
	}
	if c.larkAppSecret == "" {
		return errors.New("required: lark_app_secret or LARK_APP_SECRET")
	}
	if c.larkBaseURL == "" {
		return errors.New("required: lark_base_url or LARK_BASE_URL")
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
