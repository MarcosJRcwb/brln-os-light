package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"lightningos-light/internal/lndclient"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	balancedOpenDefaultListLimit = 50
	balancedOpenMaxListLimit     = 500
	balancedOpenDefaultEventList = 100
	balancedOpenMaxEventList     = 500
	balancedOpenMinCapacitySat   = 20000
	balancedOpenProtocolVersion  = 1
	balancedOpenCustomMsgType    = uint32(42069)
	balancedOpenReconcileEvery   = 20 * time.Second
	balancedOpenExecutionMode    = "push_sat_v1"
)

const (
	balancedOpenRoleInitiator = "initiator"
	balancedOpenRoleAccepter  = "accepter"
)

const (
	balancedOpenStateSessionCreated       = "session_created"
	balancedOpenStateProposalSent         = "proposal_sent"
	balancedOpenStateProposalReceived     = "proposal_received"
	balancedOpenStateAccepted             = "accepted"
	balancedOpenStateFundingTxHalfSigned  = "funding_tx_half_signed"
	balancedOpenStateFundingTxFullySigned = "funding_tx_fully_signed"
	balancedOpenStateChannelProposed      = "channel_proposed_to_lnd"
	balancedOpenStateFundingBroadcasted   = "funding_broadcasted"
	balancedOpenStatePendingOpenDetected  = "pending_open_detected"
	balancedOpenStateActive               = "active"
	balancedOpenStateFailed               = "failed"
	balancedOpenStateCanceled             = "canceled"
	balancedOpenStateRecoveryRequired     = "recovery_required"
	balancedOpenStateRecovered            = "recovered"
)

const (
	balancedOpenMessageKindProposal = "proposal"
	balancedOpenMessageKindAccept   = "accept"
	balancedOpenMessageKindCancel   = "cancel"
)

var (
	ErrBalancedOpenDBUnavailable   = errors.New("balanced open db unavailable")
	ErrBalancedOpenSessionNotFound = errors.New("balanced open session not found")
	ErrBalancedOpenInvalidPeerKey  = errors.New("invalid peer pubkey")
	ErrBalancedOpenInvalidCapacity = errors.New("invalid capacity")
	ErrBalancedOpenInvalidFeeRate  = errors.New("invalid fee rate")
	ErrBalancedOpenInvalidRole     = errors.New("invalid role")
	ErrBalancedOpenInvalidState    = errors.New("invalid state")
	ErrBalancedOpenTerminalState   = errors.New("session already in terminal state")
	ErrBalancedOpenInvalidSession  = errors.New("invalid session")
	ErrBalancedOpenInvalidAction   = errors.New("invalid session action")
)

type BalancedOpenService struct {
	db     *pgxpool.Pool
	lnd    *lndclient.Client
	logger *log.Logger

	mu           sync.Mutex
	started      bool
	stopCh       chan struct{}
	streamCancel context.CancelFunc
}

