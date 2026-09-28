package core

import (
	"context"
	"database/sql"
	"errors"
)

// LabelsArgs asks how ids are shown to a human (no ULIDs in text output).
type LabelsArgs struct {
	IDs []string `json:"ids"`
}

// Labels (admin, read-only) maps each id to what a human sees: a participant by its name, a
// message by its #seq, a team by its name. Other ids (runs) have no label.
func (e *Engine) Labels(ctx context.Context, a LabelsArgs) (map[string]string, error) {
	out := map[string]string{}
	err := e.readOnly(ctx, func(t *txn) error {
		for _, id := range a.IDs {
			if _, done := out[id]; done || id == "" {
				continue
			}
			var label string
			err := t.QueryRowContext(t.ctx, `SELECT name FROM participants WHERE id=?
				UNION ALL SELECT '#' || seq FROM messages WHERE id=?
				UNION ALL SELECT name FROM teams WHERE id=?`, id, id, id).Scan(&label)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return internal(err)
			}
			out[id] = label
		}
		return nil
	})
	return out, err
}
