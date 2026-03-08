import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  createNodeRetirementSession,
  getNodeRetirementSessionEvents,
  getNodeRetirementSessions,
  getNodeRetirementStatus,
  getSuccessionConfig,
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
  created_at: string
  last_error?: string
}

type NodeRetirementEvent = {
  id: number
  event_type: string
  severity: string
  created_at: string
}

type SuccessionConfig = {
  enabled: boolean
  dry_run: boolean
  destination_address: string
  preapprove_fc_offline: boolean
  preapprove_fc_stuck_htlc: boolean
  stuck_htlc_threshold_sec: number
  check_period_days: number
  reminder_period_days: number
  last_alive_at?: string
  next_check_at?: string
  deadline_at?: string
  status: string
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

export default function NodeRetirement() {
  const { t } = useTranslation()
  const [loading, setLoading] = useState(true)
  const [status, setStatus] = useState<NodeRetirementStatus | null>(null)
  const [sessions, setSessions] = useState<NodeRetirementSession[]>([])
  const [selectedSessionID, setSelectedSessionID] = useState('')
  const [events, setEvents] = useState<NodeRetirementEvent[]>([])
  const [disclaimerAccepted, setDisclaimerAccepted] = useState(false)
  const [dryRun, setDryRun] = useState(true)
  const [sessionStatus, setSessionStatus] = useState('')
  const [sessionBusy, setSessionBusy] = useState(false)
  const [succession, setSuccession] = useState<SuccessionConfig | null>(null)
  const [successionStatus, setSuccessionStatus] = useState('')
  const [successionBusy, setSuccessionBusy] = useState(false)
  const [successionForm, setSuccessionForm] = useState<SuccessionConfig>({
    enabled: false,
    dry_run: true,
    destination_address: '',
    preapprove_fc_offline: false,
    preapprove_fc_stuck_htlc: false,
    stuck_htlc_threshold_sec: 86400,
    check_period_days: 30,
    reminder_period_days: 30,
    status: 'disabled',
  })

  const loadAll = async (silent = false) => {
    if (!silent) setLoading(true)
    try {
      const [statusRes, sessionsRes, successionRes] = await Promise.all([
        getNodeRetirementStatus(),
        getNodeRetirementSessions(50),
        getSuccessionConfig(),
      ])

      const statusData = statusRes as NodeRetirementStatus
      const sessionItems = Array.isArray((sessionsRes as any)?.items)
        ? ((sessionsRes as any).items as NodeRetirementSession[])
        : []
      const successionData = successionRes as SuccessionConfig

      setStatus(statusData)
      setSessions(sessionItems)
      setSuccession(successionData)
      setSuccessionForm(successionData)

      const activeID = statusData.active_session_id || sessionItems[0]?.session_id || ''
      if (activeID) {
        setSelectedSessionID((prev) => prev || activeID)
      } else {
        setSelectedSessionID('')
        setEvents([])
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
    const loadEvents = async () => {
      if (!selectedSessionID) {
        if (mounted) setEvents([])
        return
      }
      try {
        const data: any = await getNodeRetirementSessionEvents(selectedSessionID, 120)
        if (!mounted) return
        setEvents(Array.isArray(data?.items) ? data.items : [])
      } catch {
        if (!mounted) return
      }
    }
    void loadEvents()
    return () => {
      mounted = false
    }
  }, [selectedSessionID])

  const activeSession = useMemo(
    () => sessions.find((item) => item.session_id === selectedSessionID) || sessions[0] || null,
    [sessions, selectedSessionID]
  )

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

  const handleSaveSuccession = async () => {
    setSuccessionBusy(true)
    setSuccessionStatus('')
    try {
      const next = await updateSuccessionConfig({
        enabled: successionForm.enabled,
        dry_run: successionForm.dry_run,
        destination_address: successionForm.destination_address,
        preapprove_fc_offline: successionForm.preapprove_fc_offline,
        preapprove_fc_stuck_htlc: successionForm.preapprove_fc_stuck_htlc,
        stuck_htlc_threshold_sec: successionForm.stuck_htlc_threshold_sec,
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
                  {item.source} · {item.dry_run ? 'dry-run' : 'live'} · {formatTs(item.created_at)}
                </p>
                {item.last_error && <p className="text-xs text-amber-200 mt-1">{item.last_error}</p>}
              </button>
            ))}
          </div>
        )}
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
              onChange={(e) => setSuccessionForm((prev) => ({ ...prev, enabled: e.target.checked }))}
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
        <div className="grid gap-2 text-xs text-fog/60 lg:grid-cols-3">
          <p>{t('nodeRetirement.successionState')}: <span className="text-fog">{succession?.status || '-'}</span></p>
          <p>{t('nodeRetirement.lastAlive')}: <span className="text-fog">{formatTs(succession?.last_alive_at)}</span></p>
          <p>{t('nodeRetirement.nextCheck')}: <span className="text-fog">{formatTs(succession?.next_check_at)}</span></p>
        </div>
      </div>
    </section>
  )
}

