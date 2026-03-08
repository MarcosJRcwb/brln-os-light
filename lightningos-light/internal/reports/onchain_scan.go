package reports

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"lightningos-light/internal/lndclient"
	"lightningos-light/lnrpc"
)

const (
	onchainTxLookupBaseURLEnv     = "REPORTS_ONCHAIN_TX_LOOKUP_BASE_URL"
	onchainTxLookupTimeoutSecEnv  = "REPORTS_ONCHAIN_TX_LOOKUP_TIMEOUT_SEC"
	defaultOnchainTxLookupBaseURL = "https://mempool.space/api"
	defaultOnchainTxLookupTimeout = 8 * time.Second
)

type onchainCloseCostBucket uint8

const (
	onchainCloseCostBucketNone onchainCloseCostBucket = iota
	onchainCloseCostBucketCoop
	onchainCloseCostBucketLocalForce
	onchainCloseCostBucketRemoteForce
)

type onchainTxMeta struct {
	FeeSat    int64
	Timestamp time.Time
	HasTime   bool
}

type onchainTxLookupResponse struct {
	Fee    int64 `json:"fee"`
	Status struct {
		Confirmed bool  `json:"confirmed"`
		BlockTime int64 `json:"block_time"`
	} `json:"status"`
}

type onchainTxLookupCacheItem struct {
	Ready     bool
	FeeSat    int64
	Timestamp time.Time
	OK        bool
}

type onchainTxLookupClient struct {
	baseURL string
	client  *http.Client
	cache   map[string]onchainTxLookupCacheItem
}

func FetchOnchainFeeMetrics(ctx context.Context, lnd *lndclient.Client, startUnix uint64, endUnix uint64) (OnchainOverride, error) {
	totals, _, err := fetchOnchainFees(ctx, lnd, startUnix, endUnix, time.Local)
	return totals, err
}

func FetchOnchainFeesByDay(ctx context.Context, lnd *lndclient.Client, startUnix uint64, endUnix uint64, loc *time.Location) (map[time.Time]OnchainOverride, error) {
	if loc == nil {
		loc = time.Local
	}
	_, byDay, err := fetchOnchainFees(ctx, lnd, startUnix, endUnix, loc)
	return byDay, err
}

func fetchOnchainFees(ctx context.Context, lnd *lndclient.Client, startUnix uint64, endUnix uint64, loc *time.Location) (OnchainOverride, map[time.Time]OnchainOverride, error) {
	if lnd == nil {
		return OnchainOverride{}, nil, fmt.Errorf("lnd client unavailable")
	}
	if loc == nil {
		loc = time.Local
	}

	items, err := lnd.ListOnchainTransactions(ctx, 0)
	if err != nil {
		return OnchainOverride{}, nil, err
	}

	totals := OnchainOverride{}
	byDay := make(map[time.Time]OnchainOverride)
	txMetaByID := make(map[string]onchainTxMeta, len(items))

	for _, tx := range items {
		tsUnix := tx.Timestamp.Unix()
		if tsUnix < int64(startUnix) || tsUnix > int64(endUnix) {
			continue
		}

		txid := normalizeTxid(tx.Txid)
		meta := txMetaByID[txid]
		if tx.FeeSat > 0 {
			meta.FeeSat = tx.FeeSat
			accumulateOnchainTotals(&totals, byDay, tx.Timestamp, loc, tx.FeeSat)
		}
		if !meta.HasTime {
			meta.Timestamp = tx.Timestamp
			meta.HasTime = true
		}
		if txid != "" {
			txMetaByID[txid] = meta
		}
	}

	closedChannels, err := lnd.ListClosedChannels(ctx)
	if err != nil {
		return totals, byDay, nil
	}

	closureTypeByTxid := make(map[string]onchainCloseCostBucket, len(closedChannels)*2)
	for _, channel := range closedChannels {
		closeBucket := closeBucketFromLndCloseType(channel.CloseType)
		addClosureTx(closureTypeByTxid, channel.ClosingTxHash, closeBucket)
		for _, resolution := range channel.Resolutions {
			addClosureTx(closureTypeByTxid, resolution.SweepTxid, closeBucket)
		}
	}

	lookup := newOnchainTxLookupClient()
	for txid, bucket := range closureTypeByTxid {
		meta, found := txMetaByID[txid]

		feeSat := int64(0)
		timestamp := time.Time{}
		hasTime := false
		if found {
			feeSat = meta.FeeSat
			timestamp = meta.Timestamp
			hasTime = meta.HasTime
		}

		if feeSat <= 0 {
			lookupFee, lookupTime, ok := lookup.Lookup(ctx, txid)
			if !ok || lookupFee <= 0 {
				continue
			}
			feeSat = lookupFee
			if !hasTime && !lookupTime.IsZero() {
				timestamp = lookupTime
				hasTime = true
			}
		}

		if !hasTime || feeSat <= 0 {
			continue
		}
		txUnix := timestamp.Unix()
		if txUnix < int64(startUnix) || txUnix > int64(endUnix) {
			continue
		}

		if meta.FeeSat <= 0 {
			accumulateOnchainTotals(&totals, byDay, timestamp, loc, feeSat)
		}
		accumulateOnchainBreakdown(&totals, byDay, timestamp, loc, feeSat, bucket)
	}

	return totals, byDay, nil
}

