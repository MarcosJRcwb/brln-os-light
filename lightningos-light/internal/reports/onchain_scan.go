package reports

import (
	"context"
	"fmt"
	"time"

	"lightningos-light/internal/lndclient"
)

func FetchOnchainFeeMetrics(ctx context.Context, lnd *lndclient.Client, startUnix uint64, endUnix uint64) (OnchainOverride, error) {
	if lnd == nil {
		return OnchainOverride{}, fmt.Errorf("lnd client unavailable")
	}
	items, err := lnd.ListOnchainTransactions(ctx, 0)
	if err != nil {
		return OnchainOverride{}, err
	}

	totals := OnchainOverride{}
	for _, tx := range items {
		ts := tx.Timestamp.Unix()
		if ts < int64(startUnix) || ts > int64(endUnix) {
			continue
		}
		if tx.FeeSat <= 0 {
			continue
		}
		totals.FeeMsat += tx.FeeSat * 1000
		totals.Count++
	}
	return totals, nil
}

func FetchOnchainFeesByDay(ctx context.Context, lnd *lndclient.Client, startUnix uint64, endUnix uint64, loc *time.Location) (map[time.Time]OnchainOverride, error) {
	if lnd == nil {
		return nil, fmt.Errorf("lnd client unavailable")
	}
	if loc == nil {
		loc = time.Local
	}
	items, err := lnd.ListOnchainTransactions(ctx, 0)
	if err != nil {
		return nil, err
	}

	results := make(map[time.Time]OnchainOverride)
	for _, tx := range items {
		ts := tx.Timestamp.Unix()
		if ts < int64(startUnix) || ts > int64(endUnix) {
			continue
		}
		if tx.FeeSat <= 0 {
			continue
		}
		local := tx.Timestamp.In(loc)
		dayKey := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		current := results[dayKey]
		current.FeeMsat += tx.FeeSat * 1000
		current.Count++
		results[dayKey] = current
	}
	return results, nil
}
