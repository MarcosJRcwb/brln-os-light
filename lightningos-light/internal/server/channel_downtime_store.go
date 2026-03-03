package server

import (
	"context"
	"strings"
	"time"

	"lightningos-light/internal/lndclient"

	"github.com/jackc/pgx/v5"
)

func normalizeChannelPoint(point string) string {
	return strings.ToLower(strings.TrimSpace(point))
}

func (s *Server) ensureChannelDowntimeSchema(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(ctx, `
create table if not exists channel_downtime_state (
  channel_point text primary key,
  channel_id bigint not null,
  inactive_since timestamptz,
  updated_at timestamptz not null default now()
);
create index if not exists idx_channel_downtime_channel_id on channel_downtime_state(channel_id);
create index if not exists idx_channel_downtime_updated_at on channel_downtime_state(updated_at);
`)
	return err
}

func (s *Server) applyPersistedChannelDowntime(ctx context.Context, channels []lndclient.ChannelInfo) error {
	if s.db == nil || len(channels) == 0 {
		return nil
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	batch := &pgx.Batch{}
	queued := 0
	for i := range channels {
		point := normalizeChannelPoint(channels[i].ChannelPoint)
		if point == "" {
			continue
		}
		channelID := int64(channels[i].ChannelID)
		if channels[i].Active {
			batch.Queue(`
insert into channel_downtime_state (channel_point, channel_id, inactive_since, updated_at)
values ($1, $2, null, now())
on conflict (channel_point) do update
set channel_id = excluded.channel_id,
    inactive_since = null,
    updated_at = now()
`, point, channelID)
		} else {
			batch.Queue(`
insert into channel_downtime_state (channel_point, channel_id, inactive_since, updated_at)
values ($1, $2, now(), now())
on conflict (channel_point) do update
set channel_id = excluded.channel_id,
    inactive_since = coalesce(channel_downtime_state.inactive_since, excluded.inactive_since),
    updated_at = now()
`, point, channelID)
		}
		queued++
	}
	if queued > 0 {
		br := tx.SendBatch(ctx, batch)
		for i := 0; i < queued; i++ {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return err
			}
		}
		if err := br.Close(); err != nil {
			return err
		}
	}

	_, err = tx.Exec(ctx, `delete from channel_downtime_state where updated_at < now() - interval '30 day'`)
	if err != nil {
		return err
	}

	rows, err := tx.Query(ctx, `
select channel_point, extract(epoch from inactive_since)::bigint as inactive_since_unix
from channel_downtime_state
where inactive_since is not null
`)
	if err != nil {
		return err
	}
	defer rows.Close()

	inactiveSinceByPoint := make(map[string]int64)
	for rows.Next() {
		var point string
		var inactiveSinceUnix int64
		if scanErr := rows.Scan(&point, &inactiveSinceUnix); scanErr != nil {
			return scanErr
		}
		if point == "" || inactiveSinceUnix <= 0 {
			continue
		}
		inactiveSinceByPoint[point] = inactiveSinceUnix
	}
	if err := rows.Err(); err != nil {
		return err
	}

	nowUnix := time.Now().Unix()
	for i := range channels {
		if channels[i].Active {
			channels[i].InactiveSinceUnix = 0
			channels[i].InactiveDurationSec = 0
			continue
		}
		point := normalizeChannelPoint(channels[i].ChannelPoint)
		sinceUnix := inactiveSinceByPoint[point]
		if sinceUnix <= 0 {
			channels[i].InactiveSinceUnix = 0
			channels[i].InactiveDurationSec = 0
			continue
		}
		channels[i].InactiveSinceUnix = sinceUnix
		if nowUnix > sinceUnix {
			channels[i].InactiveDurationSec = nowUnix - sinceUnix
		} else {
			channels[i].InactiveDurationSec = 0
		}
	}

	return tx.Commit(ctx)
}
