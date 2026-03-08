package server

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const nodeRetirementInitRetryCooldown = 10 * time.Second

func (s *Server) initNodeRetirement() {
	s.nodeRetirementMu.Lock()
	defer s.nodeRetirementMu.Unlock()

	if s.nodeRetirement != nil && s.nodeRetirementErr == "" {
		return
	}
	if !s.nodeRetirementInitAt.IsZero() && time.Since(s.nodeRetirementInitAt) < nodeRetirementInitRetryCooldown {
		return
	}
	s.nodeRetirementInitAt = time.Now()

	dsn, err := ResolveNotificationsDSN(s.logger)
	if err != nil {
		s.nodeRetirementErr = fmt.Sprintf("node retirement unavailable: %v", err)
		s.logger.Printf("%s", s.nodeRetirementErr)
		return
	}

	pool := s.db
	if pool == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pool, err = pgxpool.New(ctx, dsn)
		if err != nil {
			s.nodeRetirementErr = fmt.Sprintf("node retirement unavailable: failed to connect to postgres: %v", err)
			s.logger.Printf("%s", s.nodeRetirementErr)
			return
		}
		s.db = pool
	}

	svc := NewNodeRetirementService(pool, s.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.EnsureSchema(ctx); err != nil {
		s.nodeRetirementErr = fmt.Sprintf("node retirement unavailable: failed to init schema: %v", err)
		s.logger.Printf("%s", s.nodeRetirementErr)
		return
	}

	s.nodeRetirement = svc
	s.nodeRetirementErr = ""
}

func (s *Server) nodeRetirementService() (*NodeRetirementService, string) {
	s.initNodeRetirement()
	return s.nodeRetirement, s.nodeRetirementErr
}
