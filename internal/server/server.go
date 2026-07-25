// Copyright (c) 2026 Michael D Henderson. All rights reserved.

// Package server implements the ECV8 HTTP API on Echo v5.
//
// The service always runs behind a reverse proxy: nginx terminates TLS in
// production, Caddy fronts both projects in development. This listener speaks
// plain HTTP on a private address and does not attempt TLS itself.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/mdhender/ecv8-api/internal/config"
	"github.com/mdhender/ecv8-api/internal/store"
)

// Server owns the HTTP listener and everything it needs.
//
// The logger lives here and is threaded through explicitly. There is no
// package-level logger, so nothing can log outside a server's configuration.
type Server struct {
	cfg  *config.Config
	db   *store.DB
	log  *slog.Logger
	echo *echo.Echo
	http *http.Server

	// crossOrigin is the standard library's CSRF defence. It inspects
	// Sec-Fetch-Site and Origin and rejects unsafe cross-origin browser
	// requests. It is not a substitute for the session cookie's HttpOnly,
	// Secure, and SameSite attributes, which defend against different things.
	crossOrigin *http.CrossOriginProtection

	// sweepDone closes when the expired-session sweeper has stopped.
	sweepDone chan struct{}
}

// New builds a server. It does not listen; call Run.
func New(cfg *config.Config, db *store.DB, log *slog.Logger) (*Server, error) {
	if cfg == nil || db == nil || log == nil {
		return nil, errors.New("server: config, database, and logger are required")
	}

	crossOrigin := http.NewCrossOriginProtection()
	for _, origin := range cfg.TrustedOrigins {
		if err := crossOrigin.AddTrustedOrigin(origin); err != nil {
			return nil, fmt.Errorf("trusted origin %q: %w", origin, err)
		}
	}

	s := &Server{
		cfg:         cfg,
		db:          db,
		log:         log,
		crossOrigin: crossOrigin,
		sweepDone:   make(chan struct{}),
	}

	ipExtractor, err := ipExtractor(cfg)
	if err != nil {
		return nil, err
	}

	s.echo = echo.NewWithConfig(echo.Config{
		Logger:           log,
		HTTPErrorHandler: s.errorHandler,
		IPExtractor:      ipExtractor,
	})
	s.routes()

	s.http = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s.echo,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// ipExtractor decides how much of a forwarding header to believe.
//
// Trusting X-Forwarded-For unconditionally lets any client claim any address,
// which would poison logs and any address-based decision. With no configured
// proxies the socket's peer address is used and headers are ignored entirely.
func ipExtractor(cfg *config.Config) (echo.IPExtractor, error) {
	if len(cfg.TrustedProxies) == 0 {
		return echo.ExtractIPDirect(), nil
	}
	options := make([]echo.TrustOption, 0, len(cfg.TrustedProxies)+3)
	// Only the explicitly listed ranges are trusted; the convenience defaults
	// for loopback, link-local, and private networks are switched off.
	options = append(options,
		echo.TrustLoopback(false),
		echo.TrustLinkLocal(false),
		echo.TrustPrivateNet(false),
	)
	for _, cidr := range cfg.TrustedProxies {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", cidr, err)
		}
		options = append(options, echo.TrustIPRange(network))
	}
	return echo.ExtractIPFromXFFHeader(options...), nil
}

// Run serves until ctx is cancelled, then shuts down gracefully.
//
// Shutdown stops accepting new connections and waits up to ShutdownTimeout for
// in-flight requests to finish before returning.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.ListenAddr, err)
	}

	// Every request context descends from this one, so cancelling ctx unblocks
	// handlers that are waiting on the database instead of letting them run out
	// the shutdown clock.
	baseCtx, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()
	s.http.BaseContext = func(net.Listener) context.Context { return baseCtx }

	go s.sweepExpiredSessions(ctx)

	serveErr := make(chan error, 1)
	go func() {
		s.log.Info("api listening",
			"addr", listener.Addr().String(),
			"public_base_url", s.cfg.PublicBaseURL,
			"read_only", s.db.ReadOnly(),
		)
		err := s.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		cancelBase()
		<-s.sweepDone
		return err
	case <-ctx.Done():
		s.log.Info("shutdown requested", "timeout", s.cfg.ShutdownTimeout)
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()

	shutdownErr := s.http.Shutdown(shutdownCtx)
	// In-flight handlers have now finished (or the deadline passed), so it is
	// safe to cancel the base context they were sharing.
	cancelBase()
	<-s.sweepDone

	if err := <-serveErr; err != nil {
		return err
	}
	if shutdownErr != nil {
		return fmt.Errorf("graceful shutdown: %w", shutdownErr)
	}
	s.log.Info("shutdown complete")
	return nil
}

