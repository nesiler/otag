package run

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/nesiler/otag/internal/apperr"
	"github.com/nesiler/otag/internal/db"
)

// TokenEnv carries a run's secret token in its environment. The run row keeps only its
// hash; `otag event` and the child commands present the token, so a hook or a command
// that names another run (a confused child's `otag --run other done`) is refused. It is
// a speed bump for accidents, not a security boundary: the child can read the
// database file. A later broker can authenticate runs by the same token.
const TokenEnv = "OTAG_RUN_TOKEN"

// newToken returns a fresh token and its hash.
func newToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = hex.EncodeToString(b)
	return token, tokenHash(token), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// hasToken reports whether run runID was created with a token (runs from before
// tokens have none).
func hasToken(ctx context.Context, q db.Querier, runID string) bool {
	var hash sql.NullString
	return q.QueryRowContext(ctx, `SELECT token_hash FROM runs WHERE id = ?`, runID).Scan(&hash) == nil && hash.Valid
}

// CheckToken verifies that token belongs to run runID. A run created before tokens
// (no hash) accepts any caller.
func CheckToken(ctx context.Context, q db.Querier, runID, token string) error {
	var hash sql.NullString
	err := q.QueryRowContext(ctx, `SELECT token_hash FROM runs WHERE id = ?`, runID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return apperr.New(apperr.NotFound, "run %s not found", runID)
	}
	if err != nil || !hash.Valid {
		return err
	}
	if token == "" {
		return apperr.New(apperr.Forbidden, "run %s: %s is not set; only the run itself may do this", runID, TokenEnv)
	}
	if subtle.ConstantTimeCompare([]byte(tokenHash(token)), []byte(hash.String)) != 1 {
		return apperr.New(apperr.Forbidden, "%s does not belong to run %s; a child acts only for its own run", TokenEnv, runID)
	}
	return nil
}
