// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package config defines the ECV8 API's runtime configuration.
//
// # Precedence
//
// Every setting can come from three places. Highest priority wins:
//
//  1. a command-line flag           --listen-addr 127.0.0.1:3000
//  2. an environment variable       EC_LISTEN_ADDR=127.0.0.1:3000
//  3. the built-in default
//
// Environment variables are read by ff with the EC_ prefix: a flag named
// --read-header-timeout is fed by EC_READ_HEADER_TIMEOUT.
//
// The process environment itself is populated from dotenv files before flags
// are parsed (see internal/dotenv), so a file can supply a variable but can
// never override a real environment variable or an explicit flag.
package config

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/peterbourgon/ff/v4"
)

// EnvVarPrefix is the prefix ff uses to map flags onto environment variables.
const EnvVarPrefix = "EC"

// Config is the validated runtime configuration of the API server.
type Config struct {
	// Env selects which dotenv files were loaded. It is read from EC_ENV before
	// flag parsing, so it is not itself a flag.
	Env string

	// DBPath is the directory holding ecv8.db. The filename is fixed.
	DBPath string
	// Memory, when set, runs against a named in-memory database seeded with the
	// development accounts instead of DBPath. Everything is discarded on exit.
	Memory string
	// ReadOnly opens the database in SQLite read-only mode. Write endpoints
	// then fail rather than silently doing nothing.
	ReadOnly bool

	ListenAddr    string
	PublicBaseURL string

	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration

	// MaxBodyBytes bounds a JSON request body. Anything larger is rejected
	// before a handler sees it.
	MaxBodyBytes int64

	// SessionTTL is the absolute lifetime of a session: it expires this long
	// after login no matter how active it is.
	SessionTTL time.Duration
	// SessionIdleTTL is the inactivity window. Each authenticated request slides
	// the deadline forward, but never past the absolute lifetime.
	SessionIdleTTL time.Duration

	CookieName     string
	CookieSecure   bool
	CookieSameSite string

	// TrustedProxies are the CIDR blocks whose X-Forwarded-For headers may be
	// believed. Empty means trust nobody and use the socket's peer address.
	TrustedProxies []string
	// TrustedOrigins are extra origins allowed by cross-origin protection.
	// A same-origin deployment needs none.
	TrustedOrigins []string

	LogLevel  string
	LogFormat string
}

// Default returns the configuration used when nothing is supplied.
func Default() Config {
	return Config{
		Env:               "development",
		DBPath:            "db",
		ListenAddr:        "127.0.0.1:3000",
		PublicBaseURL:     "http://localhost:8081",
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ShutdownTimeout:   20 * time.Second,
		MaxBodyBytes:      1 << 20, // 1 MiB
		SessionTTL:        7 * 24 * time.Hour,
		SessionIdleTTL:    24 * time.Hour,
		CookieName:        "ec_session",
		CookieSecure:      true,
		CookieSameSite:    "lax",
		LogLevel:          "info",
		LogFormat:         "text",
	}
}

// Bind registers every server flag on fs and returns the Config the flags write
// into. The returned Config is only valid after fs has been parsed and
// Validate has succeeded.
func Bind(fs *ff.FlagSet) *Config {
	cfg := Default()
	def := Default()

	fs.StringVar(&cfg.DBPath, 0, "db-path", def.DBPath,
		"directory containing ecv8.db; the filename is fixed and is never created by opening")
	fs.StringVar(&cfg.Memory, 0, "memory", def.Memory,
		"serve a seeded in-memory database with this name instead of --db-path; for development only")
	fs.BoolVarDefault(&cfg.ReadOnly, 0, "read-only", def.ReadOnly,
		"open the database read-only; write endpoints will fail")

	fs.StringVar(&cfg.ListenAddr, 0, "listen-addr", def.ListenAddr,
		"host:port for the private HTTP listener; TLS is terminated by the reverse proxy")
	fs.StringVar(&cfg.PublicBaseURL, 0, "public-base-url", def.PublicBaseURL,
		"origin browsers use, e.g. https://ec.example.com; activation links are built from it")

	fs.DurationVar(&cfg.ReadTimeout, 0, "read-timeout", def.ReadTimeout,
		"maximum duration for reading an entire request")
	fs.DurationVar(&cfg.ReadHeaderTimeout, 0, "read-header-timeout", def.ReadHeaderTimeout,
		"maximum duration for reading request headers")
	fs.DurationVar(&cfg.WriteTimeout, 0, "write-timeout", def.WriteTimeout,
		"maximum duration before timing out a response write")
	fs.DurationVar(&cfg.IdleTimeout, 0, "idle-timeout", def.IdleTimeout,
		"maximum time to wait for the next request on a keep-alive connection")
	fs.DurationVar(&cfg.ShutdownTimeout, 0, "shutdown-timeout", def.ShutdownTimeout,
		"how long graceful shutdown waits for in-flight requests")

	fs.Int64Var(&cfg.MaxBodyBytes, 0, "max-body-bytes", def.MaxBodyBytes,
		"maximum accepted request body size in bytes")

	fs.DurationVar(&cfg.SessionTTL, 0, "session-ttl", def.SessionTTL,
		"absolute session lifetime measured from login")
	fs.DurationVar(&cfg.SessionIdleTTL, 0, "session-idle-ttl", def.SessionIdleTTL,
		"session inactivity window; never extends past the absolute lifetime")

	fs.StringVar(&cfg.CookieName, 0, "cookie-name", def.CookieName,
		"name of the session cookie")
	fs.BoolVarDefault(&cfg.CookieSecure, 0, "cookie-secure", def.CookieSecure,
		"set Secure on the session cookie; disable only for plain-http local development")
	fs.StringEnumVar(&cfg.CookieSameSite, 0, "cookie-samesite",
		"SameSite policy for the session cookie", "lax", "strict", "none")

	fs.StringListVar(&cfg.TrustedProxies, 0, "trusted-proxy",
		"CIDR whose forwarding headers may be trusted; repeatable, empty trusts none")
	fs.StringListVar(&cfg.TrustedOrigins, 0, "trusted-origin",
		"extra origin accepted by cross-origin protection; repeatable, unnecessary when same-origin")

	fs.StringEnumVar(&cfg.LogLevel, 0, "log-level", "minimum log level",
		"info", "debug", "warn", "error")
	fs.StringEnumVar(&cfg.LogFormat, 0, "log-format", "log output format", "text", "json")

	return &cfg
}

