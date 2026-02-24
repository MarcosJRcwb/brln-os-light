import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis
} from 'recharts'
import {
  getReportsConfig,
  getReportsCustom,
  getReportsLive,
  getReportsMovementLive,
  getReportsRange,
  getReportsSummary,
  getReportsSummaryCustom,
  updateReportsConfig
} from '../api'
import { getLocale } from '../i18n'

type ReportSeriesItem = {
  date: string
  forward_fee_revenue_sats: number
  rebalance_fee_cost_sats: number
  payment_fee_cost_sats?: number
  total_fee_cost_sats?: number
  net_routing_profit_sats: number
  forward_count: number
  rebalance_count: number
  payment_count?: number
  routed_volume_sats: number
  onchain_balance_sats?: number | null
  lightning_balance_sats?: number | null
  total_balance_sats?: number | null
}

type ReportMetrics = {
  forward_fee_revenue_sats: number
  rebalance_fee_cost_sats: number
  payment_fee_cost_sats?: number
  total_fee_cost_sats?: number
  net_routing_profit_sats: number
  forward_count: number
  rebalance_count: number
  payment_count?: number
  routed_volume_sats: number
  onchain_balance_sats?: number | null
  lightning_balance_sats?: number | null
  total_balance_sats?: number | null
}

type SeriesResponse = {
  range: string
  timezone: string
  series: ReportSeriesItem[]
}

type SummaryResponse = {
  range: string
  timezone: string
  days: number
  totals: ReportMetrics
  averages: ReportMetrics
  movement_target_sats?: number
  movement_pct?: number
}

type LiveResponse = ReportMetrics & {
  start: string
  end: string
  timezone: string
}

type MovementLiveResponse = {
  date: string
  start: string
  end: string
  timezone: string
  outbound_target_sats: number
  routed_volume_sats: number
  movement_pct: number
}

type ReportsConfig = {
  live_timeout_sec?: number | null
  live_lookback_hours?: number | null
  run_timeout_sec?: number | null
}

type RangeKey = 'd-1' | 'date' | 'month' | '3m' | '6m' | '12m' | 'all'

const rangeOptions: RangeKey[] = ['d-1', 'date', 'month', '3m', '6m', '12m', 'all']

const COLORS = {
  net: '#34d399',
  netNegative: '#f87171',
  revenue: '#38bdf8',
  costRebalance: '#f59e0b',
  costPayment: '#fbbf24',
  cost: '#f59e0b',
  onchain: '#22c55e',
  lightning: '#fb7185',
  total: '#eab308'
}

const tooltipContentStyle = {
  background: '#0f172a',
  borderRadius: 12,
  border: '1px solid rgba(255,255,255,0.1)',
  color: '#f8fafc'
}

const tooltipLabelStyle = {
  color: '#f8fafc',
  fontWeight: 600
}

const tooltipItemStyle = {
  color: '#f8fafc'
}

