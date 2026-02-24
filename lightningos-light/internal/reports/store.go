package reports

import (
  "context"
  "errors"
  "time"

  "github.com/jackc/pgx/v5"
  "github.com/jackc/pgx/v5/pgtype"
  "github.com/jackc/pgx/v5/pgxpool"
)

func EnsureSchema(ctx context.Context, db *pgxpool.Pool) error {
  if db == nil {
    return nil
  }
  _, err := db.Exec(ctx, `
create table if not exists reports_daily (
  report_date date primary key,
  forward_fee_revenue_sats bigint not null default 0,
  forward_fee_revenue_msat bigint not null default 0,
  rebalance_fee_cost_sats bigint not null default 0,
  rebalance_fee_cost_msat bigint not null default 0,
  payment_fee_cost_sats bigint not null default 0,
  payment_fee_cost_msat bigint not null default 0,
  net_routing_profit_sats bigint not null default 0,
  net_routing_profit_msat bigint not null default 0,
  forward_count integer not null default 0,
  rebalance_count integer not null default 0,
  payment_count integer not null default 0,
  routed_volume_sats bigint not null default 0,
  routed_volume_msat bigint not null default 0,
  onchain_balance_sats bigint null,
  lightning_balance_sats bigint null,
  total_balance_sats bigint null,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table if not exists reports_movement_daily (
  report_date date primary key,
  outbound_target_sats bigint not null default 0,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

alter table reports_daily add column if not exists forward_fee_revenue_msat bigint not null default 0;
alter table reports_daily add column if not exists rebalance_fee_cost_msat bigint not null default 0;
alter table reports_daily add column if not exists payment_fee_cost_sats bigint not null default 0;
alter table reports_daily add column if not exists payment_fee_cost_msat bigint not null default 0;
alter table reports_daily add column if not exists net_routing_profit_msat bigint not null default 0;
alter table reports_daily add column if not exists payment_count integer not null default 0;
alter table reports_daily add column if not exists routed_volume_msat bigint not null default 0;
`)
  return err
}

func UpsertDaily(ctx context.Context, db *pgxpool.Pool, row Row) error {
  if db == nil {
    return nil
  }
  query, args := buildUpsertDaily(row)
  _, err := db.Exec(ctx, query, args...)
  return err
}

func buildUpsertDaily(row Row) (string, []any) {
  reportDate := normalizeReportDate(row.ReportDate)
  metrics := row.Metrics

  args := []any{
    reportDate,
    metrics.ForwardFeeRevenueSat,
    metrics.ForwardFeeRevenueMsat,
    metrics.RebalanceFeeCostSat,
    metrics.RebalanceFeeCostMsat,
    metrics.PaymentFeeCostSat,
    metrics.PaymentFeeCostMsat,
    metrics.NetRoutingProfitSat,
    metrics.NetRoutingProfitMsat,
    metrics.ForwardCount,
    metrics.RebalanceCount,
    metrics.PaymentCount,
    metrics.RoutedVolumeSat,
    metrics.RoutedVolumeMsat,
    nullableInt64(metrics.OnchainBalanceSat),
    nullableInt64(metrics.LightningBalanceSat),
    nullableInt64(metrics.TotalBalanceSat),
  }

  query := `
insert into reports_daily (
  report_date,
  forward_fee_revenue_sats,
  forward_fee_revenue_msat,
  rebalance_fee_cost_sats,
  rebalance_fee_cost_msat,
  payment_fee_cost_sats,
  payment_fee_cost_msat,
  net_routing_profit_sats,
  net_routing_profit_msat,
  forward_count,
  rebalance_count,
  payment_count,
  routed_volume_sats,
  routed_volume_msat,
  onchain_balance_sats,
  lightning_balance_sats,
  total_balance_sats
) values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
on conflict (report_date) do update set
  forward_fee_revenue_sats = excluded.forward_fee_revenue_sats,
  forward_fee_revenue_msat = excluded.forward_fee_revenue_msat,
  rebalance_fee_cost_sats = excluded.rebalance_fee_cost_sats,
  rebalance_fee_cost_msat = excluded.rebalance_fee_cost_msat,
  payment_fee_cost_sats = excluded.payment_fee_cost_sats,
  payment_fee_cost_msat = excluded.payment_fee_cost_msat,
  net_routing_profit_sats = excluded.net_routing_profit_sats,
  net_routing_profit_msat = excluded.net_routing_profit_msat,
  forward_count = excluded.forward_count,
  rebalance_count = excluded.rebalance_count,
  payment_count = excluded.payment_count,
  routed_volume_sats = excluded.routed_volume_sats,
  routed_volume_msat = excluded.routed_volume_msat,
  onchain_balance_sats = excluded.onchain_balance_sats,
  lightning_balance_sats = excluded.lightning_balance_sats,
  total_balance_sats = excluded.total_balance_sats,
  updated_at = now()
`

  return query, args
}

