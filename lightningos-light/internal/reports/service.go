package reports

import (
	"context"
	"log"
	"sync"
	"time"

	"lightningos-light/internal/lndclient"

	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultLiveTTL = 60 * time.Second

type Service struct {
	db     *pgxpool.Pool
	lnd    *lndclient.Client
	logger *log.Logger

	liveTTL   time.Duration
	liveMu    sync.Mutex
	liveCache liveSnapshot
}

type liveSnapshot struct {
	ExpiresAt     time.Time
	Range         TimeRange
	Metrics       Metrics
	LookbackHours int
}

func NewService(db *pgxpool.Pool, lnd *lndclient.Client, logger *log.Logger) *Service {
	return &Service{
		db:      db,
		lnd:     lnd,
		logger:  logger,
		liveTTL: defaultLiveTTL,
	}
}

func (s *Service) EnsureSchema(ctx context.Context) error {
	return EnsureSchema(ctx, s.db)
}

func (s *Service) RunDaily(ctx context.Context, reportDate time.Time, loc *time.Location, rebalanceOverride *RebalanceOverride, paymentOverride *PaymentOverride, keysendOverride *KeysendReceivedOverride, onchainOverride *OnchainOverride) (Row, error) {
	tr := BuildTimeRangeForDate(reportDate, loc)
	metrics, err := ComputeMetrics(ctx, s.lnd, tr, false, rebalanceOverride, paymentOverride, keysendOverride, onchainOverride)
	if err != nil {
		return Row{}, err
	}
	if shouldAttachBalances(reportDate, loc) {
		metrics = s.attachBalances(ctx, metrics)
	}

	row := Row{ReportDate: dateOnly(reportDate, loc), Metrics: metrics}
	if err := UpsertDaily(ctx, s.db, row); err != nil {
		return Row{}, err
	}
	return row, nil
}

func (s *Service) Range(ctx context.Context, key string, now time.Time, loc *time.Location) ([]Row, DateRange, error) {
	dr, err := ResolveRangeWindow(now, loc, key)
	if err != nil {
		return nil, dr, err
	}
	if dr.All {
		items, err := FetchAll(ctx, s.db)
		return items, dr, err
	}
	items, err := FetchRange(ctx, s.db, dr.StartDate, dr.EndDate)
	return items, dr, err
}

func (s *Service) Summary(ctx context.Context, key string, now time.Time, loc *time.Location) (Summary, DateRange, error) {
	dr, err := ResolveRangeWindow(now, loc, key)
	if err != nil {
		return Summary{}, dr, err
	}
	if dr.All {
		summary, err := FetchSummaryAll(ctx, s.db)
		if err != nil {
			return Summary{}, dr, err
		}
		targetSat, err := FetchMovementTargetAllSum(ctx, s.db)
		if err != nil {
			return Summary{}, dr, err
		}
		summary.MovementTargetSat = targetSat
		summary.MovementPct = movementPct(summary.Totals, targetSat)
		return summary, dr, nil
	}
	summary, err := FetchSummaryRange(ctx, s.db, dr.StartDate, dr.EndDate)
	if err != nil {
		return Summary{}, dr, err
	}
	targetSat, err := FetchMovementTargetRangeSum(ctx, s.db, dr.StartDate, dr.EndDate)
	if err != nil {
		return Summary{}, dr, err
	}
	summary.MovementTargetSat = targetSat
	summary.MovementPct = movementPct(summary.Totals, targetSat)
	return summary, dr, nil
}

func (s *Service) CustomRange(ctx context.Context, startDate, endDate time.Time) ([]Row, error) {
	return FetchRange(ctx, s.db, startDate, endDate)
}

func (s *Service) CustomSummary(ctx context.Context, startDate, endDate time.Time) (Summary, error) {
	summary, err := FetchSummaryRange(ctx, s.db, startDate, endDate)
	if err != nil {
		return Summary{}, err
	}
	targetSat, err := FetchMovementTargetRangeSum(ctx, s.db, startDate, endDate)
	if err != nil {
		return Summary{}, err
	}
	summary.MovementTargetSat = targetSat
	summary.MovementPct = movementPct(summary.Totals, targetSat)
	return summary, nil
}

func (s *Service) Live(ctx context.Context, now time.Time, loc *time.Location, lookbackHours int) (TimeRange, Metrics, error) {
	if loc == nil {
		loc = time.Local
	}
	s.liveMu.Lock()
	cached := s.liveCache
	if time.Now().Before(cached.ExpiresAt) && cached.LookbackHours == lookbackHours {
		s.liveMu.Unlock()
		return cached.Range, cached.Metrics, nil
	}
	s.liveMu.Unlock()

	tr := BuildTimeRangeForLookback(now, loc, lookbackHours)
	metrics, err := ComputeMetrics(ctx, s.lnd, tr, false, nil, nil, nil, nil)
	if err != nil {
		return TimeRange{}, Metrics{}, err
	}
	metrics = s.attachBalances(ctx, metrics)

	s.liveMu.Lock()
	s.liveCache = liveSnapshot{
		ExpiresAt:     time.Now().Add(s.liveTTL),
		Range:         tr,
		Metrics:       metrics,
		LookbackHours: lookbackHours,
	}
	s.liveMu.Unlock()

	return tr, metrics, nil
}

func (s *Service) MovementLive(ctx context.Context, now time.Time, loc *time.Location) (MovementLive, error) {
	if loc == nil {
		loc = time.Local
	}
	reportDate := dateOnly(now, loc)
	targetSat, err := s.EnsureMovementTargetForDate(ctx, reportDate, loc)
	if err != nil {
		return MovementLive{}, err
	}
	tr, metrics, err := s.Live(ctx, now, loc, 0)
	if err != nil {
		return MovementLive{}, err
	}
	routed := routedVolumeSat(metrics)
	pct := 0.0
	if targetSat > 0 {
		pct = (routed / float64(targetSat)) * 100
	}
	return MovementLive{
		Date:            reportDate,
		Start:           tr.StartLocal,
		End:             tr.EndLocal,
		Timezone:        loc.String(),
		TargetSat:       targetSat,
		RoutedVolumeSat: routed,
		MovementPct:     pct,
	}, nil
}

func (s *Service) CaptureMovementTargetForDate(ctx context.Context, reportDate time.Time, loc *time.Location) (int64, error) {
	if s.lnd == nil {
		return 0, nil
	}
	if loc == nil {
		loc = time.Local
	}
	targetDate := dateOnly(reportDate, loc)
	targetSat, err := s.computeOutboundTarget(ctx)
	if err != nil {
		return 0, err
	}
	if err := UpsertMovementTargetDaily(ctx, s.db, targetDate, targetSat); err != nil {
		return 0, err
	}
	return targetSat, nil
}

func (s *Service) EnsureMovementTargetForDate(ctx context.Context, reportDate time.Time, loc *time.Location) (int64, error) {
	if loc == nil {
		loc = time.Local
	}
	targetDate := dateOnly(reportDate, loc)
	targetSat, ok, err := FetchMovementTargetDaily(ctx, s.db, targetDate)
	if err != nil {
		return 0, err
	}
	if ok {
		return targetSat, nil
	}
	return s.CaptureMovementTargetForDate(ctx, targetDate, loc)
}

func shouldAttachBalances(reportDate time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = time.Local
	}
	today := dateOnly(time.Now(), loc)
	target := dateOnly(reportDate, loc)
	return target.Equal(today.AddDate(0, 0, -1))
}

