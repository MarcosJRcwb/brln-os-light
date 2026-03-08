package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	successionConfigID                     = 1
	successionDefaultCheckPeriodDays       = 30
	successionDefaultReminderDays          = 30
	successionDefaultStuckHTLCSec    int64 = 86400
)

var (
	ErrSuccessionDBUnavailable = errors.New("succession db unavailable")
	ErrSuccessionInvalidAction = errors.New("invalid succession simulation action")
)

type SuccessionService struct {
	db     *pgxpool.Pool
	logger *log.Logger
}

type SuccessionConfig struct {
	Enabled               bool       `json:"enabled"`
	DryRun                bool       `json:"dry_run"`
	DestinationAddress    string     `json:"destination_address"`
	PreapproveFCOffline   bool       `json:"preapprove_fc_offline"`
	PreapproveFCStuckHTLC bool       `json:"preapprove_fc_stuck_htlc"`
	StuckHTLCThresholdSec int64      `json:"stuck_htlc_threshold_sec"`
	CheckPeriodDays       int        `json:"check_period_days"`
	ReminderPeriodDays    int        `json:"reminder_period_days"`
	LastAliveAt           *time.Time `json:"last_alive_at,omitempty"`
	NextCheckAt           *time.Time `json:"next_check_at,omitempty"`
	DeadlineAt            *time.Time `json:"deadline_at,omitempty"`
	Status                string     `json:"status"`
	UpdatedAt             *time.Time `json:"updated_at,omitempty"`
}

type SuccessionConfigUpdate struct {
	Enabled               *bool
	DryRun                *bool
	DestinationAddress    *string
	PreapproveFCOffline   *bool
	PreapproveFCStuckHTLC *bool
	StuckHTLCThresholdSec *int64
	CheckPeriodDays       *int
	ReminderPeriodDays    *int
}

type SuccessionStatus struct {
	Available bool              `json:"available"`
	Config    *SuccessionConfig `json:"config,omitempty"`
}

func NewSuccessionService(db *pgxpool.Pool, logger *log.Logger) *SuccessionService {
	return &SuccessionService{db: db, logger: logger}
}

func (s *SuccessionService) Start() {}

func (s *SuccessionService) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return ErrSuccessionDBUnavailable
	}

	_, err := s.db.Exec(ctx, `
create table if not exists succession_config (
  id integer primary key,
  enabled boolean not null default false,
  dry_run boolean not null default false,
  destination_address text not null default '',
  preapprove_fc_offline boolean not null default false,
  preapprove_fc_stuck_htlc boolean not null default false,
  stuck_htlc_threshold_sec bigint not null default 86400,
  check_period_days integer not null default 30,
  reminder_period_days integer not null default 30,
  last_alive_at timestamptz,
  next_check_at timestamptz,
  deadline_at timestamptz,
  status text not null default 'disabled',
  updated_at timestamptz not null default now()
);

create table if not exists succession_events (
  id bigserial primary key,
  event_type text not null,
  source text not null default '',
  payload_json jsonb not null default '{}'::jsonb,
  created_at timestamptz not null default now()
);

create index if not exists succession_events_created_idx on succession_events (created_at desc);
`)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(ctx, `
insert into succession_config (
  id, enabled, dry_run, destination_address, preapprove_fc_offline,
  preapprove_fc_stuck_htlc, stuck_htlc_threshold_sec, check_period_days,
  reminder_period_days, status
)
values ($1, false, false, '', false, false, $2, $3, $4, 'disabled')
on conflict (id) do nothing
`, successionConfigID, successionDefaultStuckHTLCSec, successionDefaultCheckPeriodDays, successionDefaultReminderDays)
	return err
}

func (s *SuccessionService) Status(ctx context.Context) (SuccessionStatus, error) {
	if s == nil || s.db == nil {
		return SuccessionStatus{Available: false}, ErrSuccessionDBUnavailable
	}
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return SuccessionStatus{Available: false}, err
	}
	return SuccessionStatus{Available: true, Config: &cfg}, nil
}

func (s *SuccessionService) GetConfig(ctx context.Context) (SuccessionConfig, error) {
	if s == nil || s.db == nil {
		return SuccessionConfig{}, ErrSuccessionDBUnavailable
	}

	var cfg SuccessionConfig
	err := s.db.QueryRow(ctx, `
select enabled, dry_run, destination_address, preapprove_fc_offline, preapprove_fc_stuck_htlc,
  stuck_htlc_threshold_sec, check_period_days, reminder_period_days,
  last_alive_at, next_check_at, deadline_at, status, updated_at
from succession_config
where id = $1
`, successionConfigID).Scan(
		&cfg.Enabled,
		&cfg.DryRun,
		&cfg.DestinationAddress,
		&cfg.PreapproveFCOffline,
		&cfg.PreapproveFCStuckHTLC,
		&cfg.StuckHTLCThresholdSec,
		&cfg.CheckPeriodDays,
		&cfg.ReminderPeriodDays,
		scanOptionalTime(&cfg.LastAliveAt),
		scanOptionalTime(&cfg.NextCheckAt),
		scanOptionalTime(&cfg.DeadlineAt),
		&cfg.Status,
		scanOptionalTime(&cfg.UpdatedAt),
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SuccessionConfig{}, ErrSuccessionDBUnavailable
		}
		return SuccessionConfig{}, err
	}
	cfg.DestinationAddress = strings.TrimSpace(cfg.DestinationAddress)
	cfg.Status = strings.TrimSpace(cfg.Status)
	if cfg.Status == "" {
		cfg.Status = "disabled"
	}
	return cfg, nil
}

