// Package config loads the server configuration. Values come from, in
// increasing order of precedence: defaults, a YAML file (--config or
// SHHTTP_CONFIG), environment variables (SHHTTP_<FLAG NAME>) and flags.
package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the server configuration.
type Config struct {
	Listen           string        `yaml:"listen"`
	DataDir          string        `yaml:"data_dir"`
	MasterKeyFile    string        `yaml:"master_key_file"`
	TLSCert          string        `yaml:"tls_cert"`
	TLSKey           string        `yaml:"tls_key"`
	DefaultRetention time.Duration `yaml:"default_retention"`
	KillGrace        time.Duration `yaml:"kill_grace"`
	MaxSessions      int           `yaml:"max_sessions"`
	SweepInterval    time.Duration `yaml:"sweep_interval"`
	LogFormat        string        `yaml:"log_format"`
	LogLevel         string        `yaml:"log_level"`

	// configFile is only settable by flag or environment variable.
	configFile string
}

// Defaults returns the default configuration.
func Defaults() Config {
	return Config{
		Listen:           "127.0.0.1:2112",
		DataDir:          "shhttp-data",
		DefaultRetention: 24 * time.Hour,
		KillGrace:        10 * time.Second,
		SweepInterval:    time.Minute,
		LogFormat:        "text",
		LogLevel:         "info",
	}
}

// newFlagSet binds flags to c. Parse errors are returned, not printed.
func newFlagSet(c *Config) *flag.FlagSet {
	fs := flag.NewFlagSet("shhttpd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.configFile, "config", c.configFile, "YAML configuration file")
	fs.StringVar(&c.Listen, "listen", c.Listen, "address to listen on")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "directory for the database, session output and generated master key")
	fs.StringVar(&c.MasterKeyFile, "master-key-file", c.MasterKeyFile, "file holding the master key (default <data-dir>/master.key; SHHTTP_MASTER_KEY takes precedence)")
	fs.StringVar(&c.TLSCert, "tls-cert", c.TLSCert, "TLS certificate file")
	fs.StringVar(&c.TLSKey, "tls-key", c.TLSKey, "TLS private key file")
	fs.DurationVar(&c.DefaultRetention, "default-retention", c.DefaultRetention, "how long finished sessions are kept when they set no retention")
	fs.DurationVar(&c.KillGrace, "kill-grace", c.KillGrace, "time between SIGTERM and SIGKILL when stopping a session")
	fs.IntVar(&c.MaxSessions, "max-sessions", c.MaxSessions, "maximum number of running sessions (0 = unlimited)")
	fs.DurationVar(&c.SweepInterval, "sweep-interval", c.SweepInterval, "how often expired sessions are deleted")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "log format: text or json")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "log level: debug, info, warn or error")
	return fs
}

// EnvName returns the environment variable for a flag name.
func EnvName(flagName string) string {
	return "SHHTTP_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Load builds the configuration from args (without the program name) and
// the environment. It returns flag.ErrHelp when -h is given.
func Load(args []string, getenv func(string) string) (Config, error) {
	// First pass: find the config file and validate the flags.
	scratch := Defaults()
	if err := newFlagSet(&scratch).Parse(args); err != nil {
		return Config{}, err
	}
	path := scratch.configFile
	if path == "" {
		path = getenv(EnvName("config"))
	}

	c := Defaults()
	if path != "" {
		if err := c.loadFile(path); err != nil {
			return Config{}, err
		}
	}
	c.configFile = path
	fs := newFlagSet(&c)
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if v := getenv(EnvName(f.Name)); v != "" && f.Name != "config" && envErr == nil {
			if err := fs.Set(f.Name, v); err != nil {
				envErr = fmt.Errorf("%s: %w", EnvName(f.Name), err)
			}
		}
	})
	if envErr != nil {
		return Config{}, envErr
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if c.MasterKeyFile == "" {
		c.MasterKeyFile = filepath.Join(c.DataDir, "master.key")
	}
	return c, c.validate()
}

func (c *Config) loadFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (c *Config) validate() error {
	switch {
	case c.Listen == "":
		return errors.New("listen must not be empty")
	case c.DataDir == "":
		return errors.New("data_dir must not be empty")
	case (c.TLSCert == "") != (c.TLSKey == ""):
		return errors.New("tls_cert and tls_key must be set together")
	case c.DefaultRetention <= 0 || c.KillGrace <= 0 || c.SweepInterval <= 0:
		return errors.New("default_retention, kill_grace and sweep_interval must be positive")
	case c.MaxSessions < 0:
		return errors.New("max_sessions must not be negative")
	case c.LogFormat != "text" && c.LogFormat != "json":
		return fmt.Errorf("log_format must be text or json, not %q", c.LogFormat)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be debug, info, warn or error, not %q", c.LogLevel)
	}
	return nil
}

// Usage prints the flags.
func Usage(output io.Writer) {
	c := Defaults()
	fs := newFlagSet(&c)
	fs.SetOutput(output)
	fmt.Fprintf(output, "Usage: shhttpd [flags]\n       shhttpd keygen    print a new random master key\n       shhttpd version\n       shhttpd openapi   print the OpenAPI document (YAML)\n\nEvery flag can also be set with an environment variable, e.g. --data-dir as %s.\n\nFlags:\n", EnvName("data-dir"))
	fs.PrintDefaults()
}
