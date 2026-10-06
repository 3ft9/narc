package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const defaultMaxDuration = 6*time.Hour + 15*time.Minute

type Config struct {
	Listen  string          `toml:"listen"`
	Nomad   NomadConfig     `toml:"nomad"`
	Targets []*TargetConfig `toml:"target"`
}

type NomadConfig struct {
	Namespace string `toml:"namespace"`
	// StateVariable records the scale sets narc created, so it can delete
	// them once they leave the config. The scale set API can't list them.
	StateVariable string `toml:"state_variable"`
}

type TargetConfig struct {
	URL       string            `toml:"url"`
	Auth      AuthConfig        `toml:"auth"`
	ScaleSets []*ScaleSetConfig `toml:"scaleset"`
}

type AuthConfig struct {
	AppClientID    string `toml:"app_client_id"`
	InstallationID int64  `toml:"installation_id"`
	PrivateKeyFile string `toml:"private_key_file"`
	TokenFile      string `toml:"token_file"`
}

type ScaleSetConfig struct {
	Name        string   `toml:"name"`
	Job         string   `toml:"job"`
	Max         int      `toml:"max"`
	Warm        int      `toml:"warm"`
	Labels      []string `toml:"labels"`
	RunnerGroup string   `toml:"runner_group"`
	MaxDuration duration `toml:"max_duration"`
}

type duration struct{ time.Duration }

func (d *duration) UnmarshalText(b []byte) (err error) {
	d.Duration, err = time.ParseDuration(string(b))
	return err
}

func LoadConfig(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, err
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		return nil, fmt.Errorf("unknown config keys: %v", undec)
	}
	c.defaults()
	return &c, c.Validate()
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = ":9090"
	}
	if c.Nomad.Namespace == "" {
		c.Nomad.Namespace = "default"
	}
	if c.Nomad.StateVariable == "" {
		c.Nomad.StateVariable = "narc/state"
	}
	for _, t := range c.Targets {
		for _, s := range t.ScaleSets {
			if s.RunnerGroup == "" {
				s.RunnerGroup = "default"
			}
			if len(s.Labels) == 0 {
				s.Labels = []string{s.Name}
			}
			if s.MaxDuration.Duration == 0 {
				s.MaxDuration.Duration = defaultMaxDuration
			}
		}
	}
}

func (c *Config) Validate() error {
	var errs []error
	if len(c.Targets) == 0 {
		errs = append(errs, errors.New("at least one [[target]] is required"))
	}
	urls := map[string]bool{}
	for i, t := range c.Targets {
		where := fmt.Sprintf("target[%d] (%s)", i, t.URL)
		u := strings.TrimRight(t.URL, "/")
		if !strings.HasPrefix(u, "https://") {
			errs = append(errs, fmt.Errorf("%s: url must be an https GitHub org or repo URL", where))
		}
		if urls[u] {
			errs = append(errs, fmt.Errorf("%s: duplicate url", where))
		}
		urls[u] = true

		a := t.Auth
		app := a.AppClientID != "" || a.InstallationID != 0 || a.PrivateKeyFile != ""
		switch {
		case app && a.TokenFile != "":
			errs = append(errs, fmt.Errorf("%s: auth: set either GitHub App fields or token_file, not both", where))
		case app && (a.AppClientID == "" || a.InstallationID == 0 || a.PrivateKeyFile == ""):
			errs = append(errs, fmt.Errorf("%s: auth: app_client_id, installation_id and private_key_file are all required", where))
		case !app && a.TokenFile == "":
			errs = append(errs, fmt.Errorf("%s: auth: GitHub App or token_file required", where))
		}

		if len(t.ScaleSets) == 0 {
			errs = append(errs, fmt.Errorf("%s: at least one [[target.scaleset]] is required", where))
		}
		names := map[string]bool{}
		for j, s := range t.ScaleSets {
			sw := fmt.Sprintf("%s scaleset[%d] (%s)", where, j, s.Name)
			if s.Name == "" {
				errs = append(errs, fmt.Errorf("%s: name is required", sw))
			}
			if names[s.Name] {
				errs = append(errs, fmt.Errorf("%s: duplicate name", sw))
			}
			names[s.Name] = true
			if s.Job == "" {
				errs = append(errs, fmt.Errorf("%s: job is required", sw))
			}
			if s.Max < 1 {
				errs = append(errs, fmt.Errorf("%s: max must be at least 1", sw))
			}
			if s.Warm < 0 || s.Warm > s.Max {
				errs = append(errs, fmt.Errorf("%s: warm must be between 0 and max", sw))
			}
			if s.MaxDuration.Duration < 0 {
				errs = append(errs, fmt.Errorf("%s: max_duration must be positive", sw))
			}
		}
	}
	return errors.Join(errs...)
}

func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}
