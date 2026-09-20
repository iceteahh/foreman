package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/100xteam-ai/foreman/internal/store"
)

// AcquireLease implements store.Leases: one row per named singleton job, taken
// by whichever replica gets there first and reclaimable once it expires. No
// election and no coordinator — a replica that dies holding a lease costs the
// job one TTL of delay, not a stuck queue (plan Step 22).
func (s *Store) AcquireLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	if name == "" || owner == "" {
		return false, errors.New("lease: name and owner are required")
	}
	if ttl <= 0 {
		return false, errors.New("lease: ttl must be > 0")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var holder, expires string
	err = tx.QueryRowContext(ctx, `SELECT owner, expires_at FROM leases WHERE name = ?`, name).Scan(&holder, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO leases(name, owner, expires_at, updated_at) VALUES (?,?,?,?)`,
			name, owner, ts(now.Add(ttl)), ts(now)); err != nil {
			return false, err
		}
		return true, tx.Commit()
	case err != nil:
		return false, err
	}
	until, perr := time.Parse(time.RFC3339Nano, expires)
	// An unparseable expiry is treated as expired: a lease nobody can read the
	// end of would otherwise block its job forever.
	if holder != owner && perr == nil && until.After(now) {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE leases SET owner = ?, expires_at = ?, updated_at = ? WHERE name = ?`,
		owner, ts(now.Add(ttl)), ts(now), name); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ReleaseLease implements store.Leases.
func (s *Store) ReleaseLease(ctx context.Context, name, owner string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE name = ? AND owner = ?`, name, owner)
	return err
}

var _ store.Leases = (*Store)(nil)