func UpsertMovementTargetDaily(ctx context.Context, db *pgxpool.Pool, reportDate time.Time, outboundTargetSat int64) error {
  if db == nil {
    return nil
  }
  _, err := db.Exec(ctx, `
insert into reports_movement_daily (report_date, outbound_target_sats)
values ($1, $2)
on conflict (report_date) do update set
  outbound_target_sats = excluded.outbound_target_sats,
  updated_at = now()
`, normalizeReportDate(reportDate), outboundTargetSat)
  return err
}

func FetchMovementTargetDaily(ctx context.Context, db *pgxpool.Pool, reportDate time.Time) (int64, bool, error) {
  if db == nil {
    return 0, false, nil
  }
  var target int64
  err := db.QueryRow(ctx, `
select outbound_target_sats
from reports_movement_daily
where report_date = $1
`, normalizeReportDate(reportDate)).Scan(&target)
  if err != nil {
    if isNotFound(err) {
      return 0, false, nil
    }
    return 0, false, err
  }
  return target, true, nil
}

func FetchMovementTargetRangeSum(ctx context.Context, db *pgxpool.Pool, startDate, endDate time.Time) (int64, error) {
  if db == nil {
    return 0, nil
  }
  var total int64
  err := db.QueryRow(ctx, `
select coalesce(sum(outbound_target_sats), 0)
from reports_movement_daily
where report_date >= $1 and report_date <= $2
`, normalizeReportDate(startDate), normalizeReportDate(endDate)).Scan(&total)
  if err != nil {
    return 0, err
  }
  return total, nil
}

func FetchMovementTargetAllSum(ctx context.Context, db *pgxpool.Pool) (int64, error) {
  if db == nil {
    return 0, nil
  }
  var total int64
  err := db.QueryRow(ctx, `
select coalesce(sum(outbound_target_sats), 0)
from reports_movement_daily
`).Scan(&total)
  if err != nil {
    return 0, err
  }
  return total, nil
}

func FetchRange(ctx context.Context, db *pgxpool.Pool, startDate, endDate time.Time) ([]Row, error) {
  if db == nil {
    return nil, nil
  }
  rows, err := db.Query(ctx, `
select report_date,
  forward_fee_revenue_sats,
  forward_fee_revenue_msat,
  rebalance_fee_cost_sats,
  rebalance_fee_cost_msat,
  payment_fee_cost_sats,
  payment_fee_cost_msat,
  net_routing_profit_sats,
  net_routing_profit_msat,
  forward_count,
  rebalance_count,
  payment_count,
  routed_volume_sats,
  routed_volume_msat,
  onchain_balance_sats,
  lightning_balance_sats,
  total_balance_sats
from reports_daily
where report_date >= $1 and report_date <= $2
order by report_date asc
`, normalizeReportDate(startDate), normalizeReportDate(endDate))
  if err != nil {
    return nil, err
  }
  defer rows.Close()

  var items []Row
  for rows.Next() {
    row, err := scanRow(rows)
    if err != nil {
      return nil, err
    }
    items = append(items, row)
  }
  return items, rows.Err()
}

