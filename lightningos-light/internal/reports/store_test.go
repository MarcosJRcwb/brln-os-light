package reports

import (
  "strings"
  "testing"
  "time"
)

func TestBuildUpsertDaily(t *testing.T) {
  reportDate := time.Date(2026, 1, 15, 0, 0, 0, 0, time.FixedZone("Local", -3*60*60))
  row := Row{
    ReportDate: reportDate,
    Metrics: Metrics{
      ForwardFeeRevenueSat: 1200,
      ForwardFeeRevenueMsat: 1200000,
      RebalanceFeeCostSat: 300,
      RebalanceFeeCostMsat: 300000,
      PaymentFeeCostSat: 100,
      PaymentFeeCostMsat: 100000,
      NetRoutingProfitSat: 800,
      NetRoutingProfitMsat: 800000,
      ForwardCount: 4,
      RebalanceCount: 2,
      PaymentCount: 3,
      RoutedVolumeSat: 18000,
      RoutedVolumeMsat: 18000000,
    },
  }

  query, args := buildUpsertDaily(row)
  if !strings.Contains(query, "on conflict (report_date) do update") {
    t.Fatalf("expected upsert query")
  }
  if !strings.Contains(query, "updated_at = now()") {
    t.Fatalf("expected updated_at update")
  }
  if len(args) != 17 {
    t.Fatalf("expected 17 args, got %d", len(args))
  }

  argDate, ok := args[0].(time.Time)
  if !ok {
    t.Fatalf("expected time arg for report date")
  }
  if argDate.Year() != 2026 || argDate.Month() != 1 || argDate.Day() != 15 {
    t.Fatalf("unexpected report date arg: %v", argDate)
  }
  if args[1] != int64(1200) || // forward_fee_revenue_sats
    args[2] != int64(1200000) || // forward_fee_revenue_msat
    args[3] != int64(300) || // rebalance_fee_cost_sats
    args[4] != int64(300000) || // rebalance_fee_cost_msat
    args[5] != int64(100) || // payment_fee_cost_sats
    args[6] != int64(100000) || // payment_fee_cost_msat
    args[7] != int64(800) || // net_routing_profit_sats
    args[8] != int64(800000) || // net_routing_profit_msat
    args[9] != int64(4) || // forward_count
    args[10] != int64(2) || // rebalance_count
    args[11] != int64(3) || // payment_count
    args[12] != int64(18000) || // routed_volume_sats
    args[13] != int64(18000000) || // routed_volume_msat
    args[14] != nil || // onchain_balance_sats
    args[15] != nil || // lightning_balance_sats
    args[16] != nil { // total_balance_sats
    t.Fatalf("unexpected metrics args")
  }
}