func (s *SuccessionService) UpdateConfig(ctx context.Context, update SuccessionConfigUpdate) (SuccessionConfig, error) {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return SuccessionConfig{}, err
	}

	if update.Enabled != nil {
		cfg.Enabled = *update.Enabled
	}
	if update.DryRun != nil {
		cfg.DryRun = *update.DryRun
	}
	if update.DestinationAddress != nil {
		cfg.DestinationAddress = strings.TrimSpace(*update.DestinationAddress)
	}
	if update.PreapproveFCOffline != nil {
		cfg.PreapproveFCOffline = *update.PreapproveFCOffline
	}
	if update.PreapproveFCStuckHTLC != nil {
		cfg.PreapproveFCStuckHTLC = *update.PreapproveFCStuckHTLC
	}
	if update.StuckHTLCThresholdSec != nil {
		cfg.StuckHTLCThresholdSec = *update.StuckHTLCThresholdSec
	}
	if update.CheckPeriodDays != nil {
		cfg.CheckPeriodDays = *update.CheckPeriodDays
	}
	if update.ReminderPeriodDays != nil {
		cfg.ReminderPeriodDays = *update.ReminderPeriodDays
	}

	if cfg.CheckPeriodDays <= 0 {
		cfg.CheckPeriodDays = successionDefaultCheckPeriodDays
	}
	if cfg.ReminderPeriodDays <= 0 {
		cfg.ReminderPeriodDays = successionDefaultReminderDays
	}
	if cfg.StuckHTLCThresholdSec <= 0 {
		cfg.StuckHTLCThresholdSec = successionDefaultStuckHTLCSec
	}
	if !cfg.Enabled {
		cfg.Status = "disabled"
	} else {
		cfg.Status = "armed"
	}

	now := time.Now().UTC()
	if cfg.LastAliveAt == nil {
		cfg.LastAliveAt = &now
	}
	if cfg.NextCheckAt == nil {
		next := now.AddDate(0, 0, cfg.CheckPeriodDays)
		cfg.NextCheckAt = &next
	}
	if cfg.DeadlineAt == nil {
		deadline := cfg.NextCheckAt.AddDate(0, 0, cfg.ReminderPeriodDays)
		cfg.DeadlineAt = &deadline
	}

	_, err = s.db.Exec(ctx, `
update succession_config
set enabled = $2,
  dry_run = $3,
  destination_address = $4,
  preapprove_fc_offline = $5,
  preapprove_fc_stuck_htlc = $6,
  stuck_htlc_threshold_sec = $7,
  check_period_days = $8,
  reminder_period_days = $9,
  last_alive_at = $10,
  next_check_at = $11,
  deadline_at = $12,
  status = $13,
  updated_at = now()
where id = $1
`, successionConfigID, cfg.Enabled, cfg.DryRun, cfg.DestinationAddress, cfg.PreapproveFCOffline,
		cfg.PreapproveFCStuckHTLC, cfg.StuckHTLCThresholdSec, cfg.CheckPeriodDays, cfg.ReminderPeriodDays,
		cfg.LastAliveAt, cfg.NextCheckAt, cfg.DeadlineAt, cfg.Status)
	if err != nil {
		return SuccessionConfig{}, err
	}

	_ = s.insertEvent(ctx, "config_updated", "api", map[string]any{
		"enabled": cfg.Enabled,
		"dry_run": cfg.DryRun,
	})

	return s.GetConfig(ctx)
}

func (s *SuccessionService) MarkAlive(ctx context.Context, source string) (SuccessionConfig, error) {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return SuccessionConfig{}, err
	}

	now := time.Now().UTC()
	next := now.AddDate(0, 0, cfg.CheckPeriodDays)
	deadline := next.AddDate(0, 0, cfg.ReminderPeriodDays)

	status := cfg.Status
	if cfg.Enabled {
		status = "armed"
	} else {
		status = "disabled"
	}

	_, err = s.db.Exec(ctx, `
update succession_config
set last_alive_at = $2,
  next_check_at = $3,
  deadline_at = $4,
  status = $5,
  updated_at = now()
where id = $1
`, successionConfigID, now, next, deadline, status)
	if err != nil {
		return SuccessionConfig{}, err
	}

	_ = s.insertEvent(ctx, "alive_confirmed", normalizeSuccessionSource(source), map[string]any{
		"last_alive_at": now.Format(time.RFC3339),
		"next_check_at": next.Format(time.RFC3339),
		"deadline_at":   deadline.Format(time.RFC3339),
	})
	return s.GetConfig(ctx)
}

func (s *SuccessionService) Simulate(ctx context.Context, action string, source string) error {
	normalized := strings.TrimSpace(strings.ToLower(action))
	if normalized != "alive" && normalized != "not_alive" {
		return ErrSuccessionInvalidAction
	}
	return s.insertEvent(ctx, "simulate_"+normalized, normalizeSuccessionSource(source), map[string]any{
		"action": normalized,
	})
}

func (s *SuccessionService) insertEvent(ctx context.Context, eventType string, source string, payload any) error {
	if s == nil || s.db == nil {
		return ErrSuccessionDBUnavailable
	}
	raw := []byte(`{}`)
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = encoded
	}
	_, err := s.db.Exec(ctx, `
insert into succession_events (event_type, source, payload_json)
values ($1, $2, $3::jsonb)
`, strings.TrimSpace(eventType), normalizeSuccessionSource(source), string(raw))
	return err
}

func normalizeSuccessionSource(source string) string {
	normalized := strings.TrimSpace(strings.ToLower(source))
	if normalized == "" {
		return "unknown"
	}
	return normalized
}

func nullableTimeToNull(value *time.Time) sql.NullTime {
	if value == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: value.UTC(), Valid: true}
}