func FetchAll(ctx context.Context, db *pgxpool.Pool) ([]Row, error) {
  if db == nil {
    return nil, nil
  }
  rows, err := db.Query(ctx, `
select report_date,
  forward_fee_revenue_sats,
  forward_fee_revenue_msat,
  rebalance_fee_cost_sats,
  rebalance_fee_cost_msat,
  payment_fee_cost_sats,
  payment_fee_cost_msat,
  net_routing_profit_sats,
  net_routing_profit_msat,
  forward_count,
  rebalance_count,
  payment_count,
  routed_volume_sats,
  routed_volume_msat,
  onchain_balance_sats,
  lightning_balance_sats,
  total_balance_sats
from reports_daily
order by report_date asc
`)
  if err != nil {
    return nil, err
  }
  defer rows.Close()

  var items []Row
  for rows.Next() {
    row, err := scanRow(rows)
    if err != nil {
      return nil, err
    }
    items = append(items, row)
  }
  return items, rows.Err()
}

func FetchSummaryRange(ctx context.Context, db *pgxpool.Pool, startDate, endDate time.Time) (Summary, error) {
  if db == nil {
    return Summary{}, nil
  }
  var days int64
  totals := Metrics{}
  err := db.QueryRow(ctx, `
select
  count(*),
  coalesce(sum(forward_fee_revenue_sats), 0),
  coalesce(sum(forward_fee_revenue_msat), 0),
  coalesce(sum(rebalance_fee_cost_sats), 0),
  coalesce(sum(rebalance_fee_cost_msat), 0),
  coalesce(sum(payment_fee_cost_sats), 0),
  coalesce(sum(payment_fee_cost_msat), 0),
  coalesce(sum(net_routing_profit_sats), 0),
  coalesce(sum(net_routing_profit_msat), 0),
  coalesce(sum(forward_count), 0),
  coalesce(sum(rebalance_count), 0),
  coalesce(sum(payment_count), 0),
  coalesce(sum(routed_volume_sats), 0),
  coalesce(sum(routed_volume_msat), 0)
from reports_daily
where report_date >= $1 and report_date <= $2
`, normalizeReportDate(startDate), normalizeReportDate(endDate)).Scan(
    &days,
    &totals.ForwardFeeRevenueSat,
    &totals.ForwardFeeRevenueMsat,
    &totals.RebalanceFeeCostSat,
    &totals.RebalanceFeeCostMsat,
    &totals.PaymentFeeCostSat,
    &totals.PaymentFeeCostMsat,
    &totals.NetRoutingProfitSat,
    &totals.NetRoutingProfitMsat,
    &totals.ForwardCount,
    &totals.RebalanceCount,
    &totals.PaymentCount,
    &totals.RoutedVolumeSat,
    &totals.RoutedVolumeMsat,
  )
  if err != nil {
    return Summary{}, err
  }

  fillMsatFromSat(&totals)
  return Summary{Days: days, Totals: totals, Averages: averageMetrics(totals, days)}, nil
}

func FetchSummaryAll(ctx context.Context, db *pgxpool.Pool) (Summary, error) {
  if db == nil {
    return Summary{}, nil
  }
  var days int64
  totals := Metrics{}
  err := db.QueryRow(ctx, `
select
  count(*),
  coalesce(sum(forward_fee_revenue_sats), 0),
  coalesce(sum(forward_fee_revenue_msat), 0),
  coalesce(sum(rebalance_fee_cost_sats), 0),
  coalesce(sum(rebalance_fee_cost_msat), 0),
  coalesce(sum(payment_fee_cost_sats), 0),
  coalesce(sum(payment_fee_cost_msat), 0),
  coalesce(sum(net_routing_profit_sats), 0),
  coalesce(sum(net_routing_profit_msat), 0),
  coalesce(sum(forward_count), 0),
  coalesce(sum(rebalance_count), 0),
  coalesce(sum(payment_count), 0),
  coalesce(sum(routed_volume_sats), 0),
  coalesce(sum(routed_volume_msat), 0)
from reports_daily
`).Scan(
    &days,
    &totals.ForwardFeeRevenueSat,
    &totals.ForwardFeeRevenueMsat,
    &totals.RebalanceFeeCostSat,
    &totals.RebalanceFeeCostMsat,
    &totals.PaymentFeeCostSat,
    &totals.PaymentFeeCostMsat,
    &totals.NetRoutingProfitSat,
    &totals.NetRoutingProfitMsat,
    &totals.ForwardCount,
    &totals.RebalanceCount,
    &totals.PaymentCount,
    &totals.RoutedVolumeSat,
    &totals.RoutedVolumeMsat,
  )
  if err != nil {
    return Summary{}, err
  }

  fillMsatFromSat(&totals)
  return Summary{Days: days, Totals: totals, Averages: averageMetrics(totals, days)}, nil
}

