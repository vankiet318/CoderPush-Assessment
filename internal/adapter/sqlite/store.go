// Package sqlite stores the catalog, update runs, changes and issues in a
// single SQLite file.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"catalogsync/internal/domain"
	"catalogsync/internal/usecase"
)

var (
	_ usecase.RunStore   = (*Store)(nil)
	_ usecase.ReadStore  = (*Store)(nil)
	_ usecase.IssueStore = (*Store)(nil)
	_ usecase.CatalogTx  = (*catalogTx)(nil)
)

type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database file and applies the schema.
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// synchronous(FULL): a committed batch survives power loss, so the
	// checkpoint never points past data that was lost.
	dsn := "file:" + filepath.ToSlash(abs) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serialises writes; batch transactions are short.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface {
	Scan(dest ...any) error
}

// ---- runs ----

const runColumns = `id, feed, status, cursor, counters, last_error, started_at, updated_at, finished_at`

func (s *Store) CreateRun(ctx context.Context, run domain.ImportRun) (domain.ImportRun, error) {
	counters, err := json.Marshal(run.Counters)
	if err != nil {
		return run, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO import_runs (feed, status, cursor, counters, last_error, started_at, updated_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		run.Feed, string(run.Status), run.Cursor, string(counters), run.LastError,
		formatTime(run.StartedAt), formatTime(run.UpdatedAt), formatTime(run.FinishedAt))
	if err != nil {
		return run, mapRunErr(err)
	}
	run.ID, err = res.LastInsertId()
	return run, err
}

func (s *Store) GetRun(ctx context.Context, id int64) (domain.ImportRun, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM import_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return run, fmt.Errorf("%w: update #%d", domain.ErrNotFound, id)
	}
	return run, err
}

// ListRuns returns runs newest first; limit <= 0 returns all of them.
func (s *Store) ListRuns(ctx context.Context, limit int) ([]domain.ImportRun, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM import_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []domain.ImportRun
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) SaveRun(ctx context.Context, run domain.ImportRun) error {
	return saveRun(ctx, s.db, run)
}

func saveRun(ctx context.Context, q querier, run domain.ImportRun) error {
	counters, err := json.Marshal(run.Counters)
	if err != nil {
		return err
	}
	res, err := q.ExecContext(ctx,
		`UPDATE import_runs SET status = ?, cursor = ?, counters = ?, last_error = ?, updated_at = ?, finished_at = ?
		 WHERE id = ?`,
		string(run.Status), run.Cursor, string(counters), run.LastError,
		formatTime(run.UpdatedAt), formatTime(run.FinishedAt), run.ID)
	if err != nil {
		return mapRunErr(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: update #%d", domain.ErrNotFound, run.ID)
	}
	return nil
}

func scanRun(sc scanner) (domain.ImportRun, error) {
	var (
		run                                        domain.ImportRun
		status, counters, started, updated, finish string
	)
	if err := sc.Scan(&run.ID, &run.Feed, &status, &run.Cursor, &counters, &run.LastError, &started, &updated, &finish); err != nil {
		return run, err
	}
	run.Status = domain.RunStatus(status)
	if err := json.Unmarshal([]byte(counters), &run.Counters); err != nil {
		return run, fmt.Errorf("decode counters of update #%d: %w", run.ID, err)
	}
	var err error
	if run.StartedAt, err = parseTime(started); err != nil {
		return run, err
	}
	if run.UpdatedAt, err = parseTime(updated); err != nil {
		return run, err
	}
	run.FinishedAt, err = parseTime(finish)
	return run, err
}

func mapRunErr(err error) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed: import_runs.status") {
		return domain.ErrRunInProgress
	}
	return err
}

// ---- batch transaction ----