// Validate checks the parsed configuration and reports the first problem in
// terms an operator can act on.
func Validate(cfg *Config) error {
	if strings.TrimSpace(cfg.DBPath) == "" && cfg.Memory == "" {
		return fmt.Errorf("--db-path is required")
	}
	if cfg.Memory != "" && cfg.ReadOnly {
		return fmt.Errorf("--memory and --read-only are mutually exclusive: " +
			"an in-memory database has to be written before it can be read")
	}
	if _, _, err := net.SplitHostPort(cfg.ListenAddr); err != nil {
		return fmt.Errorf("--listen-addr %q is not host:port: %w", cfg.ListenAddr, err)
	}

	base, err := url.Parse(cfg.PublicBaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return fmt.Errorf("--public-base-url %q must be an absolute URL such as https://ec.example.com", cfg.PublicBaseURL)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return fmt.Errorf("--public-base-url scheme must be http or https, got %q", base.Scheme)
	}

	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"--read-timeout", cfg.ReadTimeout},
		{"--read-header-timeout", cfg.ReadHeaderTimeout},
		{"--write-timeout", cfg.WriteTimeout},
		{"--idle-timeout", cfg.IdleTimeout},
		{"--shutdown-timeout", cfg.ShutdownTimeout},
		{"--session-ttl", cfg.SessionTTL},
		{"--session-idle-ttl", cfg.SessionIdleTTL},
	} {
		if d.value <= 0 {
			return fmt.Errorf("%s must be positive, got %s", d.name, d.value)
		}
	}
	if cfg.SessionIdleTTL > cfg.SessionTTL {
		return fmt.Errorf("--session-idle-ttl (%s) must not exceed --session-ttl (%s)",
			cfg.SessionIdleTTL, cfg.SessionTTL)
	}
	if cfg.MaxBodyBytes <= 0 {
		return fmt.Errorf("--max-body-bytes must be positive, got %d", cfg.MaxBodyBytes)
	}
	if strings.TrimSpace(cfg.CookieName) == "" {
		return fmt.Errorf("--cookie-name is required")
	}

	// SameSite=None is only meaningful on a Secure cookie, and browsers reject
	// the combination without it. Refusing it here beats a silent auth failure.
	if cfg.SameSite() == http.SameSiteNoneMode && !cfg.CookieSecure {
		return fmt.Errorf("--cookie-samesite=none requires --cookie-secure")
	}
	if cfg.CookieSecure && base.Scheme == "http" && !isLoopbackURL(base) {
		return fmt.Errorf("--cookie-secure is set but --public-base-url %q is plain http; "+
			"browsers will discard the session cookie", cfg.PublicBaseURL)
	}

	for _, cidr := range cfg.TrustedProxies {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("--trusted-proxy %q is not a CIDR block: %w", cidr, err)
		}
	}
	for _, origin := range cfg.TrustedOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" {
			return fmt.Errorf("--trusted-origin %q must be a scheme://host[:port] origin", origin)
		}
	}
	return nil
}

// SameSite converts the configured policy to its net/http value.
func (cfg *Config) SameSite() http.SameSite {
	switch strings.ToLower(cfg.CookieSameSite) {
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}

// ActivationURL builds the magic link an administrator sends to an invitee.
func (cfg *Config) ActivationURL(token string) string {
	base := strings.TrimRight(cfg.PublicBaseURL, "/")
	return base + "/activate?token=" + url.QueryEscape(token)
}

// isLoopbackURL reports whether u points at the local machine, where a browser
// still treats plain http as a secure context.
func isLoopbackURL(u *url.URL) bool {
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