func addClosureTx(target map[string]onchainCloseCostBucket, txid string, bucket onchainCloseCostBucket) {
	clean := normalizeTxid(txid)
	if clean == "" {
		return
	}
	if _, exists := target[clean]; exists {
		return
	}
	target[clean] = bucket
}

func closeBucketFromLndCloseType(closeType int32) onchainCloseCostBucket {
	switch lnrpc.ChannelCloseSummary_ClosureType(closeType) {
	case lnrpc.ChannelCloseSummary_COOPERATIVE_CLOSE:
		return onchainCloseCostBucketCoop
	case lnrpc.ChannelCloseSummary_LOCAL_FORCE_CLOSE:
		return onchainCloseCostBucketLocalForce
	case lnrpc.ChannelCloseSummary_REMOTE_FORCE_CLOSE, lnrpc.ChannelCloseSummary_BREACH_CLOSE:
		return onchainCloseCostBucketRemoteForce
	default:
		return onchainCloseCostBucketNone
	}
}

func normalizeTxid(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func dayKey(ts time.Time, loc *time.Location) time.Time {
	local := ts.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

func accumulateOnchainTotals(totals *OnchainOverride, byDay map[time.Time]OnchainOverride, timestamp time.Time, loc *time.Location, feeSat int64) {
	if totals == nil || byDay == nil || feeSat <= 0 {
		return
	}
	totals.FeeMsat += feeSat * 1000
	totals.Count++

	key := dayKey(timestamp, loc)
	entry := byDay[key]
	entry.FeeMsat += feeSat * 1000
	entry.Count++
	byDay[key] = entry
}

func accumulateOnchainBreakdown(totals *OnchainOverride, byDay map[time.Time]OnchainOverride, timestamp time.Time, loc *time.Location, feeSat int64, bucket onchainCloseCostBucket) {
	if totals == nil || byDay == nil || feeSat <= 0 {
		return
	}

	feeMsat := feeSat * 1000
	key := dayKey(timestamp, loc)
	entry := byDay[key]

	switch bucket {
	case onchainCloseCostBucketCoop:
		totals.CoopCloseFeeMsat += feeMsat
		entry.CoopCloseFeeMsat += feeMsat
	case onchainCloseCostBucketLocalForce:
		totals.LocalForceFeeMsat += feeMsat
		entry.LocalForceFeeMsat += feeMsat
	case onchainCloseCostBucketRemoteForce:
		totals.RemoteForceFeeMsat += feeMsat
		entry.RemoteForceFeeMsat += feeMsat
	default:
		return
	}

	byDay[key] = entry
}

func newOnchainTxLookupClient() *onchainTxLookupClient {
	baseURL := strings.TrimSpace(os.Getenv(onchainTxLookupBaseURLEnv))
	if baseURL == "" {
		baseURL = defaultOnchainTxLookupBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	timeout := defaultOnchainTxLookupTimeout

	if raw := strings.TrimSpace(os.Getenv(onchainTxLookupTimeoutSecEnv)); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			timeout = time.Duration(seconds) * time.Second
		}
	}

	return &onchainTxLookupClient{
		baseURL: baseURL,
		client:  &http.Client{Timeout: timeout},
		cache:   map[string]onchainTxLookupCacheItem{},
	}
}

func (c *onchainTxLookupClient) Lookup(ctx context.Context, txid string) (int64, time.Time, bool) {
	if c == nil {
		return 0, time.Time{}, false
	}
	clean := normalizeTxid(txid)
	if clean == "" || c.baseURL == "" || c.client == nil {
		return 0, time.Time{}, false
	}

	if cached, ok := c.cache[clean]; ok && cached.Ready {
		return cached.FeeSat, cached.Timestamp, cached.OK
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/tx/"+clean, nil)
	if err != nil {
		c.cache[clean] = onchainTxLookupCacheItem{Ready: true}
		return 0, time.Time{}, false
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		c.cache[clean] = onchainTxLookupCacheItem{Ready: true}
		return 0, time.Time{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.cache[clean] = onchainTxLookupCacheItem{Ready: true}
		return 0, time.Time{}, false
	}

	var parsed onchainTxLookupResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		c.cache[clean] = onchainTxLookupCacheItem{Ready: true}
		return 0, time.Time{}, false
	}

	item := onchainTxLookupCacheItem{Ready: true}
	if parsed.Fee > 0 {
		item.FeeSat = parsed.Fee
		item.OK = true
	}
	if parsed.Status.BlockTime > 0 {
		item.Timestamp = time.Unix(parsed.Status.BlockTime, 0).UTC()
	}
	c.cache[clean] = item

	return item.FeeSat, item.Timestamp, item.OK
}
