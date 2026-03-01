package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"lightningos-light/internal/lndclient"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ripemd160"
)

const (
	balancedOpenDefaultListLimit  = 50
	balancedOpenMaxListLimit      = 500
	balancedOpenDefaultEventList  = 100
	balancedOpenMaxEventList      = 500
	balancedOpenMinCapacitySat    = 20000
	balancedOpenProtocolVersion   = 1
	balancedOpenCustomMsgType     = uint32(42069)
	balancedOpenReconcileEvery    = 20 * time.Second
	balancedOpenExecutionModePush = "push_sat_v1"
	balancedOpenExecutionModeDual = "dual_funded_v1"
	balancedOpenMultiSigKeyFamily = int32(0)
	balancedOpenTransitKeyFamily  = int32(805)
	balancedOpenFundingVBytes     = int64(190)
	balancedOpenAnchorSafetySat   = int64(10000)
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
	ErrBalancedOpenDBUnavailable             = errors.New("balanced open db unavailable")
	ErrBalancedOpenSessionNotFound           = errors.New("balanced open session not found")
	ErrBalancedOpenInvalidPeerKey            = errors.New("invalid peer pubkey")
	ErrBalancedOpenInvalidCapacity           = errors.New("invalid capacity")
	ErrBalancedOpenInvalidFeeRate            = errors.New("invalid fee rate")
	ErrBalancedOpenInvalidRole               = errors.New("invalid role")
	ErrBalancedOpenInvalidState              = errors.New("invalid state")
	ErrBalancedOpenTerminalState             = errors.New("session already in terminal state")
	ErrBalancedOpenInvalidSession            = errors.New("invalid session")
	ErrBalancedOpenInvalidAction             = errors.New("invalid session action")
	ErrBalancedOpenInsufficientOnchainSafety = errors.New("insufficient on-chain spendable balance for anchor reserve")
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
	Version           int      `json:"v"`
	Kind              string   `json:"kind"`
	SessionID         string   `json:"session_id"`
	FromPubkey        string   `json:"from_pubkey,omitempty"`
	ToPubkey          string   `json:"to_pubkey,omitempty"`
	PeerHost          string   `json:"peer_host,omitempty"`
	CapacitySat       int64    `json:"capacity_sat,omitempty"`
	FeeRateSatVb      int64    `json:"fee_rate_sat_vb,omitempty"`
	Private           bool     `json:"private,omitempty"`
	CloseAddress      string   `json:"close_address,omitempty"`
	Reason            string   `json:"reason,omitempty"`
	ExecutionMode     string   `json:"execution_mode,omitempty"`
	PendingChanIDHex  string   `json:"pending_chan_id,omitempty"`
	MultisigPubkey    string   `json:"multisig_pubkey,omitempty"`
	TransitTxID       string   `json:"transit_tx_id,omitempty"`
	TransitTxVout     uint32   `json:"transit_tx_vout,omitempty"`
	TransitOutputSat  int64    `json:"transit_output_sat,omitempty"`
	TransitOutputPk   string   `json:"transit_output_script,omitempty"`
	TransitInputStack []string `json:"transit_input_witness,omitempty"`
	SentAtUnix        int64    `json:"sent_at_unix"`
}

type balancedOpenKeyDescriptor struct {
	PublicKey string `json:"public_key"`
	Family    int32  `json:"family"`
	Index     int32  `json:"index"`
}

type balancedOpenTransitDetails struct {
	TxID         string                    `json:"tx_id"`
	Vout         uint32                    `json:"vout"`
	OutputSat    int64                     `json:"output_sat"`
	OutputScript string                    `json:"output_script"`
	Key          balancedOpenKeyDescriptor `json:"key,omitempty"`
}

type balancedOpenMetadata struct {
	ExecutionMode          string                     `json:"execution_mode,omitempty"`
	PendingChanID          string                     `json:"pending_chan_id,omitempty"`
	InitiatorMultisigKey   balancedOpenKeyDescriptor  `json:"initiator_multisig_key,omitempty"`
	AccepterMultisigKey    balancedOpenKeyDescriptor  `json:"accepter_multisig_key,omitempty"`
	InitiatorTransit       balancedOpenTransitDetails `json:"initiator_transit,omitempty"`
	AccepterTransit        balancedOpenTransitDetails `json:"accepter_transit,omitempty"`
	AccepterInputWitness   []string                   `json:"accepter_input_witness,omitempty"`
	FundingTxHex           string                     `json:"funding_tx_hex,omitempty"`
	FundingTxID            string                     `json:"funding_tx_id,omitempty"`
	FundingTxVout          uint32                     `json:"funding_tx_vout,omitempty"`
	ChannelPoint           string                     `json:"channel_point,omitempty"`
	LastExecutionErr       string                     `json:"last_execution_err,omitempty"`
	LastExecutionErrorUnix int64                      `json:"last_execution_error_unix,omitempty"`
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

	meta, err := decodeBalancedOpenMetadata(session.Metadata)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	switch normalizeBalancedExecutionMode(meta.ExecutionMode) {
	case balancedOpenExecutionModeDual:
		return s.executeSessionDual(ctx, session, meta)
	default:
		return s.executeSessionPush(ctx, session)
	}
}

