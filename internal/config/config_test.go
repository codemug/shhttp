package config

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shhttp.yaml")
	yaml := "listen: 0.0.0.0:9000\ndata_dir: /from/file\nkill_grace: 3s\nmax_sessions: 5\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(
		[]string{"--config", path, "--max-sessions", "7"},
		env(map[string]string{"SHHTTP_DATA_DIR": "/from/env", "SHHTTP_MAX_SESSIONS": "6"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:9000" {
		t.Errorf("listen = %q, want the file value", c.Listen)
	}
	if c.DataDir != "/from/env" {
		t.Errorf("data_dir = %q, want the env value", c.DataDir)
	}
	if c.MaxSessions != 7 {
		t.Errorf("max_sessions = %d, want the flag value", c.MaxSessions)
	}
	if c.KillGrace != 3*time.Second {
		t.Errorf("kill_grace = %s", c.KillGrace)
	}
	if c.DefaultRetention != 24*time.Hour {
		t.Errorf("default_retention = %s, want the default", c.DefaultRetention)
	}
	if c.MasterKeyFile != "/from/env/master.key" {
		t.Errorf("master_key_file = %q", c.MasterKeyFile)
	}
}

func TestConfigFileFromEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(path, []byte("log_format: json\n"), 0o600)
	c, err := Load(nil, env(map[string]string{"SHHTTP_CONFIG": path}))
	if err != nil || c.LogFormat != "json" {
		t.Fatalf("%+v, %v", c, err)
	}
}

func TestErrors(t *testing.T) {
	dir := t.TempDir()
	unknown := filepath.Join(dir, "unknown.yaml")
	os.WriteFile(unknown, []byte("listn: x\n"), 0o600)
	cases := []struct {
		args []string
		env  map[string]string
	}{
		{[]string{"--config", unknown}, nil},
		{[]string{"--config", filepath.Join(dir, "missing.yaml")}, nil},
		{[]string{"--tls-cert", "a"}, nil},
		{[]string{"--log-format", "xml"}, nil},
		{[]string{"--nope"}, nil},
		{[]string{"extra"}, nil},
		{nil, map[string]string{"SHHTTP_KILL_GRACE": "soon"}},
	}
	for _, c := range cases {
		if _, err := Load(c.args, env(c.env)); err == nil {
			t.Errorf("Load(%v, %v) succeeded", c.args, c.env)
		}
	}
	if _, err := Load([]string{"-h"}, env(nil)); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
}