export default function Reports() {
  const { t, i18n } = useTranslation()
  const locale = getLocale(i18n.language)
  const [range, setRange] = useState<RangeKey>('d-1')
  const [customDate, setCustomDate] = useState(() => {
    const value = new Date()
    value.setDate(value.getDate() - 1)
    const year = value.getFullYear()
    const month = String(value.getMonth() + 1).padStart(2, '0')
    const day = String(value.getDate()).padStart(2, '0')
    return `${year}-${month}-${day}`
  })
  const [series, setSeries] = useState<ReportSeriesItem[]>([])
  const [summary, setSummary] = useState<SummaryResponse | null>(null)
  const [live, setLive] = useState<LiveResponse | null>(null)
  const [movementLive, setMovementLive] = useState<MovementLiveResponse | null>(null)
  const [movementLoading, setMovementLoading] = useState(true)
  const [movementError, setMovementError] = useState('')
  const [seriesLoading, setSeriesLoading] = useState(true)
  const [seriesError, setSeriesError] = useState('')
  const [liveLoading, setLiveLoading] = useState(true)
  const [liveError, setLiveError] = useState('')
  const [configLoading, setConfigLoading] = useState(true)
  const [configSaving, setConfigSaving] = useState(false)
  const [configStatus, setConfigStatus] = useState('')
  const [liveTimeout, setLiveTimeout] = useState('')
  const [liveLookback, setLiveLookback] = useState('')
  const [runTimeout, setRunTimeout] = useState('')

  const formatter = useMemo(() => new Intl.NumberFormat(locale, { maximumFractionDigits: 3 }), [locale])
  const compactFormatter = useMemo(() => new Intl.NumberFormat(locale, { notation: 'compact', maximumFractionDigits: 2 }), [locale])

  const formatSats = (value: number) => `${formatter.format(value)} sats`
  const formatCompact = (value: number) => compactFormatter.format(value)
  const formatInputDate = (value: Date) => {
    const year = value.getFullYear()
    const month = String(value.getMonth() + 1).padStart(2, '0')
    const day = String(value.getDate()).padStart(2, '0')
    return `${year}-${month}-${day}`
  }
  const isTodaySelection = (dateValue: string, serverToday?: string) => {
    if (serverToday && serverToday.trim()) {
      return dateValue === serverToday
    }
    return dateValue === formatInputDate(new Date())
  }
  const parseChartNumber = (value: number | string) => {
    if (typeof value === 'number') return value
    const raw = String(value).trim()
    if (!raw) return 0
    const inParens = raw.startsWith('(') && raw.endsWith(')')
    const cleaned = raw
      .replace(/^\(/, '')
      .replace(/\)$/, '')
      .replace(/\u2212/g, '-')
      .replace(/,/g, '')
      .replace(/\s/g, '')
      .replace(/[^\d.-]/g, '')
    const parsed = Number(cleaned)
    if (Number.isNaN(parsed)) return 0
    return inParens ? -parsed : parsed
  }

  const formatDateLabel = (value: string) => {
    const parsed = new Date(`${value}T00:00:00`)
    if (Number.isNaN(parsed.getTime())) {
      return value
    }
    return parsed.toLocaleDateString(locale, { month: 'short', day: 'numeric' })
  }

  const formatDateLong = (value: string) => {
    const parsed = new Date(value)
    if (Number.isNaN(parsed.getTime())) {
      return value
    }
    return parsed.toLocaleString(locale, {
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit'
    })
  }

  const formatTimeOnly = (value: string) => {
    const parsed = new Date(value)
    if (Number.isNaN(parsed.getTime())) {
      return value
    }
    return parsed.toLocaleTimeString(locale, {
      hour: '2-digit',
      minute: '2-digit'
    })
  }

  const formatLiveWindow = (start: string, end: string) => {
    const startDate = new Date(start)
    const endDate = new Date(end)
    if (Number.isNaN(startDate.getTime()) || Number.isNaN(endDate.getTime())) {
      return t('reports.liveRange')
    }
    const sameDay = startDate.toDateString() === endDate.toDateString()
    if (sameDay) {
      return `${formatTimeOnly(start)} - ${formatTimeOnly(end)}`
    }
    const startLabel = formatDateLong(start)
    const endLabel = formatDateLong(end)
    return `${startLabel} - ${endLabel}`
  }

  useEffect(() => {
    const selectedToday = range === 'date' && isTodaySelection(customDate, movementLive?.date)
    if (selectedToday) {
      setSeriesError('')
      if (!live) {
        setSeriesLoading(true)
        setSeries([])
        setSummary(null)
      }
      return
    }

    let active = true
    setSeriesLoading(true)
    setSeriesError('')

    const rangeRequest = range === 'date' ? getReportsCustom(customDate, customDate) : getReportsRange(range)
    const summaryRequest = range === 'date' ? getReportsSummaryCustom(customDate, customDate) : getReportsSummary(range)

    Promise.all([rangeRequest, summaryRequest])
      .then(([rangeResp, summaryResp]) => {
        if (!active) return
        const typedRange = rangeResp as SeriesResponse
        const typedSummary = summaryResp as SummaryResponse
        setSeries(Array.isArray(typedRange.series) ? typedRange.series : [])
        setSummary(typedSummary)
      })
      .catch((err) => {
        if (!active) return
        setSeriesError(err instanceof Error ? err.message : t('reports.unavailable'))
        setSeries([])
        setSummary(null)
      })
      .finally(() => {
        if (!active) return
        setSeriesLoading(false)
      })

    return () => {
      active = false
    }
  }, [customDate, movementLive?.date, range, t])

  useEffect(() => {
    const selectedToday = range === 'date' && isTodaySelection(customDate, movementLive?.date)
    if (!selectedToday || !live) return

    const liveMetrics: ReportMetrics = {
      forward_fee_revenue_sats: live.forward_fee_revenue_sats,
      rebalance_fee_cost_sats: live.rebalance_fee_cost_sats,
      payment_fee_cost_sats: live.payment_fee_cost_sats ?? 0,
      total_fee_cost_sats: live.total_fee_cost_sats ?? ((live.rebalance_fee_cost_sats ?? 0) + (live.payment_fee_cost_sats ?? 0)),
      net_routing_profit_sats: live.net_routing_profit_sats,
      forward_count: live.forward_count,
      rebalance_count: live.rebalance_count,
      payment_count: live.payment_count ?? 0,
      routed_volume_sats: live.routed_volume_sats,
      onchain_balance_sats: live.onchain_balance_sats ?? null,
      lightning_balance_sats: live.lightning_balance_sats ?? null,
      total_balance_sats: live.total_balance_sats ?? null
    }

    setSeries([{
      date: customDate,
      forward_fee_revenue_sats: live.forward_fee_revenue_sats,
      rebalance_fee_cost_sats: live.rebalance_fee_cost_sats,
      payment_fee_cost_sats: live.payment_fee_cost_sats ?? 0,
      total_fee_cost_sats: live.total_fee_cost_sats ?? ((live.rebalance_fee_cost_sats ?? 0) + (live.payment_fee_cost_sats ?? 0)),
      net_routing_profit_sats: live.net_routing_profit_sats,
      forward_count: live.forward_count,
      rebalance_count: live.rebalance_count,
      payment_count: live.payment_count ?? 0,
      routed_volume_sats: live.routed_volume_sats,
      onchain_balance_sats: live.onchain_balance_sats ?? null,
      lightning_balance_sats: live.lightning_balance_sats ?? null,
      total_balance_sats: live.total_balance_sats ?? null
    }])
    setSummary({
      range: 'custom',
      timezone: live.timezone,
      days: 1,
      totals: liveMetrics,
      averages: liveMetrics,
      movement_target_sats: movementLive?.outbound_target_sats ?? 0,
      movement_pct: movementLive?.movement_pct ?? 0
    })
    setSeriesError('')
    setSeriesLoading(false)
  }, [customDate, live, movementLive, range])

  useEffect(() => {
    let active = true
    const loadLive = () => {
      setLiveLoading(true)
      setLiveError('')
      getReportsLive()
        .then((data) => {
          if (!active) return
          setLive(data as LiveResponse)
        })
        .catch((err) => {
          if (!active) return
          setLiveError(err instanceof Error ? err.message : t('reports.liveUnavailable'))
        })
        .finally(() => {
          if (!active) return
          setLiveLoading(false)
        })
    }

    loadLive()
    const timer = window.setInterval(loadLive, 60000)
    return () => {
      active = false
      window.clearInterval(timer)
    }
  }, [])

  useEffect(() => {
    let active = true
    const loadMovement = () => {
      setMovementLoading(true)
      setMovementError('')
      getReportsMovementLive()
        .then((data) => {
          if (!active) return
          setMovementLive(data as MovementLiveResponse)
        })
        .catch((err) => {
          if (!active) return
          setMovementError(err instanceof Error ? err.message : t('reports.dailyMovementUnavailable'))
        })
        .finally(() => {
          if (!active) return
          setMovementLoading(false)
        })
    }
    loadMovement()
    const timer = window.setInterval(loadMovement, 60000)
    return () => {
      active = false
      window.clearInterval(timer)
    }
  }, [t])

  useEffect(() => {
    let active = true
    setConfigLoading(true)
    getReportsConfig()
      .then((data) => {
        if (!active) return
        const cfg = data as ReportsConfig
        setLiveTimeout(cfg.live_timeout_sec ? String(cfg.live_timeout_sec) : '')
        setLiveLookback(cfg.live_lookback_hours ? String(cfg.live_lookback_hours) : '')
        setRunTimeout(cfg.run_timeout_sec ? String(cfg.run_timeout_sec) : '')
      })
      .catch(() => {
        if (!active) return
      })
      .finally(() => {
        if (!active) return
        setConfigLoading(false)
      })

    return () => {
      active = false
    }
  }, [])

  const handleSaveConfig = async () => {
    setConfigSaving(true)
    setConfigStatus('')
    const payload: ReportsConfig = {
      live_timeout_sec: liveTimeout ? Number(liveTimeout) : null,
      live_lookback_hours: liveLookback ? Number(liveLookback) : null,
      run_timeout_sec: runTimeout ? Number(runTimeout) : null
    }
    try {
      await updateReportsConfig(payload)
      setConfigStatus(t('reports.settings.saved'))
    } catch (err) {
      setConfigStatus(err instanceof Error ? err.message : t('reports.settings.saveFailed'))
    } finally {
      setConfigSaving(false)
    }
  }

  const chartData = useMemo(() => {
    return series.map((item) => ({
      date: item.date,
      net: item.net_routing_profit_sats,
      revenue: item.forward_fee_revenue_sats,
      rebalanceCost: item.rebalance_fee_cost_sats ?? 0,
      paymentCost: item.payment_fee_cost_sats ?? 0,
      cost: item.total_fee_cost_sats ?? ((item.rebalance_fee_cost_sats ?? 0) + (item.payment_fee_cost_sats ?? 0)),
      onchain: item.onchain_balance_sats ?? null,
      lightning: item.lightning_balance_sats ?? null,
      total: item.total_balance_sats ?? null
    }))
  }, [series])

  const liveChartData = useMemo(() => {
    if (!live) return []
    const revenueValue = parseChartNumber(live.forward_fee_revenue_sats)
    const rebalanceCostValue = parseChartNumber(live.rebalance_fee_cost_sats ?? 0)
    const paymentCostValue = parseChartNumber(live.payment_fee_cost_sats ?? 0)
    const costValue = parseChartNumber(live.total_fee_cost_sats ?? (rebalanceCostValue + paymentCostValue))
    const netValue = parseChartNumber(live.net_routing_profit_sats)
    return [
      { name: t('reports.revenue'), revenue: revenueValue, rebalanceCost: 0, paymentCost: 0, net: 0 },
      { name: t('reports.cost'), revenue: 0, rebalanceCost: rebalanceCostValue, paymentCost: paymentCostValue, net: 0 },
      {
        name: t('reports.net'),
        revenue: 0,
        rebalanceCost: 0,
        paymentCost: 0,
        net: netValue,
        netColor: netValue < 0 ? COLORS.netNegative : COLORS.net
      }
    ]
  }, [live, t])

  const hasBalances = chartData.some((item) => item.onchain !== null || item.lightning !== null || item.total !== null)
  const movementPct = movementLive?.movement_pct ?? 0
  const hasMovementTarget = (movementLive?.outbound_target_sats ?? 0) > 0
  const movementProgress = Math.max(0, Math.min(100, movementPct))
  const movementTone = !hasMovementTarget ? 'bg-slate-500' : movementPct >= 75 ? 'bg-emerald-500' : movementPct >= 50 ? 'bg-amber-400' : 'bg-rose-500'
  const summaryMovementPct = summary?.movement_pct ?? 0
  const summaryMovementTarget = summary?.movement_target_sats ?? 0
  const summaryMovementTone = summaryMovementPct >= 75 ? 'bg-emerald-500' : summaryMovementPct >= 50 ? 'bg-amber-400' : 'bg-rose-500'
  const liveRebalanceCost = live?.rebalance_fee_cost_sats ?? 0
  const livePaymentCost = live?.payment_fee_cost_sats ?? 0
  const liveTotalCost = live?.total_fee_cost_sats ?? (liveRebalanceCost + livePaymentCost)

  return (
    <section className="space-y-6">
      <div className="section-card flex flex-wrap items-center justify-between gap-4">
        <div>
          <h2 className="text-2xl font-semibold">{t('reports.title')}</h2>
          <p className="text-fog/60">{t('reports.subtitle')}</p>
        </div>
        <span className="text-xs uppercase tracking-wide text-fog/60">{t('reports.updatedDaily')}</span>
      </div>

      <div className="grid gap-6 lg:grid-cols-5">
        <div className="section-card lg:col-span-3 flex flex-col gap-4">
          <div className="flex items-center justify-between">
            <h3 className="text-lg font-semibold">{t('reports.liveResults')}</h3>
            <span className="rounded-full bg-white/10 px-3 py-1 text-xs font-semibold uppercase tracking-wide text-fog">
              {live ? `${t('reports.liveWindow')}: ${formatLiveWindow(live.start, live.end)}` : t('reports.liveRange')}
            </span>
          </div>
          {liveLoading && !live && <p className="text-sm text-fog/60">{t('reports.loadingLive')}</p>}
          {liveError && <p className="text-sm text-brass">{liveError}</p>}
          {!liveLoading && !liveError && live && (
            <div className="flex flex-1 flex-col gap-4">
              <div className="grid gap-3 sm:grid-cols-3">
                <div className="rounded-2xl bg-white/5 p-4">
                  <p className="text-xs uppercase tracking-wide text-fog/60">{t('reports.revenue')}</p>
                  <p className="text-lg font-semibold text-fog">{formatSats(live.forward_fee_revenue_sats)}</p>
                </div>
                <div className="rounded-2xl bg-white/5 p-4">
                  <p className="text-xs uppercase tracking-wide text-fog/60">{t('reports.cost')}</p>
                  <div className="flex items-baseline justify-between gap-3">
                    <p className="text-lg font-semibold text-fog">{formatSats(liveTotalCost)}</p>
                    <p className="text-xs text-fog/60">
                      {t('reports.rebalances')} {formatSats(liveRebalanceCost)} | {t('reports.payments')} {formatSats(livePaymentCost)}
                    </p>
                  </div>
                </div>
                <div className="rounded-2xl bg-white/5 p-4">
                  <p className="text-xs uppercase tracking-wide text-fog/60">{t('reports.net')}</p>
                  <p className="text-lg font-semibold text-fog">{formatSats(live.net_routing_profit_sats)}</p>
                </div>
              </div>
              <div className="grid gap-3 sm:grid-cols-3 text-sm text-fog/70">
                <div className="flex items-center justify-between rounded-2xl bg-white/5 px-4 py-3">
                  <span>{t('reports.forwardCount')}</span>
                  <span className="text-fog">{formatter.format(live.forward_count)}</span>
                </div>
                <div className="flex items-center justify-between rounded-2xl bg-white/5 px-4 py-3">
                  <span>{t('reports.rebalanceCount')}</span>
                  <span className="text-fog">{formatter.format(live.rebalance_count)}</span>
                </div>
                <div className="flex items-center justify-between rounded-2xl bg-white/5 px-4 py-3">
                  <span>{t('reports.paymentCount')}</span>
                  <span className="text-fog">{formatter.format(live.payment_count ?? 0)}</span>
                </div>
              </div>
              <div className="min-h-[220px] flex-1">
                <ResponsiveContainer width="100%" height="100%">
                  <BarChart data={liveChartData} margin={{ top: 10, right: 10, left: 0, bottom: 10 }}>
                    <CartesianGrid stroke="rgba(255,255,255,0.08)" vertical={false} />
                    <XAxis dataKey="name" tick={{ fill: '#cbd5f5', fontSize: 12 }} axisLine={false} tickLine={false} />
                    <YAxis tick={{ fill: '#cbd5f5', fontSize: 11 }} axisLine={false} tickLine={false} tickFormatter={formatCompact} />
                    <Tooltip
                      cursor={{ fill: 'rgba(255,255,255,0.06)' }}
                      contentStyle={tooltipContentStyle}
                      labelStyle={tooltipLabelStyle}
                      itemStyle={tooltipItemStyle}
                      formatter={(value) => formatSats(Number(value))}
                    />
                    <Legend verticalAlign="top" height={24} formatter={(value) => <span className="text-xs text-fog/60">{value}</span>} />
                    <Bar dataKey="revenue" stackId="live" name={t('reports.revenue')} fill={COLORS.revenue} radius={[8, 8, 8, 8]} />
                    <Bar dataKey="rebalanceCost" stackId="live" name={t('reports.rebalances')} fill={COLORS.costRebalance} radius={[8, 8, 8, 8]} />
                    <Bar dataKey="paymentCost" stackId="live" name={t('reports.payments')} fill={COLORS.costPayment} radius={[8, 8, 8, 8]} />
                    <Bar dataKey="net" stackId="live" name={t('reports.net')} radius={[8, 8, 8, 8]}>
                      {liveChartData.map((entry) => (
                        <Cell key={entry.name} fill={entry.netColor ?? COLORS.net} />
                      ))}
                    </Bar>
                  </BarChart>
                </ResponsiveContainer>
              </div>
              <p className="text-xs text-fog/50">{t('reports.lastUpdated', { time: formatDateLong(live.end) })}</p>
            </div>
          )}
        </div>

        <div className="section-card lg:col-span-2 space-y-4">
          <h3 className="text-lg font-semibold">{t('reports.historicalRange')}</h3>
          <div className="flex flex-wrap gap-2">
            {rangeOptions.map((key) => (
              <button
                key={key}
                type="button"
                className={range === key ? 'btn-primary' : 'btn-secondary'}
                onClick={() => setRange(key)}
              >
                {t(`reports.range.${key}`)}
              </button>
            ))}
          </div>
          {range === 'date' && (
            <label className="block text-sm text-fog/70">
              {t('reports.selectDate')}
              <input
                className="input-field mt-2"
                type="date"
                value={customDate}
                onChange={(e) => setCustomDate(e.target.value)}
              />
            </label>
          )}
          {seriesLoading && series.length === 0 && <p className="text-sm text-fog/60">{t('reports.loadingRange')}</p>}
          {seriesError && <p className="text-sm text-brass">{seriesError}</p>}
          {!seriesLoading && !seriesError && summary && (
            <div className="space-y-3 text-sm text-fog/70">
              <div className="rounded-2xl bg-white/5 px-4 py-3">
                <p className="text-xs uppercase tracking-wide text-fog/50">{t('reports.totals')}</p>
                <p className="text-fog">{t('reports.revenue')} {formatSats(summary.totals.forward_fee_revenue_sats)}</p>
                <p className="text-fog">{t('reports.cost')} {formatSats(summary.totals.total_fee_cost_sats ?? ((summary.totals.rebalance_fee_cost_sats ?? 0) + (summary.totals.payment_fee_cost_sats ?? 0)))}</p>
                <p className="text-fog/80">{t('reports.rebalances')} {formatSats(summary.totals.rebalance_fee_cost_sats ?? 0)}</p>
                <p className="text-fog/80">{t('reports.payments')} {formatSats(summary.totals.payment_fee_cost_sats ?? 0)}</p>
                <p className="text-fog">{t('reports.net')} {formatSats(summary.totals.net_routing_profit_sats)}</p>
              </div>
              <div className="rounded-2xl bg-white/5 px-4 py-3">
                <p className="text-xs uppercase tracking-wide text-fog/50">{t('reports.averagesPerDay')}</p>
                <p className="text-fog">{t('reports.revenue')} {formatSats(summary.averages.forward_fee_revenue_sats)}</p>
                <p className="text-fog">{t('reports.cost')} {formatSats(summary.averages.total_fee_cost_sats ?? ((summary.averages.rebalance_fee_cost_sats ?? 0) + (summary.averages.payment_fee_cost_sats ?? 0)))}</p>
                <p className="text-fog/80">{t('reports.rebalances')} {formatSats(summary.averages.rebalance_fee_cost_sats ?? 0)}</p>
                <p className="text-fog/80">{t('reports.payments')} {formatSats(summary.averages.payment_fee_cost_sats ?? 0)}</p>
                <p className="text-fog">{t('reports.net')} {formatSats(summary.averages.net_routing_profit_sats)}</p>
              </div>
              <div className="rounded-2xl bg-white/5 px-4 py-3">
                <p className="text-xs uppercase tracking-wide text-fog/50">{t('reports.activity')}</p>
                <p className="text-fog">{t('reports.forwards')} {formatter.format(summary.totals.forward_count)}</p>
                <p className="text-fog">{t('reports.rebalances')} {formatter.format(summary.totals.rebalance_count)}</p>
                <p className="text-fog">{t('reports.payments')} {formatter.format(summary.totals.payment_count ?? 0)}</p>
                <p className="text-fog">{t('reports.routedVolume')} {formatSats(summary.totals.routed_volume_sats)}</p>
                {summaryMovementTarget > 0 ? (
                  <p className="mt-1 flex items-center gap-2 text-fog">
                    <span className={`inline-block h-2.5 w-2.5 rounded-full ${summaryMovementTone}`} />
                    <span>
                      {t('reports.movementProgress', {
                        routed: formatSats(summary.totals.routed_volume_sats),
                        target: formatSats(summaryMovementTarget),
                        percent: formatter.format(summaryMovementPct)
                      })}
                    </span>
                  </p>
                ) : (
                  <p className="mt-1 text-fog/70">{t('reports.movementProgressUnavailable')}</p>
                )}
              </div>
              <p className="text-xs text-fog/50">{t('reports.basedOnDays', { count: summary.days })}</p>
            </div>
          )}
        </div>
      </div>

      <div className="section-card space-y-4">
        <div className="flex items-center justify-between gap-3">
          <h3 className="text-lg font-semibold">{t('reports.dailyMovementGoal')}</h3>
          <span className="text-xs text-fog/60">{t('reports.liveRange')}</span>
        </div>
        {movementLoading && !movementLive && <p className="text-sm text-fog/60">{t('reports.loadingMovement')}</p>}
        {movementError && <p className="text-sm text-brass">{movementError}</p>}
        {!movementLoading && !movementError && movementLive && (
          <div className="space-y-3">
            <div className="flex flex-wrap items-center justify-between gap-2 text-sm text-fog/70">
              <span>{t('reports.routedVolume')} {formatSats(movementLive.routed_volume_sats)}</span>
              <span>{t('reports.outboundTarget')} {formatSats(movementLive.outbound_target_sats)}</span>
              <span>{t('reports.progressPct', { value: formatter.format(movementPct) })}</span>
            </div>
            <div className="h-3 w-full rounded-full bg-white/10 overflow-hidden">
              <div className={`h-full ${movementTone}`} style={{ width: `${movementProgress}%` }} />
            </div>
            {!hasMovementTarget && <p className="text-xs text-fog/50">{t('reports.movementProgressUnavailable')}</p>}
            <p className="text-xs text-fog/50">{t('reports.lastUpdated', { time: formatDateLong(movementLive.end) })}</p>
          </div>
        )}
      </div>

      <div className="section-card space-y-4">
        <div>
          <h3 className="text-lg font-semibold">{t('reports.settings.title')}</h3>
          <p className="text-fog/60">{t('reports.settings.subtitle')}</p>
        </div>
        {configLoading && <p className="text-sm text-fog/60">{t('reports.settings.loading')}</p>}
        {!configLoading && (
          <div className="grid gap-4 lg:grid-cols-3">
            <div className="space-y-2">
              <label className="text-sm text-fog/70">{t('reports.settings.liveTimeout')}</label>
              <input
                className="input-field"
                type="number"
                min={0}
                placeholder="60"
                value={liveTimeout}
                onChange={(e) => setLiveTimeout(e.target.value)}
              />
              <p className="text-xs text-fog/50">{t('reports.settings.liveTimeoutHint')}</p>
            </div>
            <div className="space-y-2">
              <label className="text-sm text-fog/70">{t('reports.settings.liveLookback')}</label>
              <input
                className="input-field"
                type="number"
                min={0}
                placeholder="24"
                value={liveLookback}
                onChange={(e) => setLiveLookback(e.target.value)}
              />
              <p className="text-xs text-fog/50">{t('reports.settings.liveLookbackHint')}</p>
            </div>
            <div className="space-y-2">
              <label className="text-sm text-fog/70">{t('reports.settings.runTimeout')}</label>
              <input
                className="input-field"
                type="number"
                min={0}
                placeholder="300"
                value={runTimeout}
                onChange={(e) => setRunTimeout(e.target.value)}
              />
              <p className="text-xs text-fog/50">{t('reports.settings.runTimeoutHint')}</p>
            </div>
          </div>
        )}
        <div className="flex flex-wrap items-center gap-3">
          <button className="btn-primary" onClick={handleSaveConfig} disabled={configSaving}>
            {configSaving ? t('reports.settings.saving') : t('reports.settings.save')}
          </button>
          {configStatus && <span className="text-sm text-fog/60">{configStatus}</span>}
        </div>
      </div>

      <div className="grid gap-6 lg:grid-cols-2">
        <div className="section-card space-y-4">
          <div className="flex items-center justify-between">
            <h3 className="text-lg font-semibold">{t('reports.netRoutingProfit')}</h3>
            <span className="text-xs text-fog/60">{t('reports.daily')}</span>
          </div>
          {chartData.length === 0 && !seriesLoading && !seriesError ? (
            <p className="text-sm text-fog/60">{t('reports.noData')}</p>
          ) : (
            <div className="h-64">
              <ResponsiveContainer width="100%" height="100%">
                <AreaChart data={chartData} margin={{ top: 10, right: 10, left: 0, bottom: 10 }}>
                  <defs>
                    <linearGradient id="netGradient" x1="0" y1="0" x2="0" y2="1">
                      <stop offset="5%" stopColor={COLORS.net} stopOpacity={0.5} />
                      <stop offset="95%" stopColor={COLORS.net} stopOpacity={0.05} />
                    </linearGradient>
                  </defs>
                  <CartesianGrid stroke="rgba(255,255,255,0.08)" vertical={false} />
                  <XAxis dataKey="date" tick={{ fill: '#cbd5f5', fontSize: 11 }} tickFormatter={formatDateLabel} axisLine={false} tickLine={false} />
                  <YAxis tick={{ fill: '#cbd5f5', fontSize: 11 }} tickFormatter={formatCompact} axisLine={false} tickLine={false} />
                  <Tooltip
                    cursor={{ stroke: 'rgba(255,255,255,0.1)', strokeWidth: 1 }}
                    contentStyle={tooltipContentStyle}
                    labelStyle={tooltipLabelStyle}
                    itemStyle={tooltipItemStyle}
                    formatter={(value) => formatSats(Number(value))}
                    labelFormatter={formatDateLabel}
                  />
                  <Area type="monotone" dataKey="net" stroke={COLORS.net} fill="url(#netGradient)" strokeWidth={2} />
                </AreaChart>
              </ResponsiveContainer>
            </div>
          )}
        </div>

        <div className="section-card space-y-4">
          <div className="flex items-center justify-between">
            <h3 className="text-lg font-semibold">{t('reports.revenueVsCost')}</h3>
            <span className="text-xs text-fog/60">{t('reports.daily')}</span>
          </div>
          {chartData.length === 0 && !seriesLoading && !seriesError ? (
            <p className="text-sm text-fog/60">{t('reports.noData')}</p>
          ) : (
            <div className="h-64">
              <ResponsiveContainer width="100%" height="100%">
                <LineChart data={chartData} margin={{ top: 10, right: 10, left: 0, bottom: 10 }}>
                  <CartesianGrid stroke="rgba(255,255,255,0.08)" vertical={false} />
                  <XAxis dataKey="date" tick={{ fill: '#cbd5f5', fontSize: 11 }} tickFormatter={formatDateLabel} axisLine={false} tickLine={false} />
                  <YAxis tick={{ fill: '#cbd5f5', fontSize: 11 }} tickFormatter={formatCompact} axisLine={false} tickLine={false} />
                  <Legend verticalAlign="top" height={24} formatter={(value) => <span className="text-xs text-fog/60">{value}</span>} />
                  <Tooltip
                    cursor={{ stroke: 'rgba(255,255,255,0.1)', strokeWidth: 1 }}
                    contentStyle={tooltipContentStyle}
                    labelStyle={tooltipLabelStyle}
                    itemStyle={tooltipItemStyle}
                    formatter={(value) => formatSats(Number(value))}
                    labelFormatter={formatDateLabel}
                  />
                  <Line type="monotone" dataKey="revenue" name={t('reports.revenue')} stroke={COLORS.revenue} strokeWidth={2} dot={false} />
                  <Line type="monotone" dataKey="cost" name={t('reports.cost')} stroke={COLORS.cost} strokeWidth={2} dot={false} />
                </LineChart>
              </ResponsiveContainer>
            </div>
          )}
        </div>
      </div>

      <div className="section-card space-y-4">
        <div className="flex items-center justify-between">
          <h3 className="text-lg font-semibold">{t('reports.balances')}</h3>
          <span className="text-xs text-fog/60">{t('reports.balancesSubtitle')}</span>
        </div>
        {!hasBalances && !seriesLoading && !seriesError ? (
          <p className="text-sm text-fog/60">{t('reports.balanceHistoryUnavailable')}</p>
        ) : (
          <div className="h-72">
            <ResponsiveContainer width="100%" height="100%">
              <LineChart data={chartData} margin={{ top: 10, right: 10, left: 0, bottom: 10 }}>
                <CartesianGrid stroke="rgba(255,255,255,0.08)" vertical={false} />
                <XAxis dataKey="date" tick={{ fill: '#cbd5f5', fontSize: 11 }} tickFormatter={formatDateLabel} axisLine={false} tickLine={false} />
                <YAxis tick={{ fill: '#cbd5f5', fontSize: 11 }} tickFormatter={formatCompact} axisLine={false} tickLine={false} />
                <Legend verticalAlign="top" height={24} formatter={(value) => <span className="text-xs text-fog/60">{value}</span>} />
                <Tooltip
                  cursor={{ stroke: 'rgba(255,255,255,0.1)', strokeWidth: 1 }}
                  contentStyle={tooltipContentStyle}
                  labelStyle={tooltipLabelStyle}
                  itemStyle={tooltipItemStyle}
                  formatter={(value) => formatSats(Number(value))}
                  labelFormatter={formatDateLabel}
                />
                <Line type="monotone" dataKey="onchain" name={t('reports.onchain')} stroke={COLORS.onchain} strokeWidth={2} dot={false} />
                <Line type="monotone" dataKey="lightning" name={t('reports.lightning')} stroke={COLORS.lightning} strokeWidth={2} dot={false} />
                <Line type="monotone" dataKey="total" name={t('reports.total')} stroke={COLORS.total} strokeWidth={2} dot={false} />
              </LineChart>
            </ResponsiveContainer>
          </div>
        )}
      </div>
    </section>
  )
}