type BalancedOpenSession struct {
	ID             int64           `json:"id"`
	SessionID      string          `json:"session_id"`
	Role           string          `json:"role"`
	PeerPubkey     string          `json:"peer_pubkey"`
	PeerHost       string          `json:"peer_host,omitempty"`
	CapacitySat    int64           `json:"capacity_sat"`
	FeeRateSatVb   int64           `json:"fee_rate_sat_vb"`
	Private        bool            `json:"private"`
	CloseAddress   string          `json:"close_address,omitempty"`
	State          string          `json:"state"`
	StateUpdatedAt time.Time       `json:"state_updated_at"`
	Attempts       int             `json:"attempts"`
	NextRetryAt    *time.Time      `json:"next_retry_at,omitempty"`
	LastError      string          `json:"last_error,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type BalancedOpenEvent struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"session_id"`
	EventType string          `json:"event_type"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type BalancedOpenCreateParams struct {
	PeerPubkey   string
	PeerHost     string
	CapacitySat  int64
	FeeRateSatVb int64
	Private      bool
	CloseAddress string
	Role         string
	Metadata     json.RawMessage
}

type BalancedOpenListFilter struct {
	State      string
	Role       string
	PeerPubkey string
	Limit      int
}

type balancedOpenProtocolMessage struct {
	Version      int    `json:"v"`
	Kind         string `json:"kind"`
	SessionID    string `json:"session_id"`
	FromPubkey   string `json:"from_pubkey,omitempty"`
	ToPubkey     string `json:"to_pubkey,omitempty"`
	PeerHost     string `json:"peer_host,omitempty"`
	CapacitySat  int64  `json:"capacity_sat,omitempty"`
	FeeRateSatVb int64  `json:"fee_rate_sat_vb,omitempty"`
	Private      bool   `json:"private,omitempty"`
	CloseAddress string `json:"close_address,omitempty"`
	Reason       string `json:"reason,omitempty"`
	SentAtUnix   int64  `json:"sent_at_unix"`
}

type balancedOpenScanner interface {
	Scan(dest ...any) error
}

type balancedOpenEventScanner interface {
	Scan(dest ...any) error
}

func NewBalancedOpenService(db *pgxpool.Pool, lnd *lndclient.Client, logger *log.Logger) *BalancedOpenService {
	return &BalancedOpenService{
		db:     db,
		lnd:    lnd,
		logger: logger,
	}
}

func (s *BalancedOpenService) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return ErrBalancedOpenDBUnavailable
	}

	_, err := s.db.Exec(ctx, `
create table if not exists balanced_open_sessions (
  id bigserial primary key,
  session_id text not null unique,
  role text not null check (role in ('initiator', 'accepter')),
  peer_pubkey text not null,
  peer_host text not null default '',
  capacity_sat bigint not null,
  fee_rate_sat_vb bigint not null default 0,
  is_private boolean not null default false,
  close_address text not null default '',
  state text not null,
  state_updated_at timestamptz not null default now(),
  attempts integer not null default 0,
  next_retry_at timestamptz,
  last_error text not null default '',
  metadata jsonb not null default '{}'::jsonb,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

alter table balanced_open_sessions add column if not exists peer_host text not null default '';
alter table balanced_open_sessions add column if not exists fee_rate_sat_vb bigint not null default 0;
alter table balanced_open_sessions add column if not exists is_private boolean not null default false;
alter table balanced_open_sessions add column if not exists close_address text not null default '';
alter table balanced_open_sessions add column if not exists state_updated_at timestamptz not null default now();
alter table balanced_open_sessions add column if not exists attempts integer not null default 0;
alter table balanced_open_sessions add column if not exists next_retry_at timestamptz;
alter table balanced_open_sessions add column if not exists last_error text not null default '';
alter table balanced_open_sessions add column if not exists metadata jsonb not null default '{}'::jsonb;

create index if not exists balanced_open_sessions_state_idx on balanced_open_sessions (state, updated_at desc);
create index if not exists balanced_open_sessions_peer_idx on balanced_open_sessions (peer_pubkey, created_at desc);
create index if not exists balanced_open_sessions_retry_idx on balanced_open_sessions (next_retry_at) where next_retry_at is not null;

create table if not exists balanced_open_events (
  id bigserial primary key,
  session_id text not null references balanced_open_sessions(session_id) on delete cascade,
  event_type text not null,
  detail jsonb not null default '{}'::jsonb,
  created_at timestamptz not null default now()
);

create index if not exists balanced_open_events_session_idx on balanced_open_events (session_id, created_at desc);
`)
	return err
}

func (s *BalancedOpenService) Start() {
	if s == nil || s.lnd == nil {
		return
	}

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.stopCh = make(chan struct{})
	s.mu.Unlock()

	go s.runMessageLoop()
	go s.runReconcileLoop()
}

func (s *BalancedOpenService) Stop() {
	if s == nil {
		return
	}

	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	stopCh := s.stopCh
	s.stopCh = nil
	cancel := s.streamCancel
	s.streamCancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stopCh != nil {
		close(stopCh)
	}
}

func (s *BalancedOpenService) runMessageLoop() {
	for {
		s.mu.Lock()
		stopCh := s.stopCh
		s.mu.Unlock()

		if stopCh == nil {
			return
		}

		streamCtx, cancel := context.WithCancel(context.Background())
		s.mu.Lock()
		s.streamCancel = cancel
		s.mu.Unlock()

		msgs, errs := s.lnd.SubscribeCustomMessages(streamCtx)
		shouldStop := false

		for !shouldStop {
			select {
			case <-stopCh:
				shouldStop = true
			case msg, ok := <-msgs:
				if !ok {
					shouldStop = true
					break
				}
				s.handleIncomingCustomMessage(msg)
			case err, ok := <-errs:
				if ok && err != nil && s.logger != nil {
					s.logger.Printf("balanced-open: custom message stream error: %v", err)
				}
				shouldStop = true
			}
		}

		cancel()

		s.mu.Lock()
		s.streamCancel = nil
		stopCh = s.stopCh
		s.mu.Unlock()

		if stopCh == nil {
			return
		}

		select {
		case <-stopCh:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *BalancedOpenService) runReconcileLoop() {
	ticker := time.NewTicker(balancedOpenReconcileEvery)
	defer ticker.Stop()

	for {
		s.mu.Lock()
		stopCh := s.stopCh
		s.mu.Unlock()
		if stopCh == nil {
			return
		}

		select {
		case <-stopCh:
			return
		case <-ticker.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := s.reconcileSessions(ctx); err != nil && s.logger != nil {
			s.logger.Printf("balanced-open: reconcile failed: %v", err)
		}
		cancel()
	}
}

func (s *BalancedOpenService) ExecuteSession(ctx context.Context, sessionID string) (BalancedOpenSession, error) {
	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	if isBalancedOpenTerminalState(session.State) {
		return BalancedOpenSession{}, ErrBalancedOpenTerminalState
	}
	if session.Role != balancedOpenRoleInitiator {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidAction
	}
	if session.State != balancedOpenStateAccepted && session.State != balancedOpenStateRecoveryRequired {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidAction
	}

	pushSat := session.CapacitySat / 2
	if pushSat <= 0 {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidCapacity
	}

	submitted, err := s.transitionSession(ctx, session.SessionID, balancedOpenStateChannelProposed, "", "channel_open_submitted", map[string]any{
		"execution_mode":    balancedOpenExecutionMode,
		"capacity_sat":      session.CapacitySat,
		"local_funding_sat": session.CapacitySat,
		"push_sat":          pushSat,
	})
	if err != nil {
		return BalancedOpenSession{}, err
	}

	if strings.TrimSpace(submitted.PeerHost) != "" {
		if err := s.connectPeerForBalancedOpen(ctx, submitted.PeerPubkey, submitted.PeerHost); err != nil {
			_, _ = s.transitionSession(ctx, submitted.SessionID, balancedOpenStateRecoveryRequired, err.Error(), "channel_open_connect_failed", map[string]any{
				"execution_mode": balancedOpenExecutionMode,
			})
			return BalancedOpenSession{}, err
		}
	}

	channelPoint, err := s.lnd.OpenChannelWithPush(
		ctx,
		submitted.PeerPubkey,
		submitted.CapacitySat,
		pushSat,
		submitted.CloseAddress,
		submitted.Private,
		submitted.FeeRateSatVb,
	)
	if err != nil {
		_, _ = s.transitionSession(ctx, submitted.SessionID, balancedOpenStateRecoveryRequired, err.Error(), "channel_open_failed", map[string]any{
			"execution_mode": balancedOpenExecutionMode,
			"capacity_sat":   submitted.CapacitySat,
			"push_sat":       pushSat,
		})
		return BalancedOpenSession{}, err
	}

	updated, err := s.transitionSessionWithMetadata(ctx, submitted.SessionID, balancedOpenStateFundingBroadcasted, "", "funding_broadcasted_local", map[string]any{
		"execution_mode": balancedOpenExecutionMode,
		"channel_point":  channelPoint,
		"capacity_sat":   submitted.CapacitySat,
		"push_sat":       pushSat,
	}, map[string]any{
		"channel_point":      channelPoint,
		"execution_mode":     balancedOpenExecutionMode,
		"local_funding_sat":  submitted.CapacitySat,
		"push_sat":           pushSat,
		"opened_at_unix":     time.Now().UTC().Unix(),
		"last_execution_err": "",
	})
	if err != nil {
		return BalancedOpenSession{}, err
	}
	return updated, nil
}

func (s *BalancedOpenService) reconcileSessions(ctx context.Context) error {
	if s == nil || s.db == nil {
		return ErrBalancedOpenDBUnavailable
	}

	sessions, err := s.listSessionsByStates(ctx, []string{
		balancedOpenStateAccepted,
		balancedOpenStateChannelProposed,
		balancedOpenStateFundingBroadcasted,
		balancedOpenStatePendingOpenDetected,
		balancedOpenStateRecoveryRequired,
	})
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return nil
	}

	pending, err := s.lnd.ListPendingChannels(ctx)
	if err != nil {
		return err
	}
	active, err := s.lnd.ListChannels(ctx)
	if err != nil {
		return err
	}

	for _, session := range sessions {
		if isBalancedOpenTerminalState(session.State) {
			continue
		}

		if session.Role == balancedOpenRoleInitiator && session.State == balancedOpenStateAccepted {
			if _, err := s.ExecuteSession(ctx, session.SessionID); err != nil && s.logger != nil {
				s.logger.Printf("balanced-open: auto execute failed session=%s err=%v", session.SessionID, err)
			}
			continue
		}

		if err := s.tryPromoteSessionByChannelState(ctx, session, pending, active); err != nil && s.logger != nil {
			s.logger.Printf("balanced-open: state promotion failed session=%s err=%v", session.SessionID, err)
		}
	}

	return nil
}

func (s *BalancedOpenService) tryPromoteSessionByChannelState(ctx context.Context, session BalancedOpenSession, pending []lndclient.PendingChannelInfo, active []lndclient.ChannelInfo) error {
	channelPointHint := balancedOpenSessionChannelPoint(session.Metadata)

	if activeCh, ok := matchBalancedOpenActiveChannel(session, channelPointHint, active); ok {
		if session.State != balancedOpenStateActive {
			_, err := s.transitionSessionWithMetadata(ctx, session.SessionID, balancedOpenStateActive, "", "channel_active_detected", map[string]any{
				"channel_point": activeCh.ChannelPoint,
				"capacity_sat":  activeCh.CapacitySat,
			}, map[string]any{
				"channel_point": activeCh.ChannelPoint,
			})
			return err
		}
		return nil
	}

	if pendingCh, ok := matchBalancedOpenPendingChannel(session, channelPointHint, pending); ok {
		if session.State != balancedOpenStatePendingOpenDetected {
			_, err := s.transitionSessionWithMetadata(ctx, session.SessionID, balancedOpenStatePendingOpenDetected, "", "pending_open_detected", map[string]any{
				"channel_point":              pendingCh.ChannelPoint,
				"capacity_sat":               pendingCh.CapacitySat,
				"confirmations_until_active": pendingCh.ConfirmationsUntilActive,
			}, map[string]any{
				"channel_point": pendingCh.ChannelPoint,
			})
			return err
		}
	}

	return nil
}

func (s *BalancedOpenService) ProposeSession(ctx context.Context, sessionID string) (BalancedOpenSession, error) {
	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	if isBalancedOpenTerminalState(session.State) {
		return BalancedOpenSession{}, ErrBalancedOpenTerminalState
	}
	if session.Role != balancedOpenRoleInitiator {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidAction
	}
	if !isProposalEligibleState(session.State) {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidAction
	}

	msg := balancedOpenProtocolMessage{
		Version:      balancedOpenProtocolVersion,
		Kind:         balancedOpenMessageKindProposal,
		SessionID:    session.SessionID,
		ToPubkey:     session.PeerPubkey,
		PeerHost:     session.PeerHost,
		CapacitySat:  session.CapacitySat,
		FeeRateSatVb: session.FeeRateSatVb,
		Private:      session.Private,
		CloseAddress: session.CloseAddress,
		SentAtUnix:   time.Now().UTC().Unix(),
	}

	if self, err := s.lnd.SelfPubkey(ctx); err == nil {
		msg.FromPubkey = strings.ToLower(strings.TrimSpace(self))
	}

	if strings.TrimSpace(session.PeerHost) != "" {
		if err := s.connectPeerForBalancedOpen(ctx, session.PeerPubkey, session.PeerHost); err != nil {
			return BalancedOpenSession{}, err
		}
	}

	if err := s.sendProtocolMessage(ctx, session.PeerPubkey, msg); err != nil {
		return BalancedOpenSession{}, err
	}

	updated, err := s.transitionSession(ctx, session.SessionID, balancedOpenStateProposalSent, "", "proposal_sent", map[string]any{
		"kind":         msg.Kind,
		"sent_at_unix": msg.SentAtUnix,
	})
	if err != nil {
		return BalancedOpenSession{}, err
	}
	return updated, nil
}

func (s *BalancedOpenService) AcceptSession(ctx context.Context, sessionID string) (BalancedOpenSession, error) {
	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	if isBalancedOpenTerminalState(session.State) {
		return BalancedOpenSession{}, ErrBalancedOpenTerminalState
	}
	if session.Role != balancedOpenRoleAccepter {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidAction
	}
	if session.State != balancedOpenStateProposalReceived {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidAction
	}

	msg := balancedOpenProtocolMessage{
		Version:    balancedOpenProtocolVersion,
		Kind:       balancedOpenMessageKindAccept,
		SessionID:  session.SessionID,
		ToPubkey:   session.PeerPubkey,
		SentAtUnix: time.Now().UTC().Unix(),
	}

	if self, err := s.lnd.SelfPubkey(ctx); err == nil {
		msg.FromPubkey = strings.ToLower(strings.TrimSpace(self))
	}

	if strings.TrimSpace(session.PeerHost) != "" {
		if err := s.connectPeerForBalancedOpen(ctx, session.PeerPubkey, session.PeerHost); err != nil {
			return BalancedOpenSession{}, err
		}
	}

	if err := s.sendProtocolMessage(ctx, session.PeerPubkey, msg); err != nil {
		return BalancedOpenSession{}, err
	}

	updated, err := s.transitionSession(ctx, session.SessionID, balancedOpenStateAccepted, "", "proposal_accepted_local", map[string]any{
		"kind":         msg.Kind,
		"sent_at_unix": msg.SentAtUnix,
	})
	if err != nil {
		return BalancedOpenSession{}, err
	}
	return updated, nil
}

func (s *BalancedOpenService) CreateSession(ctx context.Context, params BalancedOpenCreateParams) (BalancedOpenSession, error) {
	if s == nil || s.db == nil {
		return BalancedOpenSession{}, ErrBalancedOpenDBUnavailable
	}

	pubkey := strings.ToLower(strings.TrimSpace(params.PeerPubkey))
	if !isValidPubkeyHex(pubkey) {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidPeerKey
	}
	if params.CapacitySat < balancedOpenMinCapacitySat || params.CapacitySat%2 != 0 {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidCapacity
	}
	if params.FeeRateSatVb < 0 {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidFeeRate
	}

	role := strings.TrimSpace(params.Role)
	if role == "" {
		role = balancedOpenRoleInitiator
	}
	if !isBalancedOpenRole(role) {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidRole
	}

	meta := params.Metadata
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	} else if !json.Valid(meta) {
		return BalancedOpenSession{}, errors.New("invalid metadata json")
	}

	sessionID, err := newBalancedOpenSessionID()
	if err != nil {
		return BalancedOpenSession{}, err
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return BalancedOpenSession{}, err
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
insert into balanced_open_sessions (
  session_id,
  role,
  peer_pubkey,
  peer_host,
  capacity_sat,
  fee_rate_sat_vb,
  is_private,
  close_address,
  state,
  metadata
)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)
returning
  id,
  session_id,
  role,
  peer_pubkey,
  peer_host,
  capacity_sat,
  fee_rate_sat_vb,
  is_private,
  close_address,
  state,
  state_updated_at,
  attempts,
  next_retry_at,
  last_error,
  metadata,
  created_at,
  updated_at
`,
		sessionID,
		role,
		pubkey,
		strings.TrimSpace(params.PeerHost),
		params.CapacitySat,
		params.FeeRateSatVb,
		params.Private,
		strings.TrimSpace(params.CloseAddress),
		balancedOpenStateSessionCreated,
		meta,
	)

	session, err := scanBalancedOpenSession(row)
	if err != nil {
		return BalancedOpenSession{}, err
	}

	if err := s.appendEventTx(ctx, tx, sessionID, "session_created", map[string]any{
		"role":         role,
		"capacity_sat": params.CapacitySat,
		"fee_rate":     params.FeeRateSatVb,
		"private":      params.Private,
	}); err != nil {
		return BalancedOpenSession{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return BalancedOpenSession{}, err
	}

	return session, nil
}

func (s *BalancedOpenService) GetSession(ctx context.Context, sessionID string) (BalancedOpenSession, error) {
	if s == nil || s.db == nil {
		return BalancedOpenSession{}, ErrBalancedOpenDBUnavailable
	}

	id := strings.TrimSpace(sessionID)
	if id == "" {
		return BalancedOpenSession{}, ErrBalancedOpenSessionNotFound
	}

	row := s.db.QueryRow(ctx, `
select
  id,
  session_id,
  role,
  peer_pubkey,
  peer_host,
  capacity_sat,
  fee_rate_sat_vb,
  is_private,
  close_address,
  state,
  state_updated_at,
  attempts,
  next_retry_at,
  last_error,
  metadata,
  created_at,
  updated_at
from balanced_open_sessions
where session_id = $1
`, id)

	session, err := scanBalancedOpenSession(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BalancedOpenSession{}, ErrBalancedOpenSessionNotFound
		}
		return BalancedOpenSession{}, err
	}
	return session, nil
}

