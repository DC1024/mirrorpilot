package store

import (
	"database/sql"
	"fmt"
)

// requireAffected turns "zero rows changed" into ErrNotFound.
//
// Without this, an UPDATE or DELETE against a missing row looks like success,
// and the caller only finds out much later when something else fails.
func requireAffected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected for %s: %w", what, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