// sweepInterval is how often expired sessions are purged. Nothing depends on
// this for correctness — every session read already filters on expires_at — so
// it only needs to keep the table from growing without bound.
const sweepInterval = 15 * time.Minute

// sweepExpiredSessions deletes rows past their deadline until ctx is cancelled.
func (s *Server) sweepExpiredSessions(ctx context.Context) {
	defer close(s.sweepDone)
	if s.db.ReadOnly() {
		return
	}
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed, err := s.db.DeleteExpiredSessions(ctx, store.Now())
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("sweep expired sessions", "error", err)
				}
				continue
			}
			if removed > 0 {
				s.log.Info("swept expired sessions", "removed", removed)
			}
		}
	}
}

// routes registers every endpoint.
//
// Middleware order matters: recovery wraps everything so a panic in any later
// middleware is still converted to a 500; the body limit runs before anything
// reads a body; cross-origin protection runs before authentication so a
// rejected cross-origin write never touches the session table.
func (s *Server) routes() {
	e := s.echo

	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	e.Use(s.requestLogger())
	e.Use(middleware.BodyLimit(s.cfg.MaxBodyBytes))
	e.Use(s.crossOriginProtection)
	e.Use(s.authenticate)

	api := e.Group("/api/v1")

	// Liveness and readiness are unauthenticated so a proxy or supervisor can
	// reach them, and they expose nothing beyond up/down.
	api.GET("/health/live", s.handleLive)
	api.GET("/health/ready", s.handleReady)

	api.POST("/session", s.handleLogin)
	api.DELETE("/session", s.handleLogout)
	api.GET("/session", s.handleCurrentSession)
	api.POST("/session/impersonation", s.handleStartImpersonation, s.requireAdmin)
	api.DELETE("/session/impersonation", s.handleStopImpersonation, s.requireAuth)

	// Redeeming an activation link is necessarily unauthenticated.
	api.POST("/activation", s.handleRedeemActivation)

	me := api.Group("/me", s.requireAuth)
	me.GET("", s.handleGetProfile)
	me.PATCH("", s.handleUpdateProfile)
	me.PUT("/password", s.handleChangePassword)
	me.GET("/games", s.handleMyGames)

	admin := api.Group("/admin", s.requireAdmin)
	admin.GET("/accounts", s.handleListAccounts)
	admin.POST("/accounts", s.handleCreateAccount)
	admin.GET("/accounts/:accountID", s.handleGetAccount)
	admin.PATCH("/accounts/:accountID", s.handleUpdateAccount)
	admin.POST("/accounts/:accountID/activation-link", s.handleReissueActivation)
	admin.DELETE("/accounts/:accountID/sessions", s.handleRevokeSessions)

	admin.GET("/games", s.handleListGames)
	admin.POST("/games", s.handleCreateGame)
	admin.GET("/games/:gameID", s.handleGetGame)
	admin.PATCH("/games/:gameID", s.handleUpdateGame)
	admin.GET("/games/:gameID/memberships", s.handleListMemberships)
	admin.PUT("/games/:gameID/memberships/:accountID", s.handleSaveMembership)

	// Anything unmatched is still a Problem document rather than Echo's default
	// JSON, so the client only ever parses one error shape.
	e.RouteNotFound("/*", func(*echo.Context) error {
		return notFound("No such endpoint.")
	})
}