func (s *BalancedOpenService) executeSessionPush(ctx context.Context, session BalancedOpenSession) (BalancedOpenSession, error) {
	pushSat := session.CapacitySat / 2
	if pushSat <= 0 {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidCapacity
	}

	submitted, err := s.transitionSession(ctx, session.SessionID, balancedOpenStateChannelProposed, "", "channel_open_submitted", map[string]any{
		"execution_mode":    balancedOpenExecutionModePush,
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
				"execution_mode": balancedOpenExecutionModePush,
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
			"execution_mode": balancedOpenExecutionModePush,
			"capacity_sat":   submitted.CapacitySat,
			"push_sat":       pushSat,
		})
		return BalancedOpenSession{}, err
	}

	updated, err := s.transitionSessionWithMetadata(ctx, submitted.SessionID, balancedOpenStateFundingBroadcasted, "", "funding_broadcasted_local", map[string]any{
		"execution_mode": balancedOpenExecutionModePush,
		"channel_point":  channelPoint,
		"capacity_sat":   submitted.CapacitySat,
		"push_sat":       pushSat,
	}, map[string]any{
		"channel_point":      channelPoint,
		"execution_mode":     balancedOpenExecutionModePush,
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

func (s *BalancedOpenService) executeSessionDual(ctx context.Context, session BalancedOpenSession, meta balancedOpenMetadata) (BalancedOpenSession, error) {
	if err := validateBalancedDualExecuteArtifacts(session, meta); err != nil {
		return BalancedOpenSession{}, err
	}
	budget, err := s.ensureBalancedOnchainBudget(ctx, 0, balancedOpenAnchorSafetySat)
	if err != nil {
		_, _ = s.transitionSessionWithMetadata(ctx, session.SessionID, balancedOpenStateRecoveryRequired, err.Error(), "anchor_reserve_precheck_failed", map[string]any{
			"execution_mode":          balancedOpenExecutionModeDual,
			"estimated_spendable_sat": budget.EstimatedSpendableSat,
			"total_sat":               budget.TotalSat,
			"locked_sat":              budget.LockedSat,
			"reserved_anchor_sat":     budget.ReservedAnchorSat,
			"required_remaining_sat":  balancedOpenAnchorSafetySat,
		}, map[string]any{
			"last_execution_err":        err.Error(),
			"last_execution_error_unix": time.Now().UTC().Unix(),
		})
		return BalancedOpenSession{}, err
	}

	plan, err := buildBalancedDualFundingPlan(session, meta)
	if err != nil {
		return BalancedOpenSession{}, err
	}

	localFundingSat := session.CapacitySat / 2
	pushSat := int64(0)
	submitted, err := s.transitionSessionWithMetadata(ctx, session.SessionID, balancedOpenStateChannelProposed, "", "channel_open_submitted", map[string]any{
		"execution_mode":            balancedOpenExecutionModeDual,
		"capacity_sat":              session.CapacitySat,
		"local_funding_sat":         localFundingSat,
		"local_onchain_funding_sat": meta.InitiatorTransit.OutputSat,
		"peer_onchain_funding_sat":  meta.AccepterTransit.OutputSat,
		"push_sat":                  pushSat,
	}, map[string]any{
		"execution_mode":  balancedOpenExecutionModeDual,
		"pending_chan_id": meta.PendingChanID,
		"funding_tx_id":   plan.FundingTxID,
		"funding_tx_vout": plan.FundingVout,
		"funding_tx_hex":  plan.TxHex,
	})
	if err != nil {
		return BalancedOpenSession{}, err
	}

	if strings.TrimSpace(submitted.PeerHost) != "" {
		if err := s.connectPeerForBalancedOpen(ctx, submitted.PeerPubkey, submitted.PeerHost); err != nil {
			_, _ = s.transitionSessionWithMetadata(ctx, submitted.SessionID, balancedOpenStateRecoveryRequired, err.Error(), "channel_open_connect_failed", map[string]any{
				"execution_mode": balancedOpenExecutionModeDual,
			}, map[string]any{
				"last_execution_err":        err.Error(),
				"last_execution_error_unix": time.Now().UTC().Unix(),
			})
			return BalancedOpenSession{}, err
		}
	}

	localInputScript, err := s.lnd.ComputeInputScript(ctx, lndclient.ComputeInputScriptParams{
		RawTxHex:        plan.TxHex,
		InputIndex:      uint32(plan.InitiatorInputIndex),
		OutputScriptHex: meta.InitiatorTransit.OutputScript,
		OutputSat:       meta.InitiatorTransit.OutputSat,
		Key: lndclient.DerivedKey{
			PublicKey: meta.InitiatorTransit.Key.PublicKey,
			Family:    meta.InitiatorTransit.Key.Family,
			Index:     meta.InitiatorTransit.Key.Index,
		},
	})
	if err != nil {
		_, _ = s.transitionSessionWithMetadata(ctx, submitted.SessionID, balancedOpenStateRecoveryRequired, err.Error(), "funding_sign_failed", map[string]any{
			"execution_mode": balancedOpenExecutionModeDual,
			"side":           balancedOpenRoleInitiator,
		}, map[string]any{
			"last_execution_err":        err.Error(),
			"last_execution_error_unix": time.Now().UTC().Unix(),
		})
		return BalancedOpenSession{}, err
	}

	remoteWitness, err := balancedDecodeWitnessStack(meta.AccepterInputWitness)
	if err != nil {
		return BalancedOpenSession{}, err
	}

	plan.Tx.TxIn[plan.InitiatorInputIndex].Witness = cloneBalancedWitness(localInputScript.Witness)
	plan.Tx.TxIn[plan.InitiatorInputIndex].SignatureScript = append([]byte(nil), localInputScript.SigScript...)
	plan.Tx.TxIn[plan.AccepterInputIndex].Witness = cloneBalancedWitness(remoteWitness)

	finalTxHex, err := encodeBalancedTxHex(plan.Tx)
	if err != nil {
		return BalancedOpenSession{}, err
	}

	pendingID, err := hex.DecodeString(meta.PendingChanID)
	if err != nil {
		return BalancedOpenSession{}, errors.New("invalid pending channel id")
	}

	if err := s.lnd.RegisterChanPointShim(ctx, lndclient.ChanPointShimParams{
		CapacitySat:   session.CapacitySat,
		PendingChanID: pendingID,
		FundingTxID:   plan.FundingTxID,
		FundingVout:   plan.FundingVout,
		LocalKey: lndclient.DerivedKey{
			PublicKey: meta.InitiatorMultisigKey.PublicKey,
			Family:    meta.InitiatorMultisigKey.Family,
			Index:     meta.InitiatorMultisigKey.Index,
		},
		RemoteKeyHex: meta.AccepterMultisigKey.PublicKey,
	}); err != nil {
		_, _ = s.transitionSessionWithMetadata(ctx, submitted.SessionID, balancedOpenStateRecoveryRequired, err.Error(), "shim_register_failed", map[string]any{
			"execution_mode": balancedOpenExecutionModeDual,
		}, map[string]any{
			"last_execution_err":        err.Error(),
			"last_execution_error_unix": time.Now().UTC().Unix(),
		})
		return BalancedOpenSession{}, err
	}

	channelPoint, err := s.lnd.OpenChannelWithShim(ctx, lndclient.OpenChannelWithShimParams{
		PubkeyHex:       submitted.PeerPubkey,
		CapacitySat:     submitted.CapacitySat,
		LocalFundingSat: localFundingSat,
		PushSat:         pushSat,
		CloseAddress:    submitted.CloseAddress,
		Private:         submitted.Private,
		ChanPointShimArgs: lndclient.ChanPointShimParams{
			CapacitySat:   submitted.CapacitySat,
			PendingChanID: pendingID,
			FundingTxID:   plan.FundingTxID,
			FundingVout:   plan.FundingVout,
			LocalKey: lndclient.DerivedKey{
				PublicKey: meta.InitiatorMultisigKey.PublicKey,
				Family:    meta.InitiatorMultisigKey.Family,
				Index:     meta.InitiatorMultisigKey.Index,
			},
			RemoteKeyHex: meta.AccepterMultisigKey.PublicKey,
		},
	})
	if err != nil {
		_, _ = s.transitionSessionWithMetadata(ctx, submitted.SessionID, balancedOpenStateRecoveryRequired, err.Error(), "channel_open_failed", map[string]any{
			"execution_mode": balancedOpenExecutionModeDual,
		}, map[string]any{
			"last_execution_err":        err.Error(),
			"last_execution_error_unix": time.Now().UTC().Unix(),
		})
		return BalancedOpenSession{}, err
	}

	if err := s.lnd.PublishTransaction(ctx, finalTxHex, fmt.Sprintf("balanced-open-%s", submitted.SessionID)); err != nil {
		errText := strings.ToLower(strings.TrimSpace(err.Error()))
		alreadyPublished := strings.Contains(errText, "already have transaction") ||
			strings.Contains(errText, "transaction already in block chain") ||
			strings.Contains(errText, "already exists")
		if !alreadyPublished {
			_, _ = s.transitionSessionWithMetadata(ctx, submitted.SessionID, balancedOpenStateRecoveryRequired, err.Error(), "funding_broadcast_failed", map[string]any{
				"execution_mode": balancedOpenExecutionModeDual,
				"funding_tx_id":  plan.FundingTxID,
			}, map[string]any{
				"last_execution_err":        err.Error(),
				"last_execution_error_unix": time.Now().UTC().Unix(),
				"funding_tx_hex":            finalTxHex,
			})
			return BalancedOpenSession{}, err
		}
	}

	updated, err := s.transitionSessionWithMetadata(ctx, submitted.SessionID, balancedOpenStateFundingBroadcasted, "", "funding_broadcasted_local", map[string]any{
		"execution_mode":            balancedOpenExecutionModeDual,
		"channel_point":             channelPoint,
		"capacity_sat":              submitted.CapacitySat,
		"push_sat":                  pushSat,
		"funding_tx_id":             plan.FundingTxID,
		"funding_tx_vout":           plan.FundingVout,
		"local_onchain_funding_sat": meta.InitiatorTransit.OutputSat,
		"peer_onchain_funding_sat":  meta.AccepterTransit.OutputSat,
	}, map[string]any{
		"channel_point":             channelPoint,
		"execution_mode":            balancedOpenExecutionModeDual,
		"funding_tx_id":             plan.FundingTxID,
		"funding_tx_vout":           plan.FundingVout,
		"funding_tx_hex":            finalTxHex,
		"local_funding_sat":         localFundingSat,
		"push_sat":                  pushSat,
		"opened_at_unix":            time.Now().UTC().Unix(),
		"last_execution_err":        "",
		"last_execution_error_unix": int64(0),
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

	meta, err := decodeBalancedOpenMetadata(session.Metadata)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	mode := normalizeBalancedExecutionMode(meta.ExecutionMode)
	if mode == "" {
		mode = balancedOpenExecutionModeDual
	}

	if mode == balancedOpenExecutionModeDual {
		nextMeta, created, err := s.ensureInitiatorDualArtifacts(ctx, session, meta)
		if err != nil {
			return BalancedOpenSession{}, err
		}
		meta = nextMeta
		if created {
			patched, err := s.transitionSessionWithMetadata(ctx, session.SessionID, session.State, "", "funding_artifacts_prepared", map[string]any{
				"execution_mode": balancedOpenExecutionModeDual,
			}, balancedEncodeMetadata(meta))
			if err != nil {
				return BalancedOpenSession{}, err
			}
			session = patched
		}
	}

	msg := balancedOpenProtocolMessage{
		Version:       balancedOpenProtocolVersion,
		Kind:          balancedOpenMessageKindProposal,
		SessionID:     session.SessionID,
		ToPubkey:      session.PeerPubkey,
		PeerHost:      session.PeerHost,
		CapacitySat:   session.CapacitySat,
		FeeRateSatVb:  session.FeeRateSatVb,
		Private:       session.Private,
		CloseAddress:  session.CloseAddress,
		ExecutionMode: mode,
		SentAtUnix:    time.Now().UTC().Unix(),
	}
	if mode == balancedOpenExecutionModeDual {
		msg.PendingChanIDHex = meta.PendingChanID
		msg.MultisigPubkey = meta.InitiatorMultisigKey.PublicKey
		msg.TransitTxID = meta.InitiatorTransit.TxID
		msg.TransitTxVout = meta.InitiatorTransit.Vout
		msg.TransitOutputSat = meta.InitiatorTransit.OutputSat
		msg.TransitOutputPk = meta.InitiatorTransit.OutputScript
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

	updated, err := s.transitionSessionWithMetadata(ctx, session.SessionID, balancedOpenStateProposalSent, "", "proposal_sent", map[string]any{
		"kind":           msg.Kind,
		"execution_mode": mode,
		"sent_at_unix":   msg.SentAtUnix,
	}, balancedEncodeMetadata(meta))
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
	if session.State != balancedOpenStateProposalReceived && session.State != balancedOpenStateFundingTxHalfSigned {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidAction
	}

	meta, err := decodeBalancedOpenMetadata(session.Metadata)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	mode := normalizeBalancedExecutionMode(meta.ExecutionMode)
	if mode == "" {
		mode = balancedOpenExecutionModePush
	}

	var localWitness []string
	if mode == balancedOpenExecutionModeDual {
		nextMeta, witness, created, err := s.ensureAccepterDualArtifacts(ctx, session, meta)
		meta = nextMeta
		if created {
			patched, err := s.transitionSessionWithMetadata(ctx, session.SessionID, session.State, "", "funding_artifacts_prepared", map[string]any{
				"execution_mode": mode,
				"side":           balancedOpenRoleAccepter,
			}, balancedEncodeMetadata(meta))
			if err != nil {
				return BalancedOpenSession{}, err
			}
			session = patched
		}
		if err != nil {
			return BalancedOpenSession{}, err
		}
		localWitness = witness

		patched, err := s.transitionSessionWithMetadata(ctx, session.SessionID, balancedOpenStateFundingTxHalfSigned, "", "funding_tx_half_signed", map[string]any{
			"execution_mode": mode,
			"side":           balancedOpenRoleAccepter,
		}, balancedEncodeMetadata(meta))
		if err != nil {
			return BalancedOpenSession{}, err
		}
		session = patched
	}

	msg := balancedOpenProtocolMessage{
		Version:       balancedOpenProtocolVersion,
		Kind:          balancedOpenMessageKindAccept,
		SessionID:     session.SessionID,
		ToPubkey:      session.PeerPubkey,
		ExecutionMode: mode,
		SentAtUnix:    time.Now().UTC().Unix(),
	}
	if mode == balancedOpenExecutionModeDual {
		msg.PendingChanIDHex = meta.PendingChanID
		msg.MultisigPubkey = meta.AccepterMultisigKey.PublicKey
		msg.TransitTxID = meta.AccepterTransit.TxID
		msg.TransitTxVout = meta.AccepterTransit.Vout
		msg.TransitOutputSat = meta.AccepterTransit.OutputSat
		msg.TransitOutputPk = meta.AccepterTransit.OutputScript
		msg.TransitInputStack = append([]string(nil), localWitness...)
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

	updated, err := s.transitionSessionWithMetadata(ctx, session.SessionID, balancedOpenStateAccepted, "", "proposal_accepted_local", map[string]any{
		"kind":           msg.Kind,
		"execution_mode": mode,
		"sent_at_unix":   msg.SentAtUnix,
	}, balancedEncodeMetadata(meta))
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
		meta = json.RawMessage(fmt.Sprintf(`{"execution_mode":"%s"}`, balancedOpenExecutionModeDual))
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

func (s *BalancedOpenService) RecoverSessionTransit(ctx context.Context, sessionID string, satPerVbyte int64) (BalancedOpenSession, error) {
	if s == nil || s.db == nil {
		return BalancedOpenSession{}, ErrBalancedOpenDBUnavailable
	}
	if satPerVbyte < 0 {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidFeeRate
	}

	session, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	if session.State == balancedOpenStateRecovered {
		return BalancedOpenSession{}, ErrBalancedOpenTerminalState
	}
	if session.State == balancedOpenStateActive {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidAction
	}
	if session.State != balancedOpenStateRecoveryRequired && session.State != balancedOpenStateCanceled {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidAction
	}

	meta, err := decodeBalancedOpenMetadata(session.Metadata)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	if normalizeBalancedExecutionMode(meta.ExecutionMode) != balancedOpenExecutionModeDual {
		return BalancedOpenSession{}, ErrBalancedOpenInvalidAction
	}

	localTransit, ok := balancedOpenLocalTransitForRole(session.Role, meta)
	if !ok {
		return BalancedOpenSession{}, errors.New("missing local transit details")
	}

	pending, err := s.lnd.ListPendingChannels(ctx)
	if err == nil {
		if pendingCh, found := matchBalancedOpenPendingChannel(session, balancedOpenSessionChannelPoint(session.Metadata), pending); found {
			if strings.EqualFold(strings.TrimSpace(pendingCh.Status), "opening") {
				return BalancedOpenSession{}, errors.New("cannot recover transit while channel is pending open")
			}
		}
	}
	active, err := s.lnd.ListChannels(ctx)
	if err == nil {
		if _, found := matchBalancedOpenActiveChannel(session, balancedOpenSessionChannelPoint(session.Metadata), active); found {
			return BalancedOpenSession{}, errors.New("cannot recover transit while channel is active")
		}
	}

	txid, address, err := s.lnd.SweepOutpoint(ctx, localTransit.TxID, localTransit.Vout, satPerVbyte)
	if err != nil {
		return BalancedOpenSession{}, err
	}

	patch := map[string]any{
		"last_execution_err":        "",
		"last_execution_error_unix": int64(0),
	}
	if session.Role == balancedOpenRoleInitiator {
		patch["initiator_recovery_txid"] = txid
		patch["initiator_recovery_address"] = address
	} else {
		patch["accepter_recovery_txid"] = txid
		patch["accepter_recovery_address"] = address
	}

	updated, err := s.transitionSessionWithMetadata(ctx, session.SessionID, balancedOpenStateRecovered, "", "transit_recovered", map[string]any{
		"role":             session.Role,
		"transit_tx_id":    localTransit.TxID,
		"transit_tx_vout":  localTransit.Vout,
		"recovery_txid":    txid,
		"recovery_address": address,
		"sat_per_vbyte":    satPerVbyte,
	}, patch)
	if err != nil {
		return BalancedOpenSession{}, err
	}
	return updated, nil
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
	mode := normalizeBalancedExecutionMode(msg.ExecutionMode)
	if mode == "" {
		mode = balancedOpenExecutionModePush
	}
	if mode == balancedOpenExecutionModeDual {
		if !isValidPubkeyHex(msg.MultisigPubkey) {
			return BalancedOpenSession{}, errors.New("proposal missing multisig pubkey")
		}
		if !isBalancedOpenTxID(msg.TransitTxID) {
			return BalancedOpenSession{}, errors.New("proposal missing transit tx id")
		}
		if msg.TransitOutputSat <= 0 {
			return BalancedOpenSession{}, errors.New("proposal missing transit output amount")
		}
		if !isBalancedOpenScriptHex(msg.TransitOutputPk) {
			return BalancedOpenSession{}, errors.New("proposal missing transit output script")
		}
		if !isBalancedPendingChanIDHex(msg.PendingChanIDHex) {
			return BalancedOpenSession{}, errors.New("proposal missing pending channel id")
		}
	}

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return BalancedOpenSession{}, err
	}
	defer tx.Rollback(ctx)

	metaPatch := map[string]any{
		"execution_mode":   mode,
		"protocol_version": msg.Version,
		"last_message": map[string]any{
			"kind":         msg.Kind,
			"from_pubkey":  sender,
			"to_pubkey":    msg.ToPubkey,
			"sent_at_unix": msg.SentAtUnix,
		},
	}
	if mode == balancedOpenExecutionModeDual {
		metaPatch["pending_chan_id"] = strings.ToLower(strings.TrimSpace(msg.PendingChanIDHex))
		metaPatch["initiator_multisig_key"] = map[string]any{
			"public_key": strings.ToLower(strings.TrimSpace(msg.MultisigPubkey)),
			"family":     balancedOpenMultiSigKeyFamily,
			"index":      0,
		}
		metaPatch["initiator_transit"] = map[string]any{
			"tx_id":         strings.ToLower(strings.TrimSpace(msg.TransitTxID)),
			"vout":          msg.TransitTxVout,
			"output_sat":    msg.TransitOutputSat,
			"output_script": strings.ToLower(strings.TrimSpace(msg.TransitOutputPk)),
		}
	}

	meta, err := marshalBalancedOpenJSON(metaPatch)
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
		"from_pubkey":    sender,
		"capacity_sat":   msg.CapacitySat,
		"fee_rate":       msg.FeeRateSatVb,
		"private":        msg.Private,
		"execution_mode": mode,
		"sent_at_unix":   msg.SentAtUnix,
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

	meta, err := decodeBalancedOpenMetadata(session.Metadata)
	if err != nil {
		return BalancedOpenSession{}, err
	}

	mode := normalizeBalancedExecutionMode(msg.ExecutionMode)
	if mode == "" {
		mode = normalizeBalancedExecutionMode(meta.ExecutionMode)
	}
	if mode == "" {
		mode = balancedOpenExecutionModePush
	}

	if mode == balancedOpenExecutionModeDual {
		if !isValidPubkeyHex(msg.MultisigPubkey) {
			return BalancedOpenSession{}, errors.New("accept missing multisig pubkey")
		}
		if !isBalancedOpenTxID(msg.TransitTxID) {
			return BalancedOpenSession{}, errors.New("accept missing transit tx id")
		}
		if msg.TransitOutputSat <= 0 {
			return BalancedOpenSession{}, errors.New("accept missing transit output amount")
		}
		if !isBalancedOpenScriptHex(msg.TransitOutputPk) {
			return BalancedOpenSession{}, errors.New("accept missing transit output script")
		}
		if len(msg.TransitInputStack) == 0 {
			return BalancedOpenSession{}, errors.New("accept missing funding input witness")
		}
		if !isBalancedPendingChanIDHex(msg.PendingChanIDHex) {
			return BalancedOpenSession{}, errors.New("accept missing pending channel id")
		}

		pendingID := strings.ToLower(strings.TrimSpace(msg.PendingChanIDHex))
		if existing := strings.ToLower(strings.TrimSpace(meta.PendingChanID)); existing != "" && existing != pendingID {
			return BalancedOpenSession{}, errors.New("accept pending channel id mismatch")
		}

		meta.ExecutionMode = balancedOpenExecutionModeDual
		meta.PendingChanID = pendingID
		meta.AccepterMultisigKey = balancedOpenKeyDescriptor{
			PublicKey: strings.ToLower(strings.TrimSpace(msg.MultisigPubkey)),
			Family:    balancedOpenMultiSigKeyFamily,
			Index:     0,
		}
		meta.AccepterTransit = balancedOpenTransitDetails{
			TxID:         strings.ToLower(strings.TrimSpace(msg.TransitTxID)),
			Vout:         msg.TransitTxVout,
			OutputSat:    msg.TransitOutputSat,
			OutputScript: strings.ToLower(strings.TrimSpace(msg.TransitOutputPk)),
		}
		meta.AccepterInputWitness = append([]string(nil), msg.TransitInputStack...)
	}

	updated, err := s.transitionSessionWithMetadata(ctx, session.SessionID, balancedOpenStateAccepted, "", "proposal_accepted_remote", map[string]any{
		"from_pubkey":    sender,
		"execution_mode": mode,
		"sent_at_unix":   msg.SentAtUnix,
	}, balancedEncodeMetadata(meta))
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
		if !(current.State == balancedOpenStateCanceled && nextState == balancedOpenStateRecovered) {
			return BalancedOpenSession{}, ErrBalancedOpenTerminalState
		}
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

type balancedDualFundingInput struct {
	Side string
	TxID string
	Vout uint32
}

type balancedDualFundingPlan struct {
	Tx                  *wire.MsgTx
	TxHex               string
	FundingTxID         string
	FundingVout         uint32
	FundingScriptHex    string
	InitiatorInputIndex int
	AccepterInputIndex  int
}

type balancedOnchainBudget struct {
	TotalSat              int64
	LockedSat             int64
	ReservedAnchorSat     int64
	EstimatedSpendableSat int64
}

func normalizeBalancedExecutionMode(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case balancedOpenExecutionModeDual:
		return balancedOpenExecutionModeDual
	case balancedOpenExecutionModePush:
		return balancedOpenExecutionModePush
	default:
		return ""
	}
}

func (s *BalancedOpenService) ensureBalancedOnchainBudget(ctx context.Context, spendingSat int64, minRemainingSat int64) (balancedOnchainBudget, error) {
	details, err := s.lnd.GetWalletBalanceDetails(ctx)
	if err != nil {
		return balancedOnchainBudget{}, err
	}

	budget := balancedOnchainBudget{
		TotalSat:              details.TotalSat,
		LockedSat:             details.LockedSat,
		ReservedAnchorSat:     details.ReservedAnchorSat,
		EstimatedSpendableSat: details.EstimatedSpendableSat,
	}

	remaining := budget.EstimatedSpendableSat - spendingSat
	if remaining < minRemainingSat {
		return budget, fmt.Errorf("%w: spendable=%d sats, spending=%d sats, remaining=%d sats, required_remaining=%d sats", ErrBalancedOpenInsufficientOnchainSafety, budget.EstimatedSpendableSat, spendingSat, remaining, minRemainingSat)
	}

	return budget, nil
}

func decodeBalancedOpenMetadata(raw json.RawMessage) (balancedOpenMetadata, error) {
	if len(raw) == 0 {
		return balancedOpenMetadata{}, nil
	}
	var meta balancedOpenMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return balancedOpenMetadata{}, err
	}
	meta.ExecutionMode = normalizeBalancedExecutionMode(meta.ExecutionMode)
	return meta, nil
}

func balancedEncodeMetadata(meta balancedOpenMetadata) map[string]any {
	out := map[string]any{}
	if mode := normalizeBalancedExecutionMode(meta.ExecutionMode); mode != "" {
		out["execution_mode"] = mode
	}
	if id := strings.ToLower(strings.TrimSpace(meta.PendingChanID)); id != "" {
		out["pending_chan_id"] = id
	}
	if key := encodeBalancedOpenKey(meta.InitiatorMultisigKey); key != nil {
		out["initiator_multisig_key"] = key
	}
	if key := encodeBalancedOpenKey(meta.AccepterMultisigKey); key != nil {
		out["accepter_multisig_key"] = key
	}
	if transit := encodeBalancedTransit(meta.InitiatorTransit); transit != nil {
		out["initiator_transit"] = transit
	}
	if transit := encodeBalancedTransit(meta.AccepterTransit); transit != nil {
		out["accepter_transit"] = transit
	}
	if len(meta.AccepterInputWitness) > 0 {
		out["accepter_input_witness"] = append([]string(nil), meta.AccepterInputWitness...)
	}
	if txHex := strings.ToLower(strings.TrimSpace(meta.FundingTxHex)); txHex != "" {
		out["funding_tx_hex"] = txHex
	}
	if txid := strings.ToLower(strings.TrimSpace(meta.FundingTxID)); txid != "" {
		out["funding_tx_id"] = txid
		out["funding_tx_vout"] = meta.FundingTxVout
	}
	if point := strings.TrimSpace(meta.ChannelPoint); point != "" {
		out["channel_point"] = point
	}
	if errText := strings.TrimSpace(meta.LastExecutionErr); errText != "" {
		out["last_execution_err"] = errText
	}
	if meta.LastExecutionErrorUnix > 0 {
		out["last_execution_error_unix"] = meta.LastExecutionErrorUnix
	}
	return out
}

func encodeBalancedOpenKey(key balancedOpenKeyDescriptor) map[string]any {
	pub := strings.ToLower(strings.TrimSpace(key.PublicKey))
	if !isValidPubkeyHex(pub) {
		return nil
	}
	return map[string]any{
		"public_key": pub,
		"family":     key.Family,
		"index":      key.Index,
	}
}

func encodeBalancedTransit(transit balancedOpenTransitDetails) map[string]any {
	txid := strings.ToLower(strings.TrimSpace(transit.TxID))
	if !isBalancedOpenTxID(txid) {
		return nil
	}
	if transit.OutputSat <= 0 {
		return nil
	}
	script := strings.ToLower(strings.TrimSpace(transit.OutputScript))
	if !isBalancedOpenScriptHex(script) {
		return nil
	}
	out := map[string]any{
		"tx_id":         txid,
		"vout":          transit.Vout,
		"output_sat":    transit.OutputSat,
		"output_script": script,
	}
	if key := encodeBalancedOpenKey(transit.Key); key != nil {
		out["key"] = key
	}
	return out
}

func validateBalancedDualExecuteArtifacts(session BalancedOpenSession, meta balancedOpenMetadata) error {
	if normalizeBalancedExecutionMode(meta.ExecutionMode) != balancedOpenExecutionModeDual {
		return ErrBalancedOpenInvalidSession
	}
	if !isBalancedPendingChanIDHex(meta.PendingChanID) {
		return errors.New("missing pending channel id")
	}
	if !isValidPubkeyHex(meta.InitiatorMultisigKey.PublicKey) || !isValidPubkeyHex(meta.AccepterMultisigKey.PublicKey) {
		return errors.New("missing multisig pubkeys")
	}
	if !hasBalancedTransit(meta.InitiatorTransit, true) || !hasBalancedTransit(meta.AccepterTransit, false) {
		return errors.New("missing transit funding details")
	}
	if len(meta.AccepterInputWitness) == 0 {
		return errors.New("missing accepter funding witness")
	}
	if session.CapacitySat <= 0 || session.CapacitySat%2 != 0 {
		return ErrBalancedOpenInvalidCapacity
	}
	return nil
}

func hasBalancedTransit(transit balancedOpenTransitDetails, requireKey bool) bool {
	if !isBalancedOpenTxID(transit.TxID) {
		return false
	}
	if transit.OutputSat <= 0 {
		return false
	}
	if !isBalancedOpenScriptHex(transit.OutputScript) {
		return false
	}
	if requireKey && !isValidPubkeyHex(transit.Key.PublicKey) {
		return false
	}
	return true
}

func balancedOpenLocalTransitForRole(role string, meta balancedOpenMetadata) (balancedOpenTransitDetails, bool) {
	switch strings.TrimSpace(role) {
	case balancedOpenRoleInitiator:
		if !hasBalancedTransit(meta.InitiatorTransit, true) {
			return balancedOpenTransitDetails{}, false
		}
		return meta.InitiatorTransit, true
	case balancedOpenRoleAccepter:
		if !hasBalancedTransit(meta.AccepterTransit, true) {
			return balancedOpenTransitDetails{}, false
		}
		return meta.AccepterTransit, true
	default:
		return balancedOpenTransitDetails{}, false
	}
}

func (s *BalancedOpenService) ensureInitiatorDualArtifacts(ctx context.Context, session BalancedOpenSession, meta balancedOpenMetadata) (balancedOpenMetadata, bool, error) {
	if normalizeBalancedExecutionMode(meta.ExecutionMode) == balancedOpenExecutionModeDual &&
		isBalancedPendingChanIDHex(meta.PendingChanID) &&
		isValidPubkeyHex(meta.InitiatorMultisigKey.PublicKey) &&
		hasBalancedTransit(meta.InitiatorTransit, true) {
		return meta, false, nil
	}

	feeRate := balancedEffectiveFeeRate(session.FeeRateSatVb)
	transitAmount, err := balancedTransitContribution(session.CapacitySat, feeRate)
	if err != nil {
		return meta, false, err
	}
	if _, err := s.ensureBalancedOnchainBudget(ctx, transitAmount, balancedOpenAnchorSafetySat); err != nil {
		return meta, false, err
	}

	multisigKey, err := s.lnd.DeriveNextKey(ctx, balancedOpenMultiSigKeyFamily)
	if err != nil {
		return meta, false, err
	}
	transitKey, err := s.lnd.DeriveNextKey(ctx, balancedOpenTransitKeyFamily)
	if err != nil {
		return meta, false, err
	}
	transitScript, err := balancedP2WPKHScriptHex(transitKey.PublicKey)
	if err != nil {
		return meta, false, err
	}

	sendResult, err := s.lnd.SendOutputScript(ctx, lndclient.SendOutputScriptParams{
		SatPerVbyte:      feeRate,
		OutputScriptHex:  transitScript,
		AmountSat:        transitAmount,
		Label:            fmt.Sprintf("balanced-open-%s-initiator", session.SessionID),
		MinConfs:         0,
		SpendUnconfirmed: true,
	})
	if err != nil {
		return meta, false, err
	}

	pendingID, err := newBalancedOpenPendingChanIDHex()
	if err != nil {
		return meta, false, err
	}

	meta.ExecutionMode = balancedOpenExecutionModeDual
	meta.PendingChanID = pendingID
	meta.InitiatorMultisigKey = balancedOpenKeyDescriptor{
		PublicKey: strings.ToLower(strings.TrimSpace(multisigKey.PublicKey)),
		Family:    multisigKey.Family,
		Index:     multisigKey.Index,
	}
	meta.InitiatorTransit = balancedOpenTransitDetails{
		TxID:         strings.ToLower(strings.TrimSpace(sendResult.TxID)),
		Vout:         sendResult.Vout,
		OutputSat:    transitAmount,
		OutputScript: strings.ToLower(strings.TrimSpace(transitScript)),
		Key: balancedOpenKeyDescriptor{
			PublicKey: strings.ToLower(strings.TrimSpace(transitKey.PublicKey)),
			Family:    transitKey.Family,
			Index:     transitKey.Index,
		},
	}

	return meta, true, nil
}

func (s *BalancedOpenService) ensureAccepterDualArtifacts(ctx context.Context, session BalancedOpenSession, meta balancedOpenMetadata) (balancedOpenMetadata, []string, bool, error) {
	if normalizeBalancedExecutionMode(meta.ExecutionMode) == balancedOpenExecutionModeDual &&
		isValidPubkeyHex(meta.AccepterMultisigKey.PublicKey) &&
		hasBalancedTransit(meta.AccepterTransit, true) &&
		len(meta.AccepterInputWitness) > 0 {
		return meta, append([]string(nil), meta.AccepterInputWitness...), false, nil
	}

	if !isValidPubkeyHex(meta.InitiatorMultisigKey.PublicKey) || !hasBalancedTransit(meta.InitiatorTransit, false) {
		return meta, nil, false, errors.New("proposal missing initiator artifacts")
	}

	created := false
	if !isValidPubkeyHex(meta.AccepterMultisigKey.PublicKey) || !hasBalancedTransit(meta.AccepterTransit, true) {
		feeRate := balancedEffectiveFeeRate(session.FeeRateSatVb)
		transitAmount, err := balancedTransitContribution(session.CapacitySat, feeRate)
		if err != nil {
			return meta, nil, false, err
		}
		if _, err := s.ensureBalancedOnchainBudget(ctx, transitAmount, balancedOpenAnchorSafetySat); err != nil {
			return meta, nil, false, err
		}

		multisigKey, err := s.lnd.DeriveNextKey(ctx, balancedOpenMultiSigKeyFamily)
		if err != nil {
			return meta, nil, false, err
		}
		transitKey, err := s.lnd.DeriveNextKey(ctx, balancedOpenTransitKeyFamily)
		if err != nil {
			return meta, nil, false, err
		}
		transitScript, err := balancedP2WPKHScriptHex(transitKey.PublicKey)
		if err != nil {
			return meta, nil, false, err
		}

		sendResult, err := s.lnd.SendOutputScript(ctx, lndclient.SendOutputScriptParams{
			SatPerVbyte:      feeRate,
			OutputScriptHex:  transitScript,
			AmountSat:        transitAmount,
			Label:            fmt.Sprintf("balanced-open-%s-accepter", session.SessionID),
			MinConfs:         0,
			SpendUnconfirmed: true,
		})
		if err != nil {
			return meta, nil, false, err
		}

		meta.ExecutionMode = balancedOpenExecutionModeDual
		meta.AccepterMultisigKey = balancedOpenKeyDescriptor{
			PublicKey: strings.ToLower(strings.TrimSpace(multisigKey.PublicKey)),
			Family:    multisigKey.Family,
			Index:     multisigKey.Index,
		}
		meta.AccepterTransit = balancedOpenTransitDetails{
			TxID:         strings.ToLower(strings.TrimSpace(sendResult.TxID)),
			Vout:         sendResult.Vout,
			OutputSat:    transitAmount,
			OutputScript: strings.ToLower(strings.TrimSpace(transitScript)),
			Key: balancedOpenKeyDescriptor{
				PublicKey: strings.ToLower(strings.TrimSpace(transitKey.PublicKey)),
				Family:    transitKey.Family,
				Index:     transitKey.Index,
			},
		}
		created = true
	}

	if !isBalancedPendingChanIDHex(meta.PendingChanID) {
		pendingID, err := newBalancedOpenPendingChanIDHex()
		if err != nil {
			return meta, nil, created, err
		}
		meta.PendingChanID = pendingID
		created = true
	}

	meta.ExecutionMode = balancedOpenExecutionModeDual

	plan, err := buildBalancedDualFundingPlan(session, meta)
	if err != nil {
		return meta, nil, created, err
	}
	meta.FundingTxID = plan.FundingTxID
	meta.FundingTxVout = plan.FundingVout
	meta.FundingTxHex = plan.TxHex

	localInputScript, err := s.lnd.ComputeInputScript(ctx, lndclient.ComputeInputScriptParams{
		RawTxHex:        plan.TxHex,
		InputIndex:      uint32(plan.AccepterInputIndex),
		OutputScriptHex: meta.AccepterTransit.OutputScript,
		OutputSat:       meta.AccepterTransit.OutputSat,
		Key: lndclient.DerivedKey{
			PublicKey: meta.AccepterTransit.Key.PublicKey,
			Family:    meta.AccepterTransit.Key.Family,
			Index:     meta.AccepterTransit.Key.Index,
		},
	})
	if err != nil {
		return meta, nil, created, err
	}

	localWitness := balancedEncodeWitnessStack(localInputScript.Witness)
	if len(localWitness) == 0 {
		return meta, nil, created, errors.New("empty accepter funding witness")
	}
	meta.AccepterInputWitness = append([]string(nil), localWitness...)

	pendingID, err := hex.DecodeString(meta.PendingChanID)
	if err != nil {
		return meta, nil, created, errors.New("invalid pending channel id")
	}

	err = s.lnd.RegisterChanPointShim(ctx, lndclient.ChanPointShimParams{
		CapacitySat:   session.CapacitySat,
		PendingChanID: pendingID,
		FundingTxID:   plan.FundingTxID,
		FundingVout:   plan.FundingVout,
		LocalKey: lndclient.DerivedKey{
			PublicKey: meta.AccepterMultisigKey.PublicKey,
			Family:    meta.AccepterMultisigKey.Family,
			Index:     meta.AccepterMultisigKey.Index,
		},
		RemoteKeyHex: meta.InitiatorMultisigKey.PublicKey,
	})
	if err != nil && !isBalancedOpenAlreadyRegisteredErr(err) {
		return meta, nil, created, err
	}

	return meta, localWitness, created, nil
}

func buildBalancedDualFundingPlan(session BalancedOpenSession, meta balancedOpenMetadata) (balancedDualFundingPlan, error) {
	witnessScript, err := balancedFundingWitnessScript(meta.InitiatorMultisigKey.PublicKey, meta.AccepterMultisigKey.PublicKey)
	if err != nil {
		return balancedDualFundingPlan{}, err
	}
	fundingPkScript := balancedFundingOutputScript(witnessScript)

	inputs := []balancedDualFundingInput{
		{
			Side: balancedOpenRoleInitiator,
			TxID: strings.ToLower(strings.TrimSpace(meta.InitiatorTransit.TxID)),
			Vout: meta.InitiatorTransit.Vout,
		},
		{
			Side: balancedOpenRoleAccepter,
			TxID: strings.ToLower(strings.TrimSpace(meta.AccepterTransit.TxID)),
			Vout: meta.AccepterTransit.Vout,
		},
	}
	for _, input := range inputs {
		if !isBalancedOpenTxID(input.TxID) {
			return balancedDualFundingPlan{}, errors.New("invalid transit tx id")
		}
	}

	sort.SliceStable(inputs, func(i, j int) bool {
		a := balancedTxidSortKey(inputs[i].TxID)
		b := balancedTxidSortKey(inputs[j].TxID)
		if cmp := bytes.Compare(a, b); cmp != 0 {
			return cmp < 0
		}
		return inputs[i].Vout < inputs[j].Vout
	})

	tx := wire.NewMsgTx(2)
	initiatorIndex := -1
	accepterIndex := -1

	for idx, input := range inputs {
		hash, err := chainhash.NewHashFromStr(input.TxID)
		if err != nil {
			return balancedDualFundingPlan{}, err
		}
		txin := wire.NewTxIn(wire.NewOutPoint(hash, input.Vout), nil, nil)
		txin.Sequence = 0
		tx.AddTxIn(txin)
		if input.Side == balancedOpenRoleInitiator {
			initiatorIndex = idx
		} else {
			accepterIndex = idx
		}
	}

	tx.AddTxOut(wire.NewTxOut(session.CapacitySat, fundingPkScript))
	if initiatorIndex < 0 || accepterIndex < 0 {
		return balancedDualFundingPlan{}, errors.New("failed to map funding inputs")
	}

	txHex, err := encodeBalancedTxHex(tx)
	if err != nil {
		return balancedDualFundingPlan{}, err
	}

	return balancedDualFundingPlan{
		Tx:                  tx,
		TxHex:               txHex,
		FundingTxID:         tx.TxHash().String(),
		FundingVout:         0,
		FundingScriptHex:    hex.EncodeToString(fundingPkScript),
		InitiatorInputIndex: initiatorIndex,
		AccepterInputIndex:  accepterIndex,
	}, nil
}

func balancedP2WPKHScriptHex(pubkeyHex string) (string, error) {
	pubkey, err := hex.DecodeString(strings.TrimSpace(pubkeyHex))
	if err != nil || len(pubkey) != 33 {
		return "", errors.New("invalid transit pubkey")
	}
	sum := sha256.Sum256(pubkey)
	r := ripemd160.New()
	_, _ = r.Write(sum[:])
	hash160 := r.Sum(nil)

	script := make([]byte, 0, 22)
	script = append(script, txscript.OP_0, 0x14)
	script = append(script, hash160...)

	return hex.EncodeToString(script), nil
}

func balancedFundingWitnessScript(keyA string, keyB string) ([]byte, error) {
	pubA, err := hex.DecodeString(strings.TrimSpace(keyA))
	if err != nil || len(pubA) != 33 {
		return nil, errors.New("invalid local multisig key")
	}
	pubB, err := hex.DecodeString(strings.TrimSpace(keyB))
	if err != nil || len(pubB) != 33 {
		return nil, errors.New("invalid remote multisig key")
	}

	pubkeys := [][]byte{pubA, pubB}
	sort.SliceStable(pubkeys, func(i, j int) bool {
		return bytes.Compare(pubkeys[i], pubkeys[j]) < 0
	})

	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_2)
	builder.AddData(pubkeys[0])
	builder.AddData(pubkeys[1])
	builder.AddOp(txscript.OP_2)
	builder.AddOp(txscript.OP_CHECKMULTISIG)
	return builder.Script()
}

func balancedFundingOutputScript(witnessScript []byte) []byte {
	sum := sha256.Sum256(witnessScript)
	script := make([]byte, 0, 34)
	script = append(script, txscript.OP_0, 0x20)
	script = append(script, sum[:]...)
	return script
}

func balancedTxidSortKey(txid string) []byte {
	raw, err := hex.DecodeString(strings.TrimSpace(txid))
	if err != nil || len(raw) != 32 {
		return nil
	}
	for i := 0; i < len(raw)/2; i++ {
		raw[i], raw[len(raw)-1-i] = raw[len(raw)-1-i], raw[i]
	}
	return raw
}

func encodeBalancedTxHex(tx *wire.MsgTx) (string, error) {
	if tx == nil {
		return "", errors.New("missing transaction")
	}
	var b bytes.Buffer
	if err := tx.Serialize(&b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b.Bytes()), nil
}

func balancedDecodeWitnessStack(values []string) (wire.TxWitness, error) {
	if len(values) == 0 {
		return nil, errors.New("missing witness stack")
	}
	stack := make(wire.TxWitness, 0, len(values))
	for _, item := range values {
		raw, err := hex.DecodeString(strings.TrimSpace(item))
		if err != nil {
			return nil, errors.New("invalid witness item")
		}
		stack = append(stack, raw)
	}
	return stack, nil
}

func balancedEncodeWitnessStack(witness [][]byte) []string {
	if len(witness) == 0 {
		return nil
	}
	out := make([]string, 0, len(witness))
	for _, item := range witness {
		out = append(out, hex.EncodeToString(item))
	}
	return out
}

func cloneBalancedWitness(src wire.TxWitness) wire.TxWitness {
	if len(src) == 0 {
		return nil
	}
	out := make(wire.TxWitness, 0, len(src))
	for _, item := range src {
		out = append(out, append([]byte(nil), item...))
	}
	return out
}

func balancedEffectiveFeeRate(value int64) int64 {
	if value <= 0 {
		return 1
	}
	return value
}

func balancedTransitContribution(capacitySat int64, feeRateSatVb int64) (int64, error) {
	if capacitySat <= 0 || capacitySat%2 != 0 {
		return 0, ErrBalancedOpenInvalidCapacity
	}
	if feeRateSatVb <= 0 {
		return 0, ErrBalancedOpenInvalidFeeRate
	}
	return (capacitySat + (balancedOpenFundingVBytes * feeRateSatVb)) / 2, nil
}

func isBalancedOpenTxID(txid string) bool {
	value := strings.TrimSpace(txid)
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func isBalancedOpenScriptHex(scriptHex string) bool {
	value := strings.TrimSpace(scriptHex)
	if value == "" {
		return false
	}
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) > 0
}

func isBalancedPendingChanIDHex(value string) bool {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) != 64 {
		return false
	}
	_, err := hex.DecodeString(trimmed)
	return err == nil
}

func newBalancedOpenPendingChanIDHex() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func isBalancedOpenAlreadyRegisteredErr(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return false
	}
	return strings.Contains(text, "already registered") ||
		strings.Contains(text, "already exists")
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
