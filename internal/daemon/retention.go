package daemon

import (
	"context"
	"database/sql"
	"time"

	"github.com/nesiler/otag/internal/db"
)

// pruneEvery is how often the raw payload retention runs.
const pruneEvery = time.Hour

// pruneRawPayloads deletes raw_hook_payloads rows older than RawPayloadRetention, at
// most once per pruneEvery. Events and state keep their own rows; a raw payload is
// only a debugging aid.
func (d *Daemon) pruneRawPayloads(ctx context.Context) error {
	keep := d.Config.RawPayloadRetention
	now := d.now()
	if keep <= 0 || now.Sub(d.lastPrune) < pruneEvery {
		return nil
	}
	cutoff := now.Add(-keep).UTC().Format(db.TimeFormat)
	err := d.DB.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM raw_hook_payloads WHERE received_at < ?`, cutoff)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			d.logf("raw payload retention: deleted %d rows older than %s", n, keep)
		}
		return nil
	})
	if err == nil {
		d.lastPrune = now
	}
	return err
}
