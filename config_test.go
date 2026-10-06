package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadString(t *testing.T, s string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "narc.toml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(p)
}

func TestConfigExample(t *testing.T) {
	c, err := LoadConfig("examples/narc.toml")
	if err != nil {
		t.Fatal(err)
	}
	s := c.Targets[0].ScaleSets[0]
	if c.Listen != ":9090" || c.Nomad.Namespace != "default" || s.RunnerGroup != "default" ||
		s.Labels[0] != s.Name || s.MaxDuration.Duration != defaultMaxDuration {
		t.Errorf("defaults not applied: %+v %+v", c, s)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]struct{ toml, wantErr string }{
		"ok pat": {`
[[target]]
url = "https://github.com/o/r"
auth = { token_file = "x" }
[[target.scaleset]]
name = "a"
job = "j"
max = 2
warm = 2
max_duration = "1h"`, ""},
		"no targets":      {``, "at least one [[target]]"},
		"http url":        {`[[target]]` + "\nurl = \"http://github.com/o\"\nauth = { token_file = \"x\" }\n[[target.scaleset]]\nname=\"a\"\njob=\"j\"\nmax=1", "https"},
		"both auths":      {`[[target]]` + "\nurl = \"https://github.com/o\"\nauth = { token_file = \"x\", app_client_id = \"c\" }\n[[target.scaleset]]\nname=\"a\"\njob=\"j\"\nmax=1", "not both"},
		"partial app":     {`[[target]]` + "\nurl = \"https://github.com/o\"\nauth = { app_client_id = \"c\" }\n[[target.scaleset]]\nname=\"a\"\njob=\"j\"\nmax=1", "all required"},
		"no auth":         {`[[target]]` + "\nurl = \"https://github.com/o\"\n[[target.scaleset]]\nname=\"a\"\njob=\"j\"\nmax=1", "token_file required"},
		"warm over max":   {`[[target]]` + "\nurl = \"https://github.com/o\"\nauth = { token_file = \"x\" }\n[[target.scaleset]]\nname=\"a\"\njob=\"j\"\nmax=1\nwarm=2", "warm"},
		"zero max":        {`[[target]]` + "\nurl = \"https://github.com/o\"\nauth = { token_file = \"x\" }\n[[target.scaleset]]\nname=\"a\"\njob=\"j\"", "max must"},
		"dup scale set":   {`[[target]]` + "\nurl = \"https://github.com/o\"\nauth = { token_file = \"x\" }\n[[target.scaleset]]\nname=\"a\"\njob=\"j\"\nmax=1\n[[target.scaleset]]\nname=\"a\"\njob=\"j\"\nmax=1", "duplicate name"},
		"unknown key":     {`lisen = ":1"`, "unknown config keys"},
		"bad duration":    {`[[target]]` + "\nurl = \"https://github.com/o\"\nauth = { token_file = \"x\" }\n[[target.scaleset]]\nname=\"a\"\njob=\"j\"\nmax=1\nmax_duration=\"6 hours\"", "duration"},
		"missing job":     {`[[target]]` + "\nurl = \"https://github.com/o\"\nauth = { token_file = \"x\" }\n[[target.scaleset]]\nname=\"a\"\nmax=1", "job is required"},
		"empty scalesets": {`[[target]]` + "\nurl = \"https://github.com/o\"\nauth = { token_file = \"x\" }", "at least one [[target.scaleset]]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := loadString(t, tc.toml)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if c.Targets[0].ScaleSets[0].MaxDuration.Duration != time.Hour {
					t.Error("max_duration not parsed")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