func averageMetrics(totals Metrics, days int64) Metrics {
  if days <= 0 {
    return Metrics{}
  }
  return Metrics{
    ForwardFeeRevenueSat: totals.ForwardFeeRevenueSat / days,
    ForwardFeeRevenueMsat: totals.ForwardFeeRevenueMsat / days,
    RebalanceFeeCostSat: totals.RebalanceFeeCostSat / days,
    RebalanceFeeCostMsat: totals.RebalanceFeeCostMsat / days,
    PaymentFeeCostSat: totals.PaymentFeeCostSat / days,
    PaymentFeeCostMsat: totals.PaymentFeeCostMsat / days,
    NetRoutingProfitSat: totals.NetRoutingProfitSat / days,
    NetRoutingProfitMsat: totals.NetRoutingProfitMsat / days,
    ForwardCount: totals.ForwardCount / days,
    RebalanceCount: totals.RebalanceCount / days,
    PaymentCount: totals.PaymentCount / days,
    RoutedVolumeSat: totals.RoutedVolumeSat / days,
    RoutedVolumeMsat: totals.RoutedVolumeMsat / days,
  }
}

type rowScanner interface {
  Scan(dest ...any) error
}

func scanRow(scanner rowScanner) (Row, error) {
  var reportDate time.Time
  var metrics Metrics
  var onchain pgtype.Int8
  var lightning pgtype.Int8
  var total pgtype.Int8
  err := scanner.Scan(
    &reportDate,
    &metrics.ForwardFeeRevenueSat,
    &metrics.ForwardFeeRevenueMsat,
    &metrics.RebalanceFeeCostSat,
    &metrics.RebalanceFeeCostMsat,
    &metrics.PaymentFeeCostSat,
    &metrics.PaymentFeeCostMsat,
    &metrics.NetRoutingProfitSat,
    &metrics.NetRoutingProfitMsat,
    &metrics.ForwardCount,
    &metrics.RebalanceCount,
    &metrics.PaymentCount,
    &metrics.RoutedVolumeSat,
    &metrics.RoutedVolumeMsat,
    &onchain,
    &lightning,
    &total,
  )
  if err != nil {
    return Row{}, err
  }
  if onchain.Valid {
    val := onchain.Int64
    metrics.OnchainBalanceSat = &val
  }
  if lightning.Valid {
    val := lightning.Int64
    metrics.LightningBalanceSat = &val
  }
  if total.Valid {
    val := total.Int64
    metrics.TotalBalanceSat = &val
  }
  fillMsatFromSat(&metrics)
  return Row{ReportDate: reportDate, Metrics: metrics}, nil
}

func nullableInt64(value *int64) any {
  if value == nil {
    return nil
  }
  return *value
}

func normalizeReportDate(value time.Time) time.Time {
  return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func fillMsatFromSat(metrics *Metrics) {
  if metrics == nil {
    return
  }
  if metrics.ForwardFeeRevenueMsat == 0 && metrics.ForwardFeeRevenueSat != 0 {
    metrics.ForwardFeeRevenueMsat = metrics.ForwardFeeRevenueSat * 1000
  }
  if metrics.RebalanceFeeCostMsat == 0 && metrics.RebalanceFeeCostSat != 0 {
    metrics.RebalanceFeeCostMsat = metrics.RebalanceFeeCostSat * 1000
  }
  if metrics.PaymentFeeCostMsat == 0 && metrics.PaymentFeeCostSat != 0 {
    metrics.PaymentFeeCostMsat = metrics.PaymentFeeCostSat * 1000
  }
  if metrics.NetRoutingProfitMsat == 0 && metrics.NetRoutingProfitSat != 0 {
    metrics.NetRoutingProfitMsat = metrics.NetRoutingProfitSat * 1000
  }
  if metrics.RoutedVolumeMsat == 0 && metrics.RoutedVolumeSat != 0 {
    metrics.RoutedVolumeMsat = metrics.RoutedVolumeSat * 1000
  }
}

func isNotFound(err error) bool {
  return errors.Is(err, pgx.ErrNoRows)
}
