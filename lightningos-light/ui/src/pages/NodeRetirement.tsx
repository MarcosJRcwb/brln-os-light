import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  confirmNodeRetirementCoopClose,
  createNodeRetirementSession,
  decideNodeRetirementChannel,
  getNodeRetirementSessionChannels,
  getNodeRetirementSessionEvents,
  getNodeRetirementSessionTransfer,
  getNodeRetirementSessions,
  getNodeRetirementStatus,
  getSuccessionConfig,
  getTelegramNotifications,
  successionAlive,
  successionSimulate,
  updateSuccessionConfig,
} from '../api'

type NodeRetirementStatus = {
  available: boolean
  active?: boolean
  active_session_id?: string
  active_state?: string
  error?: string
}

type NodeRetirementSession = {
  session_id: string
  source: string
  dry_run: boolean
  state: string
  disclaimer_accepted: boolean
  snapshot?: unknown
  config?: unknown
  reconciliation?: unknown
  started_at?: string
  completed_at?: string
  created_at: string
  last_error?: string
}

type NodeRetirementEvent = {
  id: number
  event_type: string
  severity: string
  created_at: string
}

type NodeRetirementChannel = {
  id: number
  session_id: string
  channel_point: string
  channel_id: number
  initial_state?: unknown
  current_state?: unknown
  decision?: string
  close_mode?: string
  close_txid?: string
  last_error?: string
  updated_at: string
}

type NodeRetirementTransfer = {
  id: number
  session_id: string
  destination_address: string
  sweep_all: boolean
  sat_per_vbyte: number
  min_confirmations: number
  status: string
  txid?: string
  amount_sat: number
  attempts: number
  last_error?: string
  last_attempt_at?: string
  confirmations?: number
  created_at: string
  updated_at: string
}

type SuccessionConfig = {
  enabled: boolean
  dry_run: boolean
  destination_address: string
  preapprove_fc_offline: boolean
  preapprove_fc_stuck_htlc: boolean
  stuck_htlc_threshold_sec: number
  sweep_min_confs: number
  sweep_sat_per_vbyte: number
  check_period_days: number
  reminder_period_days: number
  last_alive_at?: string
  next_check_at?: string
  deadline_at?: string
  status: string
}

type TelegramNotificationsConfig = {
  activity_mirror_enabled?: boolean
}

const stepOrder = [
  'snapshot',
  'quiesce',
  'drain_htlcs',
  'coop_close',
  'exceptions',
  'onchain_monitor',
  'reconcile',
] as const

type StepState = 'completed' | 'in_progress' | 'pending'

const stateIndexBySessionState: Record<string, number> = {
  created: 0,
  snapshot_taken: 1,
  quiescing: 1,
  draining_htlcs: 2,
  ready_for_coop_confirmation: 3,
  closing_coop: 3,
  awaiting_user_decision: 4,
  force_closing: 4,
  monitoring_onchain: 5,
  completed: 6,
  dry_run_completed: 6,
}

const badgeClass = (state: StepState) => {
  if (state === 'completed') return 'bg-emerald-500/20 text-emerald-200 border border-emerald-400/40'
  if (state === 'in_progress') return 'bg-amber-500/20 text-amber-100 border border-amber-300/40'
  return 'bg-rose-500/20 text-rose-100 border border-rose-300/40'
}

const formatTs = (value?: string) => {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString()
}

const txidExplorerLink = (txid?: string) => {
  const trimmed = String(txid || '').trim()
  if (!/^[0-9a-fA-F]{64}$/.test(trimmed)) return ''
  return `https://mempool.space/tx/${trimmed}`
}

const transferBadgeClass = (status?: string) => {
  const normalized = String(status || '').trim().toLowerCase()
  switch (normalized) {
    case 'confirmed':
    case 'completed':
      return 'bg-emerald-500/20 text-emerald-200 border border-emerald-400/40'
    case 'submitted':
      return 'bg-sky-500/20 text-sky-100 border border-sky-300/40'
    case 'failed':
      return 'bg-rose-500/20 text-rose-100 border border-rose-300/40'
    case 'waiting_funds':
    case 'waiting':
    default:
      return 'bg-amber-500/20 text-amber-100 border border-amber-300/40'
  }
}

const asRecord = (value: unknown): Record<string, unknown> => {
  if (!value) return {}
  if (typeof value === 'string') {
    const trimmed = value.trim()
    if (!trimmed) return {}
    try {
      const parsed = JSON.parse(trimmed)
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        return parsed as Record<string, unknown>
      }
    } catch {
      return {}
    }
    return {}
  }
  if (typeof value === 'object' && !Array.isArray(value)) {
    return value as Record<string, unknown>
  }
  return {}
}

const asNumber = (value: unknown): number | null => {
  if (typeof value === 'number' && Number.isFinite(value)) return value
  if (typeof value === 'string') {
    const parsed = Number(value.trim())
    if (Number.isFinite(parsed)) return parsed
  }
  return null
}

const asBool = (value: unknown): boolean | null => {
  if (typeof value === 'boolean') return value
  if (typeof value === 'number') {
    if (value === 1) return true
    if (value === 0) return false
  }
  if (typeof value === 'string') {
    const normalized = value.trim().toLowerCase()
    if (normalized === 'true' || normalized === '1') return true
    if (normalized === 'false' || normalized === '0') return false
  }
  return null
}

const pickNumber = (source: Record<string, unknown>, keys: string[]): number | null => {
  for (const key of keys) {
    const parsed = asNumber(source[key])
    if (parsed !== null) return parsed
  }
  return null
}

