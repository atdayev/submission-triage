package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// LLMSpendRepository tracks estimated LLM spend per UTC day in SQLite, so a
// restart doesn't reset the running total.
type LLMSpendRepository struct {
	db *sql.DB
}

// NewLLMSpendRepository returns a SQLite-backed spend tracker.
func NewLLMSpendRepository(db *sql.DB) *LLMSpendRepository {
	return &LLMSpendRepository{db: db}
}

// Today returns the total estimated USD spent so far this UTC day.
func (r *LLMSpendRepository) Today(ctx context.Context) (float64, error) {
	var usd float64
	err := r.db.QueryRowContext(ctx, `SELECT usd FROM llm_spend WHERE day = ?`, utcDay()).Scan(&usd)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("llm_spend: query: %w", err)
	}
	return usd, nil
}

// Add increments this UTC day's spend by usd.
func (r *LLMSpendRepository) Add(ctx context.Context, usd float64) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO llm_spend (day, usd) VALUES (?, ?)
		ON CONFLICT(day) DO UPDATE SET usd = usd + excluded.usd`, utcDay(), usd)
	if err != nil {
		return fmt.Errorf("llm_spend: add: %w", err)
	}
	return nil
}

func utcDay() string { return time.Now().UTC().Format("2006-01-02") }