func (s *Service) attachBalances(ctx context.Context, metrics Metrics) Metrics {
	if s.lnd == nil {
		return metrics
	}
	balCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	summary, err := s.lnd.GetBalances(balCtx)
	if err != nil {
		if s.logger != nil {
			s.logger.Printf("reports: balances unavailable: %v", err)
		}
		return metrics
	}

	onchain := summary.OnchainSat
	lightning := summary.LightningSat
	total := summary.OnchainSat + summary.LightningSat

	metrics.OnchainBalanceSat = &onchain
	metrics.LightningBalanceSat = &lightning
	metrics.TotalBalanceSat = &total
	return metrics
}

func (s *Service) computeOutboundTarget(ctx context.Context) (int64, error) {
	if s.lnd == nil {
		return 0, nil
	}
	channels, err := s.lnd.ListChannels(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, ch := range channels {
		total += ch.LocalBalanceSat
	}
	return total, nil
}

func movementPct(metrics Metrics, targetSat int64) float64 {
	if targetSat <= 0 {
		return 0
	}
	return (routedVolumeSat(metrics) / float64(targetSat)) * 100
}

func routedVolumeSat(metrics Metrics) float64 {
	if metrics.RoutedVolumeMsat != 0 {
		return float64(metrics.RoutedVolumeMsat) / 1000
	}
	return float64(metrics.RoutedVolumeSat)
}