const pickString = (source: Record<string, unknown>, keys: string[]): string => {
  for (const key of keys) {
    const value = source[key]
    if (typeof value === 'string' && value.trim()) return value
  }
  return ''
}

const formatSats = (value: number | null) => (value === null ? '-' : `${value} sats`)

const formatDelta = (initial: number | null, current: number | null) => {
  if (initial === null || current === null) return '-'
  const delta = current - initial
  if (delta > 0) return `+${delta} sats`
  if (delta < 0) return `${delta} sats`
  return '0 sats'
}

export default function NodeRetirement() {
  const { t } = useTranslation()
  const [loading, setLoading] = useState(true)
  const [status, setStatus] = useState<NodeRetirementStatus | null>(null)
  const [sessions, setSessions] = useState<NodeRetirementSession[]>([])
  const [selectedSessionID, setSelectedSessionID] = useState('')
  const [events, setEvents] = useState<NodeRetirementEvent[]>([])
  const [channels, setChannels] = useState<NodeRetirementChannel[]>([])
  const [transfer, setTransfer] = useState<NodeRetirementTransfer | null>(null)
  const [confirmCoopOpen, setConfirmCoopOpen] = useState(false)
  const [disclaimerAccepted, setDisclaimerAccepted] = useState(false)
  const [dryRun, setDryRun] = useState(true)
  const [sessionStatus, setSessionStatus] = useState('')
  const [sessionBusy, setSessionBusy] = useState(false)
  const [sessionActionBusy, setSessionActionBusy] = useState(false)
  const [succession, setSuccession] = useState<SuccessionConfig | null>(null)
  const [telegramMirrorEnabled, setTelegramMirrorEnabled] = useState(false)
  const [successionStatus, setSuccessionStatus] = useState('')
  const [successionBusy, setSuccessionBusy] = useState(false)
  const [successionForm, setSuccessionForm] = useState<SuccessionConfig>({
    enabled: false,
    dry_run: true,
    destination_address: '',
    preapprove_fc_offline: false,
    preapprove_fc_stuck_htlc: false,
    stuck_htlc_threshold_sec: 86400,
    sweep_min_confs: 3,
    sweep_sat_per_vbyte: 0,
    check_period_days: 30,
    reminder_period_days: 30,
    status: 'disabled',
  })

  const loadAll = async (silent = false) => {
    if (!silent) setLoading(true)
    try {
      const [statusRes, sessionsRes, successionRes, telegramRes] = await Promise.all([
        getNodeRetirementStatus(),
        getNodeRetirementSessions(50),
        getSuccessionConfig(),
        getTelegramNotifications().catch(() => null),
      ])

      const statusData = statusRes as NodeRetirementStatus
      const sessionItems = Array.isArray((sessionsRes as any)?.items)
        ? ((sessionsRes as any).items as NodeRetirementSession[])
        : []
      const successionData = successionRes as SuccessionConfig
      const telegramData = telegramRes as TelegramNotificationsConfig | null

      setStatus(statusData)
      setSessions(sessionItems)
      setSuccession(successionData)
      setSuccessionForm(successionData)
      setTelegramMirrorEnabled(Boolean(telegramData?.activity_mirror_enabled))

      const activeID = statusData.active_session_id || sessionItems[0]?.session_id || ''
      if (activeID) {
        setSelectedSessionID((prev) => prev || activeID)
      } else {
        setSelectedSessionID('')
        setEvents([])
        setChannels([])
        setTransfer(null)
      }
    } catch (err) {
      setSessionStatus(err instanceof Error ? err.message : t('nodeRetirement.loadFailed'))
    } finally {
      if (!silent) setLoading(false)
    }
  }

  useEffect(() => {
    void loadAll()
    const timer = window.setInterval(() => {
      void loadAll(true)
    }, 10000)
    return () => window.clearInterval(timer)
  }, [])

  useEffect(() => {
    let mounted = true
    const loadSessionData = async () => {
      if (!selectedSessionID) {
        if (mounted) {
          setEvents([])
          setChannels([])
          setTransfer(null)
        }
        return
      }
      try {
        const [eventData, channelData, transferData] = await Promise.all([
          getNodeRetirementSessionEvents(selectedSessionID, 120),
          getNodeRetirementSessionChannels(selectedSessionID),
          getNodeRetirementSessionTransfer(selectedSessionID).catch(() => ({ item: null })),
        ])
        if (!mounted) return
        setEvents(Array.isArray((eventData as any)?.items) ? (eventData as any).items : [])
        setChannels(Array.isArray((channelData as any)?.items) ? (channelData as any).items : [])
        setTransfer(((transferData as any)?.item || null) as NodeRetirementTransfer | null)
      } catch {
        if (!mounted) return
      }
    }
    void loadSessionData()
    return () => {
      mounted = false
    }
  }, [selectedSessionID])

  const activeSession = useMemo(
    () => sessions.find((item) => item.session_id === selectedSessionID) || sessions[0] || null,
    [sessions, selectedSessionID]
  )

  const snapshotView = useMemo(() => {
    const snapshot = asRecord(activeSession?.snapshot)
    if (Object.keys(snapshot).length === 0) return null
    const balances = asRecord(snapshot.balances)
    const summary = asRecord(snapshot.summary)
    const pending = Array.isArray(snapshot.pending) ? snapshot.pending.length : null
    const channels = Array.isArray(snapshot.channels) ? snapshot.channels.length : null
    return {
      takenAt: pickString(snapshot, ['taken_at']),
      openChannels: pickNumber(summary, ['open_channels']) ?? channels,
      pendingChannels: pickNumber(summary, ['pending_channels']) ?? pending,
      pendingHtlcs: pickNumber(summary, ['pending_htlc_channels']),
      onchainConfirmed: pickNumber(balances, ['OnchainConfirmedSat', 'onchain_confirmed_sat']),
      onchainPending: pickNumber(balances, ['OnchainUnconfirmedSat', 'onchain_unconfirmed_sat']),
      lightningSettled: pickNumber(balances, ['LightningLocalSat', 'lightning_local_sat']),
      lightningPending: pickNumber(balances, ['LightningUnsettledLocalSat', 'lightning_unsettled_local_sat']),
    }
  }, [activeSession])

  const reconciliationView = useMemo(() => {
    const reconciliation = asRecord(activeSession?.reconciliation)
    if (Object.keys(reconciliation).length === 0) return null
    const balances = asRecord(reconciliation.balances)
    const transferData = asRecord(reconciliation.transfer)
    return {
      finishedAt: pickString(reconciliation, ['finished_at']),
      result: pickString(reconciliation, ['result']),
      openChannels: pickNumber(reconciliation, ['open_channels']),
      pendingChannels: pickNumber(reconciliation, ['pending_channels']),
      onchainConfirmed: pickNumber(balances, ['OnchainConfirmedSat', 'onchain_confirmed_sat']),
      onchainPending: pickNumber(balances, ['OnchainUnconfirmedSat', 'onchain_unconfirmed_sat']),
      lightningSettled: pickNumber(balances, ['LightningLocalSat', 'lightning_local_sat']),
      lightningPending: pickNumber(balances, ['LightningUnsettledLocalSat', 'lightning_unsettled_local_sat']),
      transferStatus: pickString(transferData, ['status']),
      transferAmountSat: pickNumber(transferData, ['amount_sat', 'AmountSat']),
    }
  }, [activeSession])

  const channelTimeline = useMemo(() => {
    return channels.map((item) => {
      const initial = asRecord(item.initial_state)
      const current = asRecord(item.current_state)
      return {
        id: item.id,
        channelPoint: item.channel_point,
        channelID: item.channel_id,
        peerAlias: pickString(current, ['peer_alias', 'peerAlias']) || pickString(initial, ['peer_alias', 'peerAlias']),
        capturedAt: pickString(initial, ['captured_at']),
        updatedAt: pickString(current, ['updated_at']) || item.updated_at,
        initialActive: asBool(initial.active),
        currentActive: asBool(current.active),
        initialLocal: pickNumber(initial, ['local_balance_sat', 'localBalanceSat']),
        currentLocal: pickNumber(current, ['local_balance_sat', 'localBalanceSat']),
        initialRemote: pickNumber(initial, ['remote_balance_sat', 'remoteBalanceSat']),
        currentRemote: pickNumber(current, ['remote_balance_sat', 'remoteBalanceSat']),
        initialPendingHTLCs: pickNumber(initial, ['pending_htlc_count', 'pendingHtlcCount']),
        currentPendingHTLCs: pickNumber(current, ['pending_htlc_count', 'pendingHtlcCount']),
        decision: item.decision || '',
        closeMode: item.close_mode || '',
        closeTxid: item.close_txid || '',
        lastError: item.last_error || '',
      }
    })
  }, [channels])

  const stepStates = useMemo(() => {
    const idx = stateIndexBySessionState[activeSession?.state || ''] ?? -1
    return stepOrder.map((_, position) => {
      if (idx < 0) return 'pending' as StepState
      if (position < idx) return 'completed' as StepState
      if (position === idx) return activeSession?.state === 'completed' || activeSession?.state === 'dry_run_completed'
        ? 'completed'
        : 'in_progress'
      return 'pending' as StepState
    })
  }, [activeSession])

  const handleCreateSession = async () => {
    if (!disclaimerAccepted) {
      setSessionStatus(t('nodeRetirement.disclaimerRequired'))
      return
    }
    setSessionBusy(true)
    setSessionStatus('')
    try {
      await createNodeRetirementSession({
        source: 'manual',
        dry_run: dryRun,
        disclaimer_accepted: true,
      })
      setSessionStatus(t('nodeRetirement.sessionCreated'))
      setDisclaimerAccepted(false)
      await loadAll(true)
    } catch (err) {
      setSessionStatus(err instanceof Error ? err.message : t('nodeRetirement.createFailed'))
    } finally {
      setSessionBusy(false)
    }
  }

  const handleConfirmCoopClose = async () => {
    if (!activeSession) return
    setSessionActionBusy(true)
    setSessionStatus('')
    try {
      await confirmNodeRetirementCoopClose(activeSession.session_id)
      setSessionStatus(t('nodeRetirement.coopConfirmed'))
      setConfirmCoopOpen(false)
      await loadAll(true)
    } catch (err) {
      setSessionStatus(err instanceof Error ? err.message : t('nodeRetirement.coopConfirmFailed'))
    } finally {
      setSessionActionBusy(false)
    }
  }

  const handleChannelDecision = async (channelPoint: string, decision: 'wait' | 'force_close') => {
    if (!activeSession) return
    setSessionActionBusy(true)
    setSessionStatus('')
    try {
      await decideNodeRetirementChannel(activeSession.session_id, { channel_point: channelPoint, decision })
      setSessionStatus(decision === 'force_close' ? t('nodeRetirement.forceCloseDecisionSaved') : t('nodeRetirement.waitDecisionSaved'))
      await loadAll(true)
    } catch (err) {
      setSessionStatus(err instanceof Error ? err.message : t('nodeRetirement.decisionFailed'))
    } finally {
      setSessionActionBusy(false)
    }
  }

  const handleSaveSuccession = async () => {
    setSuccessionStatus('')
    if (successionForm.enabled && !telegramMirrorEnabled) {
      setSuccessionStatus(t('nodeRetirement.telegramMirrorRequiredError'))
      return
    }
    setSuccessionBusy(true)
    try {
      const next = await updateSuccessionConfig({
        enabled: successionForm.enabled,
        dry_run: successionForm.dry_run,
        destination_address: successionForm.destination_address,
        preapprove_fc_offline: successionForm.preapprove_fc_offline,
        preapprove_fc_stuck_htlc: successionForm.preapprove_fc_stuck_htlc,
        stuck_htlc_threshold_sec: successionForm.stuck_htlc_threshold_sec,
        sweep_min_confs: successionForm.sweep_min_confs,
        sweep_sat_per_vbyte: successionForm.sweep_sat_per_vbyte,
        check_period_days: successionForm.check_period_days,
        reminder_period_days: successionForm.reminder_period_days,
      })
      setSuccession(next as SuccessionConfig)
      setSuccessionForm(next as SuccessionConfig)
      setSuccessionStatus(t('nodeRetirement.successionSaved'))
    } catch (err) {
      setSuccessionStatus(err instanceof Error ? err.message : t('nodeRetirement.successionSaveFailed'))
    } finally {
      setSuccessionBusy(false)
    }
  }

  const handleAlive = async () => {
    setSuccessionBusy(true)
    setSuccessionStatus('')
    try {
      const next = await successionAlive({ source: 'ui' })
      setSuccession(next as SuccessionConfig)
      setSuccessionForm(next as SuccessionConfig)
      setSuccessionStatus(t('nodeRetirement.aliveConfirmed'))
    } catch (err) {
      setSuccessionStatus(err instanceof Error ? err.message : t('nodeRetirement.aliveFailed'))
    } finally {
      setSuccessionBusy(false)
    }
  }

  const handleSimulate = async (action: 'alive' | 'not_alive') => {
    setSuccessionBusy(true)
    setSuccessionStatus('')
    try {
      await successionSimulate({ action, source: 'ui' })
      setSuccessionStatus(action === 'alive' ? t('nodeRetirement.simulateAliveOk') : t('nodeRetirement.simulateNotAliveOk'))
    } catch (err) {
      setSuccessionStatus(err instanceof Error ? err.message : t('nodeRetirement.simulateFailed'))
    } finally {
      setSuccessionBusy(false)
    }
  }

  if (loading) {
    return <section className="section-card">{t('nodeRetirement.loading')}</section>
  }

  const transferStateLabel = (value?: string) => {
    const normalized = String(value || '').trim().toLowerCase()
    if (!normalized) return '-'
    const key = `nodeRetirement.transferStates.${normalized}`
    const translated = t(key)
    return translated === key ? normalized : translated
  }

  const channelActivityLabel = (value: boolean | null) => {
    if (value === true) return t('nodeRetirement.channelActive')
    if (value === false) return t('nodeRetirement.channelInactive')
    return t('nodeRetirement.channelUnknown')
  }

  return (
    <section className="space-y-6">
      <div className="section-card space-y-2 border border-amber-400/30">
        <h2 className="text-2xl font-semibold">{t('nodeRetirement.title')}</h2>
        <p className="text-sm text-fog/70">{t('nodeRetirement.subtitle')}</p>
        <p className="text-sm text-amber-100">{t('nodeRetirement.disclaimer')}</p>
      </div>

      <div className="section-card space-y-3">
        <div className="flex flex-wrap items-center gap-4">
          <label className="text-sm text-fog/70">
            <input
              className="mr-2"
              type="checkbox"
              checked={dryRun}
              onChange={(e) => setDryRun(e.target.checked)}
            />
            {t('nodeRetirement.dryRun')}
          </label>
          <label className="text-sm text-fog/70">
            <input
              className="mr-2"
              type="checkbox"
              checked={disclaimerAccepted}
              onChange={(e) => setDisclaimerAccepted(e.target.checked)}
            />
            {t('nodeRetirement.disclaimerAck')}
          </label>
          <button
            className="btn-primary disabled:opacity-60 disabled:cursor-not-allowed"
            type="button"
            onClick={handleCreateSession}
            disabled={sessionBusy || !status?.available}
          >
            {sessionBusy ? t('nodeRetirement.creating') : t('nodeRetirement.createSession')}
          </button>
        </div>
        {sessionStatus && <p className="text-sm text-brass">{sessionStatus}</p>}
        <div className="text-xs text-fog/60">
          {t('nodeRetirement.moduleStatus')}: {status?.available ? t('common.enabled') : t('common.disabled')}
          {status?.error ? ` - ${status.error}` : ''}
        </div>
      </div>

      <div className="section-card space-y-4">
        <h3 className="text-lg font-semibold">{t('nodeRetirement.stepsTitle')}</h3>
        <div className="grid gap-2 lg:grid-cols-2">
          {stepOrder.map((step, idx) => (
            <div key={step} className="rounded-2xl border border-white/10 bg-ink/60 px-3 py-2 flex items-center justify-between">
              <span className="text-sm text-fog/80">{t(`nodeRetirement.steps.${step}`)}</span>
              <span className={`rounded-full px-2 py-0.5 text-[11px] uppercase ${badgeClass(stepStates[idx])}`}>
                {t(`nodeRetirement.badges.${stepStates[idx]}`)}
              </span>
            </div>
          ))}
        </div>
      </div>

      <div className="section-card space-y-4">
        <h3 className="text-lg font-semibold">{t('nodeRetirement.sessionsTitle')}</h3>
        {sessions.length === 0 ? (
          <p className="text-sm text-fog/60">{t('nodeRetirement.sessionsEmpty')}</p>
        ) : (
          <div className="grid gap-3">
            {sessions.map((item) => (
              <button
                key={item.session_id}
                className={`text-left rounded-2xl border px-3 py-2 transition ${
                  selectedSessionID === item.session_id ? 'border-glow/60 bg-white/10' : 'border-white/10 bg-ink/60'
                }`}
                onClick={() => setSelectedSessionID(item.session_id)}
                type="button"
              >
                <div className="flex items-center justify-between">
                  <span className="text-sm text-fog">{item.session_id}</span>
                  <span className="text-xs uppercase text-fog/60">{item.state}</span>
                </div>
                <p className="text-xs text-fog/50 mt-1">
                  {item.source} - {item.dry_run ? 'dry-run' : 'live'} - {formatTs(item.created_at)}
                </p>
                {item.last_error && <p className="text-xs text-amber-200 mt-1">{item.last_error}</p>}
              </button>
            ))}
          </div>
        )}
        <div className="grid gap-3 lg:grid-cols-2">
          <div className="rounded-2xl border border-white/10 bg-ink/60 p-3">
            <p className="text-sm text-fog mb-2">{t('nodeRetirement.snapshotTitle')}</p>
            {!snapshotView ? (
              <p className="text-xs text-fog/60">{t('nodeRetirement.snapshotEmpty')}</p>
            ) : (
              <div className="grid gap-1 text-xs text-fog/70">
                <p>{t('nodeRetirement.snapshotTakenAt')}: <span className="text-fog">{formatTs(snapshotView.takenAt || activeSession?.started_at)}</span></p>
                <p>{t('nodeRetirement.openChannels')}: <span className="text-fog">{snapshotView.openChannels ?? '-'}</span></p>
                <p>{t('nodeRetirement.pendingChannels')}: <span className="text-fog">{snapshotView.pendingChannels ?? '-'}</span></p>
                <p>{t('nodeRetirement.pendingHtlcs')}: <span className="text-fog">{snapshotView.pendingHtlcs ?? '-'}</span></p>
                <p>{t('nodeRetirement.onchainConfirmed')}: <span className="text-fog">{formatSats(snapshotView.onchainConfirmed)}</span></p>
                <p>{t('nodeRetirement.onchainPending')}: <span className="text-fog">{formatSats(snapshotView.onchainPending)}</span></p>
                <p>{t('nodeRetirement.lightningSettled')}: <span className="text-fog">{formatSats(snapshotView.lightningSettled)}</span></p>
                <p>{t('nodeRetirement.lightningPending')}: <span className="text-fog">{formatSats(snapshotView.lightningPending)}</span></p>
              </div>
            )}
          </div>
          <div className="rounded-2xl border border-white/10 bg-ink/60 p-3">
            <p className="text-sm text-fog mb-2">{t('nodeRetirement.reconciliationTitle')}</p>
            {!reconciliationView ? (
              <p className="text-xs text-fog/60">{t('nodeRetirement.reconciliationEmpty')}</p>
            ) : (
              <div className="grid gap-1 text-xs text-fog/70">
                <p>{t('nodeRetirement.sessionCompletedAt')}: <span className="text-fog">{formatTs(reconciliationView.finishedAt || activeSession?.completed_at)}</span></p>
                {reconciliationView.result && <p>{t('nodeRetirement.reconciliationResult')}: <span className="text-fog">{reconciliationView.result}</span></p>}
                <p>{t('nodeRetirement.openChannels')}: <span className="text-fog">{reconciliationView.openChannels ?? '-'}</span></p>
                <p>{t('nodeRetirement.pendingChannels')}: <span className="text-fog">{reconciliationView.pendingChannels ?? '-'}</span></p>
                <p>{t('nodeRetirement.onchainConfirmed')}: <span className="text-fog">{formatSats(reconciliationView.onchainConfirmed)}</span></p>
                <p>{t('nodeRetirement.onchainPending')}: <span className="text-fog">{formatSats(reconciliationView.onchainPending)}</span></p>
                <p>{t('nodeRetirement.lightningSettled')}: <span className="text-fog">{formatSats(reconciliationView.lightningSettled)}</span></p>
                <p>{t('nodeRetirement.lightningPending')}: <span className="text-fog">{formatSats(reconciliationView.lightningPending)}</span></p>
                {reconciliationView.transferStatus && (
                  <p>
                    {t('nodeRetirement.transferStatus')}:{' '}
                    <span className={`rounded-full px-2 py-0.5 text-[11px] uppercase ${transferBadgeClass(reconciliationView.transferStatus)}`}>
                      {transferStateLabel(reconciliationView.transferStatus)}
                    </span>
                    {reconciliationView.transferAmountSat !== null ? ` - ${reconciliationView.transferAmountSat} sats` : ''}
                  </p>
                )}
              </div>
            )}
          </div>
        </div>
        <div className="rounded-2xl border border-white/10 bg-ink/60 p-3 space-y-2">
          <p className="text-sm text-fog">{t('nodeRetirement.channelTimelineTitle')}</p>
          <p className="text-xs text-fog/60">{t('nodeRetirement.channelTimelineSubtitle')}</p>
          {channelTimeline.length === 0 ? (
            <p className="text-xs text-fog/60">{t('nodeRetirement.channelTimelineEmpty')}</p>
          ) : (
            <div className="space-y-2 max-h-80 overflow-y-auto pr-1">
              {channelTimeline.map((item) => (
                <div key={item.id} className="rounded-xl border border-white/10 bg-black/20 p-2 space-y-2">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <p className="text-xs text-fog break-all">
                      {item.channelPoint}
                      {item.peerAlias ? ` - ${item.peerAlias}` : ''}
                    </p>
                    <div className="flex items-center gap-2 text-[11px]">
                      <span className={`rounded-full px-2 py-0.5 uppercase ${item.initialActive ? 'bg-sky-500/20 text-sky-100 border border-sky-300/40' : 'bg-white/10 text-fog/70 border border-white/20'}`}>
                        {t('nodeRetirement.channelInitial')}: {channelActivityLabel(item.initialActive)}
                      </span>
                      <span className={`rounded-full px-2 py-0.5 uppercase ${item.currentActive ? 'bg-emerald-500/20 text-emerald-100 border border-emerald-300/40' : 'bg-amber-500/20 text-amber-100 border border-amber-300/40'}`}>
                        {t('nodeRetirement.channelCurrent')}: {channelActivityLabel(item.currentActive)}
                      </span>
                    </div>
                  </div>
                  <div className="grid gap-1 text-xs text-fog/70 lg:grid-cols-3">
                    <p>
                      {t('nodeRetirement.channelLocalBalance')}:{' '}
                      <span className="text-fog">{formatSats(item.initialLocal)} {'->'} {formatSats(item.currentLocal)}</span>{' '}
                      <span className="text-fog/60">({t('nodeRetirement.channelDelta')}: {formatDelta(item.initialLocal, item.currentLocal)})</span>
                    </p>
                    <p>
                      {t('nodeRetirement.channelRemoteBalance')}:{' '}
                      <span className="text-fog">{formatSats(item.initialRemote)} {'->'} {formatSats(item.currentRemote)}</span>{' '}
                      <span className="text-fog/60">({t('nodeRetirement.channelDelta')}: {formatDelta(item.initialRemote, item.currentRemote)})</span>
                    </p>
                    <p>
                      {t('nodeRetirement.channelPendingHtlcs')}:{' '}
                      <span className="text-fog">{item.initialPendingHTLCs ?? '-'} {'->'} {item.currentPendingHTLCs ?? '-'}</span>
                    </p>
                    <p>{t('nodeRetirement.channelCloseMode')}: <span className="text-fog">{item.closeMode || '-'}</span></p>
                    <p className="break-all">{t('nodeRetirement.channelCloseTxid')}: <span className="text-fog">{item.closeTxid || '-'}</span></p>
                    <p>{t('nodeRetirement.channelDecisionLabel')}: <span className="text-fog">{item.decision || '-'}</span></p>
                    <p>{t('nodeRetirement.snapshotTakenAt')}: <span className="text-fog">{formatTs(item.capturedAt)}</span></p>
                    <p>{t('nodeRetirement.transferUpdated')}: <span className="text-fog">{formatTs(item.updatedAt)}</span></p>
                    {item.lastError && <p className="text-amber-200 lg:col-span-3">{item.lastError}</p>}
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
        <div className="rounded-2xl border border-white/10 bg-ink/60 p-3">
          <p className="text-sm text-fog mb-2">{t('nodeRetirement.eventsTitle')}</p>
          {events.length === 0 ? (
            <p className="text-xs text-fog/60">{t('nodeRetirement.eventsEmpty')}</p>
          ) : (
            <div className="max-h-56 overflow-y-auto space-y-2">
              {events.map((evt) => (
                <p key={evt.id} className="text-xs text-fog/70">
                  [{formatTs(evt.created_at)}] {evt.event_type} ({evt.severity})
                </p>
              ))}
            </div>
          )}
        </div>
        {activeSession?.state === 'ready_for_coop_confirmation' && (
          <div className="rounded-2xl border border-amber-400/30 bg-amber-500/10 p-3 space-y-2">
            <p className="text-sm text-amber-100">{t('nodeRetirement.coopConfirmWarning')}</p>
            <button
              className="btn-primary text-xs px-3 py-2"
              type="button"
              onClick={() => setConfirmCoopOpen(true)}
              disabled={sessionActionBusy}
            >
              {t('nodeRetirement.confirmCoopClose')}
            </button>
          </div>
        )}
        {(activeSession?.state === 'awaiting_user_decision' || activeSession?.state === 'force_closing') && (
          <div className="rounded-2xl border border-white/10 bg-ink/60 p-3 space-y-3">
            <p className="text-sm text-fog">{t('nodeRetirement.channelDecisions')}</p>
            {channels.length === 0 ? (
              <p className="text-xs text-fog/60">{t('nodeRetirement.channelsEmpty')}</p>
            ) : (
              <div className="space-y-2">
                {channels.map((item) => (
                  <div key={item.id} className="rounded-xl border border-white/10 bg-black/20 p-2 space-y-1">
                    <p className="text-xs text-fog">
                      {item.channel_point}
                      {item.close_mode ? ` - ${item.close_mode}` : ''}
                      {item.close_txid ? ` - ${item.close_txid}` : ''}
                    </p>
                    {item.last_error && <p className="text-xs text-amber-200">{item.last_error}</p>}
                    <p className="text-xs text-fog/60">
                      {t('nodeRetirement.currentDecision')}: {item.decision || '-'}
                    </p>
                    <div className="flex flex-wrap gap-2">
                      <button
                        className="btn-secondary text-xs px-2 py-1"
                        type="button"
                        disabled={sessionActionBusy}
                        onClick={() => handleChannelDecision(item.channel_point, 'wait')}
                      >
                        {t('nodeRetirement.waitAction')}
                      </button>
                      <button
                        className="btn-secondary text-xs px-2 py-1"
                        type="button"
                        disabled={sessionActionBusy}
                        onClick={() => handleChannelDecision(item.channel_point, 'force_close')}
                      >
                        {t('nodeRetirement.forceCloseAction')}
                      </button>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}
        {(transfer || activeSession?.source === 'succession') && (
          <div className="rounded-2xl border border-white/10 bg-ink/60 p-3 space-y-2">
            <p className="text-sm text-fog">{t('nodeRetirement.transferAuditTitle')}</p>
            {!transfer ? (
              <p className="text-xs text-fog/60">{t('nodeRetirement.transferAuditEmpty')}</p>
            ) : (
              <div className="grid gap-2 text-xs text-fog/70 lg:grid-cols-2">
                <p>
                  {t('nodeRetirement.transferStatus')}:{' '}
                  <span className={`rounded-full px-2 py-0.5 text-[11px] uppercase ${transferBadgeClass(transfer.status)}`}>
                    {transferStateLabel(transfer.status || 'waiting')}
                  </span>
                </p>
                <p>{t('nodeRetirement.transferAttempts')}: <span className="text-fog">{transfer.attempts ?? 0}</span></p>
                <p>{t('nodeRetirement.transferDestination')}: <span className="text-fog">{transfer.destination_address || '-'}</span></p>
                <p>{t('nodeRetirement.transferAmount')}: <span className="text-fog">{transfer.amount_sat ?? 0} sats</span></p>
                <p className="break-all">
                  {t('nodeRetirement.transferTxid')}:{' '}
                  {transfer.txid ? (
                    <>
                      <span className="text-fog">{transfer.txid}</span>
                      {txidExplorerLink(transfer.txid) && (
                        <>
                          {' '}
                          <a
                            className="underline text-glow"
                            href={txidExplorerLink(transfer.txid)}
                            target="_blank"
                            rel="noreferrer noopener"
                          >
                            {t('nodeRetirement.viewInExplorer')}
                          </a>
                        </>
                      )}
                    </>
                  ) : (
                    <span className="text-fog">-</span>
                  )}
                </p>
                <p>{t('nodeRetirement.transferConfs')}: <span className="text-fog">{transfer.confirmations ?? 0}</span></p>
                <p>{t('nodeRetirement.transferFeeRate')}: <span className="text-fog">{transfer.sat_per_vbyte} sat/vb</span></p>
                <p>{t('nodeRetirement.transferMinConfs')}: <span className="text-fog">{transfer.min_confirmations}</span></p>
                <p>{t('nodeRetirement.transferLastAttempt')}: <span className="text-fog">{formatTs(transfer.last_attempt_at)}</span></p>
                <p>{t('nodeRetirement.transferUpdated')}: <span className="text-fog">{formatTs(transfer.updated_at)}</span></p>
                {transfer.last_error && <p className="text-amber-200 lg:col-span-2">{transfer.last_error}</p>}
              </div>
            )}
          </div>
        )}
      </div>

      <div className="section-card space-y-4">
        <h3 className="text-lg font-semibold">{t('nodeRetirement.successionTitle')}</h3>
        <p className="text-sm text-fog/60">{t('nodeRetirement.successionSubtitle')}</p>
        <div className="grid gap-3 lg:grid-cols-2">
          <label className="text-sm text-fog/70">
            <input
              className="mr-2"
              type="checkbox"
              checked={successionForm.enabled}
              disabled={!telegramMirrorEnabled && !successionForm.enabled}
              onChange={(e) => {
                const checked = e.target.checked
                if (checked && !telegramMirrorEnabled) {
                  setSuccessionStatus(t('nodeRetirement.telegramMirrorRequiredError'))
                  return
                }
                setSuccessionForm((prev) => ({ ...prev, enabled: checked }))
              }}
            />
            {t('nodeRetirement.successionEnabled')}
          </label>
          <label className="text-sm text-fog/70">
            <input
              className="mr-2"
              type="checkbox"
              checked={successionForm.dry_run}
              onChange={(e) => setSuccessionForm((prev) => ({ ...prev, dry_run: e.target.checked }))}
            />
            {t('nodeRetirement.successionDryRun')}
          </label>
          <p className={`text-xs lg:col-span-2 ${telegramMirrorEnabled ? 'text-emerald-200/80' : 'text-amber-200'}`}>
            {telegramMirrorEnabled
              ? t('nodeRetirement.telegramMirrorReady')
              : t('nodeRetirement.telegramMirrorRequiredHint')}
          </p>
          <label className="text-sm text-fog/70 lg:col-span-2">
            {t('nodeRetirement.destinationAddress')}
            <input
              className="input-field mt-1"
              value={successionForm.destination_address}
              onChange={(e) => setSuccessionForm((prev) => ({ ...prev, destination_address: e.target.value }))}
              placeholder="bc1..."
            />
          </label>
          <label className="text-sm text-fog/70">
            {t('nodeRetirement.checkPeriodDays')}
            <input
              className="input-field mt-1"
              type="number"
              min={1}
              value={successionForm.check_period_days}
              onChange={(e) => setSuccessionForm((prev) => ({ ...prev, check_period_days: Number(e.target.value) || 1 }))}
            />
            <p className="mt-1 text-xs text-fog/55">{t('nodeRetirement.checkPeriodHint')}</p>
          </label>
          <label className="text-sm text-fog/70">
            {t('nodeRetirement.reminderDays')}
            <input
              className="input-field mt-1"
              type="number"
              min={1}
              value={successionForm.reminder_period_days}
              onChange={(e) => setSuccessionForm((prev) => ({ ...prev, reminder_period_days: Number(e.target.value) || 1 }))}
            />
            <p className="mt-1 text-xs text-fog/55">{t('nodeRetirement.reminderDaysHint')}</p>
          </label>
          <label className="text-sm text-fog/70">
            {t('nodeRetirement.sweepMinConfs')}
            <input
              className="input-field mt-1"
              type="number"
              min={1}
              value={successionForm.sweep_min_confs}
              onChange={(e) => setSuccessionForm((prev) => ({ ...prev, sweep_min_confs: Number(e.target.value) || 1 }))}
            />
            <p className="mt-1 text-xs text-fog/55">{t('nodeRetirement.sweepMinConfsHint')}</p>
          </label>
          <label className="text-sm text-fog/70">
            {t('nodeRetirement.sweepSatPerVbyte')}
            <input
              className="input-field mt-1"
              type="number"
              min={0}
              value={successionForm.sweep_sat_per_vbyte}
              onChange={(e) => setSuccessionForm((prev) => ({ ...prev, sweep_sat_per_vbyte: Number(e.target.value) || 0 }))}
            />
            <p className="mt-1 text-xs text-fog/55">{t('nodeRetirement.sweepSatPerVbyteHint')}</p>
          </label>
          <label className="text-sm text-fog/70">
            <input
              className="mr-2"
              type="checkbox"
              checked={successionForm.preapprove_fc_offline}
              onChange={(e) => setSuccessionForm((prev) => ({ ...prev, preapprove_fc_offline: e.target.checked }))}
            />
            {t('nodeRetirement.preapproveOffline')}
          </label>
          <label className="text-sm text-fog/70">
            <input
              className="mr-2"
              type="checkbox"
              checked={successionForm.preapprove_fc_stuck_htlc}
              onChange={(e) => setSuccessionForm((prev) => ({ ...prev, preapprove_fc_stuck_htlc: e.target.checked }))}
            />
            {t('nodeRetirement.preapproveStuck')}
          </label>
        </div>
        <div className="flex flex-wrap gap-2">
          <button className="btn-primary text-xs px-3 py-2" type="button" onClick={handleSaveSuccession} disabled={successionBusy}>
            {t('common.save')}
          </button>
          <button className="btn-secondary text-xs px-3 py-2" type="button" onClick={handleAlive} disabled={successionBusy}>
            {t('nodeRetirement.aliveNow')}
          </button>
          <button className="btn-secondary text-xs px-3 py-2" type="button" onClick={() => handleSimulate('alive')} disabled={successionBusy}>
            {t('nodeRetirement.simulateAlive')}
          </button>
          <button className="btn-secondary text-xs px-3 py-2" type="button" onClick={() => handleSimulate('not_alive')} disabled={successionBusy}>
            {t('nodeRetirement.simulateNotAlive')}
          </button>
        </div>
        {successionStatus && <p className="text-sm text-brass">{successionStatus}</p>}
        <div className="grid gap-2 text-xs text-fog/60 lg:grid-cols-4">
          <p>{t('nodeRetirement.successionState')}: <span className="text-fog">{succession?.status || '-'}</span></p>
          <p>{t('nodeRetirement.lastAlive')}: <span className="text-fog">{formatTs(succession?.last_alive_at)}</span></p>
          <p>{t('nodeRetirement.nextCheck')}: <span className="text-fog">{formatTs(succession?.next_check_at)}</span></p>
          <p>{t('nodeRetirement.deadline')}: <span className="text-fog">{formatTs(succession?.deadline_at)}</span></p>
        </div>
      </div>
      {confirmCoopOpen && (
        <div className="fixed inset-0 z-50 bg-black/70 flex items-center justify-center px-4">
          <div className="w-full max-w-xl rounded-2xl border border-red-400/40 bg-ink p-5 space-y-3">
            <h4 className="text-lg font-semibold text-red-100">{t('nodeRetirement.coopModalTitle')}</h4>
            <p className="text-sm text-fog/80">{t('nodeRetirement.coopModalBody')}</p>
            <div className="flex gap-2 justify-end">
              <button className="btn-secondary text-xs px-3 py-2" type="button" onClick={() => setConfirmCoopOpen(false)}>
                {t('common.cancel')}
              </button>
              <button className="btn-primary text-xs px-3 py-2" type="button" onClick={handleConfirmCoopClose} disabled={sessionActionBusy}>
                {sessionActionBusy ? t('common.saving') : t('nodeRetirement.coopModalConfirm')}
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  )
}