func (s *Store) InTx(ctx context.Context, fn func(usecase.CatalogTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after a successful commit
	if err := fn(&catalogTx{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

type catalogTx struct {
	tx *sql.Tx
}

func (t *catalogTx) GetProduct(ctx context.Context, sku string) (*domain.Product, error) {
	p, err := scanProduct(t.tx.QueryRowContext(ctx, `SELECT `+productColumns+` FROM products WHERE sku = ?`, sku))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (t *catalogTx) SaveProduct(ctx context.Context, p domain.Product, runID int64, now time.Time) error {
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO products (sku, name, category, price_cents, status, source_updated_at, last_run_id, local_updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(sku) DO UPDATE SET
		   name = excluded.name, category = excluded.category, price_cents = excluded.price_cents,
		   status = excluded.status, source_updated_at = excluded.source_updated_at,
		   last_run_id = excluded.last_run_id, local_updated_at = excluded.local_updated_at`,
		p.SKU, p.Name, p.Category, p.PriceCents, string(p.Status), formatTime(p.SourceUpdatedAt), runID, formatTime(now))
	return err
}

func (t *catalogTx) AddChange(ctx context.Context, c domain.Change) error {
	fields, err := json.Marshal(c.Fields)
	if err != nil {
		return err
	}
	_, err = t.tx.ExecContext(ctx,
		`INSERT INTO changes (run_id, batch_id, sku, kind, fields, reason, at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.RunID, c.BatchID, c.SKU, string(c.Kind), string(fields), c.Reason, formatTime(c.At))
	return err
}

func (t *catalogTx) AddIssueIfNew(ctx context.Context, i domain.Issue) (bool, error) {
	res, err := t.tx.ExecContext(ctx,
		`INSERT INTO issues (run_id, sku, kind, message, fingerprint, status, created_at, resolved_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(fingerprint) DO NOTHING`,
		i.RunID, i.SKU, string(i.Kind), i.Message, i.Fingerprint, string(i.Status),
		formatTime(i.CreatedAt), formatTime(i.ResolvedAt))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (t *catalogTx) SaveRun(ctx context.Context, run domain.ImportRun) error {
	return saveRun(ctx, t.tx, run)
}

// ---- products ----

const productColumns = `sku, name, category, price_cents, status, source_updated_at`

func (s *Store) ListProducts(ctx context.Context) ([]domain.Product, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+productColumns+` FROM products ORDER BY sku`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var products []domain.Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	return products, rows.Err()
}

func scanProduct(sc scanner) (domain.Product, error) {
	var (
		p                domain.Product
		status, updateAt string
	)
	if err := sc.Scan(&p.SKU, &p.Name, &p.Category, &p.PriceCents, &status, &updateAt); err != nil {
		return p, err
	}
	p.Status = domain.ProductStatus(status)
	var err error
	p.SourceUpdatedAt, err = parseTime(updateAt)
	return p, err
}

// ---- changes ----

func (s *Store) ListChanges(ctx context.Context, runID int64) ([]domain.Change, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, batch_id, sku, kind, fields, reason, at FROM changes WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var changes []domain.Change
	for rows.Next() {
		var (
			ch               domain.Change
			kind, fields, at string
		)
		if err := rows.Scan(&ch.ID, &ch.RunID, &ch.BatchID, &ch.SKU, &kind, &fields, &ch.Reason, &at); err != nil {
			return nil, err
		}
		ch.Kind = domain.DecisionKind(kind)
		if err := json.Unmarshal([]byte(fields), &ch.Fields); err != nil {
			return nil, fmt.Errorf("decode change %d: %w", ch.ID, err)
		}
		if ch.At, err = parseTime(at); err != nil {
			return nil, err
		}
		changes = append(changes, ch)
	}
	return changes, rows.Err()
}

// ---- issues ----

const issueColumns = `id, run_id, sku, kind, message, fingerprint, status, created_at, resolved_at`

func (s *Store) ListIssues(ctx context.Context, f usecase.IssueFilter) ([]domain.Issue, error) {
	var (
		where []string
		args  []any
	)
	if f.Status != "" {
		where, args = append(where, "status = ?"), append(args, string(f.Status))
	}
	if f.RunID != 0 {
		where, args = append(where, "run_id = ?"), append(args, f.RunID)
	}
	query := `SELECT ` + issueColumns + ` FROM issues`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY id DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var issues []domain.Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, i)
	}
	return issues, rows.Err()
}

func (s *Store) GetIssue(ctx context.Context, id int64) (domain.Issue, error) {
	i, err := scanIssue(s.db.QueryRowContext(ctx, `SELECT `+issueColumns+` FROM issues WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return i, fmt.Errorf("%w: issue #%d", domain.ErrNotFound, id)
	}
	return i, err
}

func (s *Store) SaveIssue(ctx context.Context, i domain.Issue) error {
	res, err := s.db.ExecContext(ctx, `UPDATE issues SET status = ?, resolved_at = ? WHERE id = ?`,
		string(i.Status), formatTime(i.ResolvedAt), i.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: issue #%d", domain.ErrNotFound, i.ID)
	}
	return nil
}

func scanIssue(sc scanner) (domain.Issue, error) {
	var (
		i                                 domain.Issue
		kind, status, created, resolvedAt string
	)
	if err := sc.Scan(&i.ID, &i.RunID, &i.SKU, &kind, &i.Message, &i.Fingerprint, &status, &created, &resolvedAt); err != nil {
		return i, err
	}
	i.Kind, i.Status = domain.IssueKind(kind), domain.IssueStatus(status)
	var err error
	if i.CreatedAt, err = parseTime(created); err != nil {
		return i, err
	}
	i.ResolvedAt, err = parseTime(resolvedAt)
	return i, err
}

// ---- time encoding ----

// Times are stored as UTC RFC 3339 text; the zero time is stored as "".
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