func (s *BalancedOpenService) ListSessionEvents(ctx context.Context, sessionID string, limit int) ([]BalancedOpenEvent, error) {
	if s == nil || s.db == nil {
		return nil, ErrBalancedOpenDBUnavailable
	}

	id := strings.TrimSpace(sessionID)
	if id == "" {
		return nil, ErrBalancedOpenSessionNotFound
	}

	if _, err := s.GetSession(ctx, id); err != nil {
		return nil, err
	}

	n := limit
	if n <= 0 {
		n = balancedOpenDefaultEventList
	}
	if n > balancedOpenMaxEventList {
		n = balancedOpenMaxEventList
	}

	rows, err := s.db.Query(ctx, `
select
  id,
  session_id,
  event_type,
  detail,
  created_at
from balanced_open_events
where session_id = $1
order by created_at desc
limit $2
`, id, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]BalancedOpenEvent, 0)
	for rows.Next() {
		event, err := scanBalancedOpenEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *BalancedOpenService) ListSessions(ctx context.Context, filter BalancedOpenListFilter) ([]BalancedOpenSession, error) {
	if s == nil || s.db == nil {
		return nil, ErrBalancedOpenDBUnavailable
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = balancedOpenDefaultListLimit
	}
	if limit > balancedOpenMaxListLimit {
		limit = balancedOpenMaxListLimit
	}

	state := strings.TrimSpace(filter.State)
	role := strings.TrimSpace(filter.Role)
	peer := strings.ToLower(strings.TrimSpace(filter.PeerPubkey))

	query := `
select
  id,
  session_id,
  role,
  peer_pubkey,
  peer_host,
  capacity_sat,
  fee_rate_sat_vb,
  is_private,
  close_address,
  state,
  state_updated_at,
  attempts,
  next_retry_at,
  last_error,
  metadata,
  created_at,
  updated_at
from balanced_open_sessions
where 1=1
`
	args := []any{}
	arg := 1

	if state != "" {
		if !isBalancedOpenState(state) {
			return nil, ErrBalancedOpenInvalidState
		}
		query += fmt.Sprintf(" and state = $%d", arg)
		args = append(args, state)
		arg++
	}

	if role != "" {
		if !isBalancedOpenRole(role) {
			return nil, ErrBalancedOpenInvalidRole
		}
		query += fmt.Sprintf(" and role = $%d", arg)
		args = append(args, role)
		arg++
	}

	if peer != "" {
		if !isValidPubkeyHex(peer) {
			return nil, ErrBalancedOpenInvalidPeerKey
		}
		query += fmt.Sprintf(" and peer_pubkey = $%d", arg)
		args = append(args, peer)
		arg++
	}

	query += fmt.Sprintf(" order by created_at desc limit $%d", arg)
	args = append(args, limit)

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]BalancedOpenSession, 0)
	for rows.Next() {
		session, err := scanBalancedOpenSession(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *BalancedOpenService) CancelSession(ctx context.Context, sessionID string, reason string) (BalancedOpenSession, error) {
	if s == nil || s.db == nil {
		return BalancedOpenSession{}, ErrBalancedOpenDBUnavailable
	}

	current, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	if isBalancedOpenTerminalState(current.State) {
		return BalancedOpenSession{}, ErrBalancedOpenTerminalState
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return BalancedOpenSession{}, err
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
update balanced_open_sessions
set
  state = $2,
  state_updated_at = now(),
  updated_at = now(),
  last_error = $3
where session_id = $1
returning
  id,
  session_id,
  role,
  peer_pubkey,
  peer_host,
  capacity_sat,
  fee_rate_sat_vb,
  is_private,
  close_address,
  state,
  state_updated_at,
  attempts,
  next_retry_at,
  last_error,
  metadata,
  created_at,
  updated_at
`, strings.TrimSpace(sessionID), balancedOpenStateCanceled, strings.TrimSpace(reason))

	session, err := scanBalancedOpenSession(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BalancedOpenSession{}, ErrBalancedOpenSessionNotFound
		}
		return BalancedOpenSession{}, err
	}

	if err := s.appendEventTx(ctx, tx, session.SessionID, "session_canceled", map[string]any{
		"reason": strings.TrimSpace(reason),
	}); err != nil {
		return BalancedOpenSession{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return BalancedOpenSession{}, err
	}

	return session, nil
}

func (s *BalancedOpenService) handleIncomingCustomMessage(msg lndclient.CustomPeerMessage) {
	if msg.Type != balancedOpenCustomMsgType {
		return
	}

	sender := strings.ToLower(strings.TrimSpace(msg.PeerPubkey))
	if !isValidPubkeyHex(sender) {
		return
	}

	var envelope balancedOpenProtocolMessage
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		if s.logger != nil {
			s.logger.Printf("balanced-open: invalid protocol payload from %s: %v", sender, err)
		}
		return
	}
	if envelope.Version != balancedOpenProtocolVersion || strings.TrimSpace(envelope.SessionID) == "" {
		return
	}
	if envelope.FromPubkey == "" {
		envelope.FromPubkey = sender
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch envelope.Kind {
	case balancedOpenMessageKindProposal:
		if _, err := s.upsertSessionFromProposal(ctx, sender, envelope); err != nil && s.logger != nil {
			s.logger.Printf("balanced-open: failed processing proposal session=%s from=%s err=%v", envelope.SessionID, sender, err)
		}
	case balancedOpenMessageKindAccept:
		if _, err := s.markAcceptedFromPeer(ctx, sender, envelope); err != nil && s.logger != nil {
			s.logger.Printf("balanced-open: failed processing accept session=%s from=%s err=%v", envelope.SessionID, sender, err)
		}
	case balancedOpenMessageKindCancel:
		if _, err := s.markCanceledFromPeer(ctx, sender, envelope); err != nil && s.logger != nil {
			s.logger.Printf("balanced-open: failed processing cancel session=%s from=%s err=%v", envelope.SessionID, sender, err)
		}
	default:
		return
	}
}

func (s *BalancedOpenService) upsertSessionFromProposal(ctx context.Context, sender string, msg balancedOpenProtocolMessage) (BalancedOpenSession, error) {
	if !isValidPubkeyHex(sender) {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidPeerKey
	}
	if msg.CapacitySat < balancedOpenMinCapacitySat || msg.CapacitySat%2 != 0 {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidCapacity
	}
	if msg.FeeRateSatVb < 0 {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidFeeRate
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return BalancedOpenSession{}, err
	}
	defer tx.Rollback(ctx)

	meta, err := marshalBalancedOpenJSON(map[string]any{
		"protocol_version": msg.Version,
		"last_message": map[string]any{
			"kind":         msg.Kind,
			"from_pubkey":  sender,
			"to_pubkey":    msg.ToPubkey,
			"sent_at_unix": msg.SentAtUnix,
		},
	})
	if err != nil {
		return BalancedOpenSession{}, err
	}

	row := tx.QueryRow(ctx, `
insert into balanced_open_sessions (
  session_id,
  role,
  peer_pubkey,
  peer_host,
  capacity_sat,
  fee_rate_sat_vb,
  is_private,
  close_address,
  state,
  state_updated_at,
  metadata
)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10::jsonb)
on conflict (session_id) do update
set
  peer_pubkey = excluded.peer_pubkey,
  peer_host = case when balanced_open_sessions.peer_host = '' then excluded.peer_host else balanced_open_sessions.peer_host end,
  capacity_sat = excluded.capacity_sat,
  fee_rate_sat_vb = excluded.fee_rate_sat_vb,
  is_private = excluded.is_private,
  close_address = excluded.close_address,
  state = case
    when balanced_open_sessions.state in ('active', 'failed', 'canceled', 'recovered') then balanced_open_sessions.state
    else $9
  end,
  state_updated_at = case
    when balanced_open_sessions.state in ('active', 'failed', 'canceled', 'recovered') then balanced_open_sessions.state_updated_at
    else now()
  end,
  metadata = coalesce(balanced_open_sessions.metadata, '{}'::jsonb) || $10::jsonb,
  updated_at = now()
returning
  id,
  session_id,
  role,
  peer_pubkey,
  peer_host,
  capacity_sat,
  fee_rate_sat_vb,
  is_private,
  close_address,
  state,
  state_updated_at,
  attempts,
  next_retry_at,
  last_error,
  metadata,
  created_at,
  updated_at
`,
		msg.SessionID,
		balancedOpenRoleAccepter,
		sender,
		strings.TrimSpace(msg.PeerHost),
		msg.CapacitySat,
		msg.FeeRateSatVb,
		msg.Private,
		strings.TrimSpace(msg.CloseAddress),
		balancedOpenStateProposalReceived,
		meta,
	)

	session, err := scanBalancedOpenSession(row)
	if err != nil {
		return BalancedOpenSession{}, err
	}

	if err := s.appendEventTx(ctx, tx, session.SessionID, "proposal_received", map[string]any{
		"from_pubkey":  sender,
		"capacity_sat": msg.CapacitySat,
		"fee_rate":     msg.FeeRateSatVb,
		"private":      msg.Private,
		"sent_at_unix": msg.SentAtUnix,
	}); err != nil {
		return BalancedOpenSession{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return BalancedOpenSession{}, err
	}
	return session, nil
}

func (s *BalancedOpenService) markAcceptedFromPeer(ctx context.Context, sender string, msg balancedOpenProtocolMessage) (BalancedOpenSession, error) {
	session, err := s.GetSession(ctx, msg.SessionID)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(session.PeerPubkey), sender) {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidPeerKey
	}
	if isBalancedOpenTerminalState(session.State) {
		return session, nil
	}

	updated, err := s.transitionSession(ctx, session.SessionID, balancedOpenStateAccepted, "", "proposal_accepted_remote", map[string]any{
		"from_pubkey":  sender,
		"sent_at_unix": msg.SentAtUnix,
	})
	if err != nil {
		return BalancedOpenSession{}, err
	}
	return updated, nil
}

func (s *BalancedOpenService) markCanceledFromPeer(ctx context.Context, sender string, msg balancedOpenProtocolMessage) (BalancedOpenSession, error) {
	session, err := s.GetSession(ctx, msg.SessionID)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(session.PeerPubkey), sender) {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidPeerKey
	}
	if isBalancedOpenTerminalState(session.State) {
		return session, nil
	}

	reason := strings.TrimSpace(msg.Reason)
	if reason == "" {
		reason = "peer canceled"
	}
	return s.transitionSession(ctx, session.SessionID, balancedOpenStateCanceled, reason, "session_canceled_remote", map[string]any{
		"from_pubkey":  sender,
		"reason":       reason,
		"sent_at_unix": msg.SentAtUnix,
	})
}

func (s *BalancedOpenService) transitionSession(ctx context.Context, sessionID string, nextState string, lastError string, eventType string, detail any) (BalancedOpenSession, error) {
	return s.transitionSessionWithMetadata(ctx, sessionID, nextState, lastError, eventType, detail, nil)
}

func (s *BalancedOpenService) transitionSessionWithMetadata(ctx context.Context, sessionID string, nextState string, lastError string, eventType string, detail any, metadataPatch any) (BalancedOpenSession, error) {
	if !isBalancedOpenState(nextState) {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidState
	}

	current, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	if isBalancedOpenTerminalState(current.State) {
		return BalancedOpenSession{}, ErrBalancedOpenTerminalState
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return BalancedOpenSession{}, err
	}
	defer tx.Rollback(ctx)

	rawMeta, err := marshalBalancedOpenJSON(metadataPatch)
	if err != nil {
		return BalancedOpenSession{}, err
	}

	row := tx.QueryRow(ctx, `
update balanced_open_sessions
set
  state = $2,
  state_updated_at = now(),
  updated_at = now(),
  last_error = $3,
  metadata = coalesce(metadata, '{}'::jsonb) || $4::jsonb
where session_id = $1
returning
  id,
  session_id,
  role,
  peer_pubkey,
  peer_host,
  capacity_sat,
  fee_rate_sat_vb,
  is_private,
  close_address,
  state,
  state_updated_at,
  attempts,
  next_retry_at,
  last_error,
  metadata,
  created_at,
  updated_at
`, strings.TrimSpace(sessionID), nextState, strings.TrimSpace(lastError), rawMeta)
	session, err := scanBalancedOpenSession(row)
	if err != nil {
		return BalancedOpenSession{}, err
	}

	if strings.TrimSpace(eventType) != "" {
		if err := s.appendEventTx(ctx, tx, session.SessionID, eventType, detail); err != nil {
			return BalancedOpenSession{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return BalancedOpenSession{}, err
	}
	return session, nil
}

func (s *BalancedOpenService) listSessionsByStates(ctx context.Context, states []string) ([]BalancedOpenSession, error) {
	if len(states) == 0 {
		return []BalancedOpenSession{}, nil
	}

	rows, err := s.db.Query(ctx, `
select
  id,
  session_id,
  role,
  peer_pubkey,
  peer_host,
  capacity_sat,
  fee_rate_sat_vb,
  is_private,
  close_address,
  state,
  state_updated_at,
  attempts,
  next_retry_at,
  last_error,
  metadata,
  created_at,
  updated_at
from balanced_open_sessions
where state = any($1::text[])
order by updated_at asc
`, states)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]BalancedOpenSession, 0)
	for rows.Next() {
		session, err := scanBalancedOpenSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func balancedOpenSessionChannelPoint(metadata json.RawMessage) string {
	if len(metadata) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(metadata, &payload); err != nil {
		return ""
	}
	raw, ok := payload["channel_point"]
	if !ok {
		return ""
	}
	value, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func matchBalancedOpenPendingChannel(session BalancedOpenSession, channelPointHint string, pending []lndclient.PendingChannelInfo) (lndclient.PendingChannelInfo, bool) {
	wantedPeer := strings.ToLower(strings.TrimSpace(session.PeerPubkey))
	wantedPoint := strings.TrimSpace(channelPointHint)

	for _, ch := range pending {
		if !strings.EqualFold(strings.TrimSpace(ch.Status), "opening") {
			continue
		}
		if wantedPoint != "" && strings.EqualFold(strings.TrimSpace(ch.ChannelPoint), wantedPoint) {
			return ch, true
		}
		if wantedPeer != "" && strings.EqualFold(strings.TrimSpace(ch.RemotePubkey), wantedPeer) && ch.CapacitySat == session.CapacitySat {
			return ch, true
		}
	}

	return lndclient.PendingChannelInfo{}, false
}

func matchBalancedOpenActiveChannel(session BalancedOpenSession, channelPointHint string, active []lndclient.ChannelInfo) (lndclient.ChannelInfo, bool) {
	wantedPeer := strings.ToLower(strings.TrimSpace(session.PeerPubkey))
	wantedPoint := strings.TrimSpace(channelPointHint)

	for _, ch := range active {
		if wantedPoint != "" && strings.EqualFold(strings.TrimSpace(ch.ChannelPoint), wantedPoint) {
			return ch, true
		}
		if wantedPeer != "" && strings.EqualFold(strings.TrimSpace(ch.RemotePubkey), wantedPeer) && ch.CapacitySat == session.CapacitySat {
			return ch, true
		}
	}

	return lndclient.ChannelInfo{}, false
}

func (s *BalancedOpenService) sendProtocolMessage(ctx context.Context, peerPubkey string, msg balancedOpenProtocolMessage) error {
	raw, err := marshalBalancedOpenJSON(msg)
	if err != nil {
		return err
	}
	return s.lnd.SendCustomMessage(ctx, peerPubkey, balancedOpenCustomMsgType, raw)
}

func (s *BalancedOpenService) connectPeerForBalancedOpen(ctx context.Context, pubkey string, host string) error {
	err := s.lnd.ConnectPeerWithTimeout(ctx, pubkey, host, false, 8)
	if err == nil {
		return nil
	}

	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(msg, "already connected") || strings.Contains(msg, "already have a connection") {
		return nil
	}
	return err
}

func (s *BalancedOpenService) appendEventTx(ctx context.Context, tx pgx.Tx, sessionID string, eventType string, detail any) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(eventType) == "" {
		return errors.New("event requires session id and type")
	}
	raw, err := marshalBalancedOpenJSON(detail)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
insert into balanced_open_events (session_id, event_type, detail)
values ($1, $2, $3::jsonb)
`, strings.TrimSpace(sessionID), strings.TrimSpace(eventType), raw)
	return err
}

