package server

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const successionInitRetryCooldown = 10 * time.Second

func (s *Server) initSuccession() {
	s.successionMu.Lock()
	defer s.successionMu.Unlock()

	if s.succession != nil && s.successionErr == "" {
		return
	}
	if !s.successionInitAt.IsZero() && time.Since(s.successionInitAt) < successionInitRetryCooldown {
		return
	}
	s.successionInitAt = time.Now()

	dsn, err := ResolveNotificationsDSN(s.logger)
	if err != nil {
		s.successionErr = fmt.Sprintf("succession unavailable: %v", err)
		s.logger.Printf("%s", s.successionErr)
		return
	}

	pool := s.db
	if pool == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pool, err = pgxpool.New(ctx, dsn)
		if err != nil {
			s.successionErr = fmt.Sprintf("succession unavailable: failed to connect to postgres: %v", err)
			s.logger.Printf("%s", s.successionErr)
			return
		}
		s.db = pool
	}

	svc := NewSuccessionService(pool, s.logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.EnsureSchema(ctx); err != nil {
		s.successionErr = fmt.Sprintf("succession unavailable: failed to init schema: %v", err)
		s.logger.Printf("%s", s.successionErr)
		return
	}

	s.succession = svc
	s.successionErr = ""
}

func (s *Server) successionService() (*SuccessionService, string) {
	s.initSuccession()
	return s.succession, s.successionErr
}
