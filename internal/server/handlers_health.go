// Copyright (c) 2026 Michael D Henderson. All rights reserved.

package server

import (
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/mdhender/ecv8-api/internal/store"
	"github.com/mdhender/ecv8-api/internal/version"
)

// handleLive reports that the process is running.
//
// It never touches the database, so a slow query cannot make a supervisor
// restart an otherwise healthy process. Use readiness for dependency health.
func (s *Server) handleLive(c *echo.Context) error {
	return c.JSON(http.StatusOK, envelope{Data: healthView{
		Status:   "ok",
		ReadOnly: s.db.ReadOnly(),
		Version:  version.Version.String(),
	}})
}

// handleReady reports whether the service can actually serve traffic.
//
// Readiness must reflect its dependencies, so it queries the database. A
// failure answers 503 with a Problem document so a proxy can drain this
// instance instead of sending it requests that will fail.
func (s *Server) handleReady(c *echo.Context) error {
	ctx := c.Request().Context()

	if err := s.db.Ping(ctx); err != nil {
		s.log.Error("readiness check failed", "error", err)
		return newProblem(http.StatusServiceUnavailable, "The database is not available.")
	}
	migration, err := s.db.MigrationVersion(ctx)
	if err != nil {
		s.log.Error("readiness check failed", "error", err)
		return newProblem(http.StatusServiceUnavailable, "The database is not available.")
	}

	latest := store.LatestMigration()
	// A writable process migrates on open, so being behind here means something
	// is wrong. A read-only process may legitimately run against a database that
	// a newer build migrated, which is why this only fails when writable.
	if migration < latest && !s.db.ReadOnly() {
		s.log.Error("readiness check failed", "migration", migration, "latest", latest)
		return newProblem(http.StatusServiceUnavailable, "The database schema is out of date.")
	}

	return c.JSON(http.StatusOK, envelope{Data: healthView{
		Status:    "ok",
		Database:  "ok",
		Migration: migration,
		Latest:    latest,
		ReadOnly:  s.db.ReadOnly(),
		Version:   version.Version.String(),
	}})
}