func marshalBalancedOpenJSON(v any) ([]byte, error) {
	if v == nil {
		return []byte(`{}`), nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return []byte(`{}`), nil
	}
	return raw, nil
}

func scanBalancedOpenSession(row balancedOpenScanner) (BalancedOpenSession, error) {
	var session BalancedOpenSession
	var nextRetry sql.NullTime
	var metadata []byte

	if err := row.Scan(
		&session.ID,
		&session.SessionID,
		&session.Role,
		&session.PeerPubkey,
		&session.PeerHost,
		&session.CapacitySat,
		&session.FeeRateSatVb,
		&session.Private,
		&session.CloseAddress,
		&session.State,
		&session.StateUpdatedAt,
		&session.Attempts,
		&nextRetry,
		&session.LastError,
		&metadata,
		&session.CreatedAt,
		&session.UpdatedAt,
	); err != nil {
		return BalancedOpenSession{}, err
	}

	if nextRetry.Valid {
		ts := nextRetry.Time
		session.NextRetryAt = &ts
	}
	if len(metadata) == 0 {
		session.Metadata = json.RawMessage(`{}`)
	} else {
		session.Metadata = json.RawMessage(metadata)
	}

	return session, nil
}

func scanBalancedOpenEvent(row balancedOpenEventScanner) (BalancedOpenEvent, error) {
	var event BalancedOpenEvent
	var detail []byte

	if err := row.Scan(
		&event.ID,
		&event.SessionID,
		&event.EventType,
		&detail,
		&event.CreatedAt,
	); err != nil {
		return BalancedOpenEvent{}, err
	}

	if len(detail) == 0 {
		event.Detail = json.RawMessage(`{}`)
	} else {
		event.Detail = json.RawMessage(detail)
	}

	return event, nil
}

func newBalancedOpenSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func isBalancedOpenRole(role string) bool {
	switch strings.TrimSpace(role) {
	case balancedOpenRoleInitiator, balancedOpenRoleAccepter:
		return true
	default:
		return false
	}
}

func isBalancedOpenState(state string) bool {
	switch strings.TrimSpace(state) {
	case balancedOpenStateSessionCreated:
		return true
	case balancedOpenStateProposalSent:
		return true
	case balancedOpenStateProposalReceived:
		return true
	case balancedOpenStateAccepted:
		return true
	case balancedOpenStateFundingTxHalfSigned:
		return true
	case balancedOpenStateFundingTxFullySigned:
		return true
	case balancedOpenStateChannelProposed:
		return true
	case balancedOpenStateFundingBroadcasted:
		return true
	case balancedOpenStatePendingOpenDetected:
		return true
	case balancedOpenStateActive:
		return true
	case balancedOpenStateFailed:
		return true
	case balancedOpenStateCanceled:
		return true
	case balancedOpenStateRecoveryRequired:
		return true
	case balancedOpenStateRecovered:
		return true
	default:
		return false
	}
}

func isBalancedOpenTerminalState(state string) bool {
	switch strings.TrimSpace(state) {
	case balancedOpenStateActive:
		return true
	case balancedOpenStateFailed:
		return true
	case balancedOpenStateCanceled:
		return true
	case balancedOpenStateRecovered:
		return true
	default:
		return false
	}
}

func isProposalEligibleState(state string) bool {
	switch strings.TrimSpace(state) {
	case balancedOpenStateSessionCreated:
		return true
	case balancedOpenStateProposalSent:
		return true
	case balancedOpenStateProposalReceived:
		return true
	default:
		return false
	}
}
