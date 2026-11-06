package discussion

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

const (
	defaultStaleVoteMinutes = 30
	staleVoteScanInterval   = time.Minute
)

var scannerOnce sync.Once

// StartStaleVoteScanner closes, every minute, the open votes with no
// closes_at that are older than discussion_stale_vote_timeout minutes. It
// starts once per process; the scans stop when ctx ends. Each tick runs in
// its own goroutine, as setInterval does not wait for the last one.
func (s *Service) StartStaleVoteScanner(ctx context.Context) {
	scannerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(staleVoteScanInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					go s.ScanStaleVotes(ctx)
				}
			}
		}()
		s.log("stale-vote-scanner-started", logger.Fields{field("intervalMs", staleVoteScanInterval.Milliseconds())})
	})
}

// ScanStaleVotes is one scan: each stale vote gets closes_at = NOW(), so
// EvaluateVote sees it expired and closes it. A failure is logged and ends
// the scan.
func (s *Service) ScanStaleVotes(ctx context.Context) {
	if err := s.scanStaleVotes(ctx); err != nil {
		s.log("stale-vote-scan-error", logger.Fields{field("error", err.Error())})
	}
}

func (s *Service) scanStaleVotes(ctx context.Context) error {
	text, err := s.Config.Get("discussion_stale_vote_timeout")
	if err != nil {
		return err
	}
	// parseInt(...) || 30: NaN and 0 take the default.
	minutes, ok := jsutil.ParseInt(text)
	if !ok || minutes == 0 {
		minutes = defaultStaleVoteMinutes
	}
	rows, err := s.DB.Query(ctx,
		`SELECT id, discussion_id FROM discussion_votes
             WHERE status = 'open'
               AND closes_at IS NULL
               AND created_at < NOW() - INTERVAL '1 minute' * $1`, jsutil.Num(minutes))
	if err != nil {
		return err
	}
	type staleVote struct {
		id, discussionID int
	}
	stale, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (staleVote, error) {
		var v staleVote
		err := row.Scan(&v.id, &v.discussionID)
		return v, err
	})
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	s.log("stale-votes-found", logger.Fields{field("count", len(stale)), field("thresholdMinutes", jsNumber(minutes))})
	for _, v := range stale {
		if _, err := s.DB.Exec(ctx, "UPDATE discussion_votes SET closes_at = NOW() WHERE id = $1 AND status = $2", v.id, "open"); err != nil {
			return err
		}
		if err := s.EvaluateVote(ctx, IntID(v.id)); err != nil {
			return err
		}
		s.log("stale-vote-closed", logger.Fields{field("vote_id", v.id), field("discussion_id", v.discussionID)})
	}
	return nil
}
