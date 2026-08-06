package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// LocalIDFence is the AUTOINCREMENT high-water mark applied to repository-local
// stores.
//
// Observation ids are per-database AUTOINCREMENT values, so a global store and a
// repo-local store would both hand out #1, #2, #3 — and a bare id coming back
// from the model would be ambiguous. Raising the local store's sequence past
// this fence makes local ids start at LocalIDFence+1, which lets any id be
// routed to the store that minted it by magnitude alone.
//
// The global store would need a billion observations to reach the fence.
const LocalIDFence int64 = 1_000_000_000

// fencedTables are the tables whose ids reach the model and therefore need to be
// unambiguous. Internal sequences (sync_mutations, memory_relations) are not
// addressed by integer id through any tool, so they are left alone.
var fencedTables = []string{"observations", "user_prompts"}

// IsLocalID reports whether id was minted by a fenced (repository-local) store.
func IsLocalID(id int64) bool { return id > LocalIDFence }

// ApplyLocalIDFence raises the AUTOINCREMENT sequence of the id-bearing tables
// to LocalIDFence, so subsequent inserts get ids above it.
//
// It is idempotent and only ever raises the sequence, never lowers it — safe to
// call on every open of a local store, which is what makes it robust against a
// migration that rebuilds a table.
//
// A non-empty warning means the store already held rows below the fence: their
// ids stay ambiguous with the global store's and cannot be retroactively moved.
func (s *Store) ApplyLocalIDFence() (string, error) {
	warning := ""
	for _, table := range fencedTables {
		var maxID int64
		//nolint:gosec // table comes from the fencedTables constant, not from input.
		if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM ` + table).Scan(&maxID); err != nil {
			return "", fmt.Errorf("engram: read max id of %s: %w", table, err)
		}
		if maxID > LocalIDFence {
			// Already fenced and in use — nothing to raise.
			continue
		}
		if maxID > 0 && warning == "" {
			warning = fmt.Sprintf(
				"local store already contains rows with ids below %d; those ids overlap the global store and cannot be disambiguated by id alone",
				LocalIDFence,
			)
		}
		if err := raiseSequence(s.db, table, LocalIDFence); err != nil {
			return warning, err
		}
	}
	return warning, nil
}

// raiseSequence sets sqlite_sequence.seq for table to at least want.
//
// sqlite_sequence has no UNIQUE constraint on name, so this reads before writing
// rather than using an upsert.
func raiseSequence(db *sql.DB, table string, want int64) error {
	var current sql.NullInt64
	err := db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name = ?`, table).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No row yet: the table has never been inserted into.
		if _, err := db.Exec(`INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)`, table, want); err != nil {
			return fmt.Errorf("engram: seed id fence for %s: %w", table, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("engram: read id sequence for %s: %w", table, err)
	}

	if current.Valid && current.Int64 >= want {
		return nil
	}
	if _, err := db.Exec(`UPDATE sqlite_sequence SET seq = ? WHERE name = ?`, want, table); err != nil {
		return fmt.Errorf("engram: raise id fence for %s: %w", table, err)
	}
	return nil
}
