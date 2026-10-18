// Package sqlstore implements core.Store on top of database/sql. It is shared
// by the sqlite and postgres packages, which supply a Dialect and their own
// embedded migrations.
//
// Every table keeps the full record as a JSON document in its data column and
// promotes the fields used for lookups, filtering, ordering and uniqueness to
// typed, indexed columns. Timestamps used for ordering and pagination are
// stored as unix nanoseconds so that both dialects order identically.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

// Dialect captures the differences between SQL engines.
type Dialect struct {
	// Name is used in error messages ("sqlite", "postgres").
	Name string
	// NumberedPlaceholders rewrites '?' placeholders to $1, $2, ...
	NumberedPlaceholders bool
	// IsUniqueViolation reports whether err is a unique/primary key violation.
	IsUniqueViolation func(error) bool
	// IsForeignKeyViolation reports whether err is a foreign key violation.
	IsForeignKeyViolation func(error) bool
	// ILike is the case-insensitive LIKE operator.
	ILike string
	// SanitizeJSON, when set, rewrites encoded JSON documents before they are
	// written (e.g. PostgreSQL jsonb cannot store \u0000).
	SanitizeJSON func(string) string
	// MigrationLockSQL, when set, is executed inside the migration
	// transaction before reading schema_migrations (e.g. an advisory lock).
	MigrationLockSQL string
	// SkipLocked is appended to row-claiming subqueries (outbox claims), e.g.
	// " FOR UPDATE SKIP LOCKED" on PostgreSQL. Empty for SQLite, whose write
	// transactions are serialized.
	SkipLocked string
	// OutboxSeq is the outbox column that gives the insert order of rows
	// ("rowid" on SQLite, a serial column on PostgreSQL). The claim uses it to
	// find the first pending row of each partition key.
	OutboxSeq string
	// TxSavepoints wraps every write statement run inside InTx in a
	// savepoint, for engines where a failed statement aborts the whole
	// transaction (PostgreSQL).
	TxSavepoints bool
}

// Store is a database/sql backed core.Store.
type Store struct {
	db      *sql.DB
	d       Dialect
	onClose func() error

	notifier   Notifier
	notifyOnce sync.Once
}

var _ core.Store = (*Store)(nil)
var _ storeutil.Pruner = (*Store)(nil)

// New wraps an open database. onClose (optional) runs after db.Close.
func New(db *sql.DB, d Dialect, onClose func() error) *Store {
	return &Store{db: db, d: d, onClose: onClose}
}

// DB exposes the underlying handle (for tests and maintenance tooling).
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.onClose != nil {
		if cerr := s.onClose(); err == nil {
			err = cerr
		}
	}
	return err
}

// ---------- low-level helpers ----------

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rebind converts '?' placeholders for dialects that use numbered ones.
// Queries in this package never contain literal question marks.
func (s *Store) rebind(q string) string {
	if !s.d.NumberedPlaceholders || !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 16)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

func (s *Store) mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return core.ErrNotFound
	case s.d.IsUniqueViolation != nil && s.d.IsUniqueViolation(err):
		return fmt.Errorf("%w: %w", core.ErrConflict, err)
	case s.d.IsForeignKeyViolation != nil && s.d.IsForeignKeyViolation(err):
		return fmt.Errorf("%w: referenced record does not exist: %w", core.ErrNotFound, err)
	}
	return fmt.Errorf("%s: %w", s.d.Name, err)
}

func (s *Store) exec(ctx context.Context, q querier, query string, args ...any) (sql.Result, error) {
	res, err := q.ExecContext(ctx, s.rebind(query), args...)
	return res, s.mapErr(err)
}

// execOne runs a statement that must affect at least one row.
func (s *Store) execOne(ctx context.Context, q querier, query string, args ...any) error {
	res, err := s.exec(ctx, q, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return s.mapErr(err)
	}
	if n == 0 {
		return core.ErrNotFound
	}
	return nil
}

// row accumulates column/value pairs for inserts and updates.
type row struct {
	cols []string
	args []any
}

func (r *row) set(col string, v any) *row {
	r.cols = append(r.cols, col)
	r.args = append(r.args, v)
	return r
}

func docRow(id string, v any) (*row, error) {
	data, err := marshal(v)
	if err != nil {
		return nil, err
	}
	r := &row{}
	r.set("id", id).set("data", data)
	return r, nil
}

func marshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("%w: encode record: %w", core.ErrInvalid, err)
	}
	return string(b), nil
}

// prep applies dialect-specific JSON sanitization to document columns.
func (s *Store) prep(r *row) *row {
	if s.d.SanitizeJSON == nil {
		return r
	}
	for i, c := range r.cols {
		if c == "data" || c == "settings" {
			if v, ok := r.args[i].(string); ok {
				r.args[i] = s.d.SanitizeJSON(v)
			}
		}
	}
	return r
}

func (s *Store) insert(ctx context.Context, q querier, table string, r *row) error {
	r = s.prep(r)
	ph := strings.TrimSuffix(strings.Repeat("?,", len(r.cols)), ",")
	query := "INSERT INTO " + table + " (" + strings.Join(r.cols, ",") + ") VALUES (" + ph + ")"
	_, err := s.exec(ctx, q, query, r.args...)
	return err
}

// upsert inserts r or, on conflict with the given key columns, updates every
// non-key column.
func (s *Store) upsert(ctx context.Context, q querier, table string, key []string, r *row) error {
	r = s.prep(r)
	ph := strings.TrimSuffix(strings.Repeat("?,", len(r.cols)), ",")
	isKey := map[string]bool{}
	for _, k := range key {
		isKey[k] = true
	}
	var sets []string
	for _, c := range r.cols {
		if !isKey[c] {
			sets = append(sets, c+"=excluded."+c)
		}
	}
	query := "INSERT INTO " + table + " (" + strings.Join(r.cols, ",") + ") VALUES (" + ph + ")" +
		" ON CONFLICT (" + strings.Join(key, ",") + ") DO UPDATE SET " + strings.Join(sets, ",")
	_, err := s.exec(ctx, q, query, r.args...)
	return err
}

// updateVersioned updates every column of r except id for the row with id
// whose version column equals old. r must set the version column to the new
// version. It returns core.ErrNotFound when the row does not exist and an
// error that wraps core.ErrStale when the version differs.
func (s *Store) updateVersioned(ctx context.Context, table, kind string, r *row, id string, old int64) error {
	r = s.prep(r)
	var sets []string
	var args []any
	for i, c := range r.cols {
		if c == "id" {
			continue
		}
		sets = append(sets, c+"=?")
		args = append(args, r.args[i])
	}
	args = append(args, id, old)
	q := s.conn(ctx)
	res, err := s.exec(ctx, q, "UPDATE "+table+" SET "+strings.Join(sets, ",")+" WHERE id=? AND version=?", args...)
	if err != nil {
		return err
	}
	n, err := s.rowsAffected(res)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	var one int
	if err := q.QueryRowContext(ctx, s.rebind("SELECT 1 FROM "+table+" WHERE id=?"), id).Scan(&one); err != nil {
		return s.mapErr(err)
	}
	return fmt.Errorf("%w: %s %s changed", core.ErrStale, kind, id)
}

// update updates every column of r except id for the row with r's id.
func (s *Store) update(ctx context.Context, q querier, table string, r *row) error {
	r = s.prep(r)
	var sets []string
	var args []any
	var id any
	for i, c := range r.cols {
		if c == "id" {
			id = r.args[i]
			continue
		}
		sets = append(sets, c+"=?")
		args = append(args, r.args[i])
	}
	args = append(args, id)
	return s.execOne(ctx, q, "UPDATE "+table+" SET "+strings.Join(sets, ",")+" WHERE id=?", args...)
}

func getDoc[T any](ctx context.Context, s *Store, q querier, query string, args ...any) (*T, error) {
	var data []byte
	if err := q.QueryRowContext(ctx, s.rebind(query), args...).Scan(&data); err != nil {
		return nil, s.mapErr(err)
	}
	v := new(T)
	if err := json.Unmarshal(data, v); err != nil {
		return nil, fmt.Errorf("%s: decode record: %w", s.d.Name, err)
	}
	return v, nil
}

func listDocs[T any](ctx context.Context, s *Store, q querier, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, s.mapErr(err)
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, s.mapErr(err)
		}
		var v T
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("%s: decode record: %w", s.d.Name, err)
		}
		out = append(out, v)
	}
	return out, s.mapErr(rows.Err())
}

// where builds a conjunction of conditions.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

func (w *where) in(col string, vals []string) {
	if len(vals) == 0 {
		return
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")
	args := make([]any, len(vals))
	for i, v := range vals {
		args[i] = v
	}
	w.add(col+" IN ("+ph+")", args...)
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// page appends cursor conditions and returns the ORDER BY/LIMIT suffix for a
// (created_ns DESC, id DESC) listing.
func (w *where) page(opts core.ListOpts) (string, error) {
	c, err := storeutil.DecodeCursor(opts.Cursor)
	if err != nil {
		return "", err
	}
	if opts.Ascending {
		if c != nil {
			w.add("(created_ns > ? OR (created_ns = ? AND id > ?))", c.CreatedNS, c.CreatedNS, c.ID)
		}
		return " ORDER BY created_ns ASC, id ASC LIMIT " + strconv.Itoa(storeutil.Limit(opts.Limit)), nil
	}
	if c != nil {
		w.add("(created_ns < ? OR (created_ns = ? AND id < ?))", c.CreatedNS, c.CreatedNS, c.ID)
	}
	return " ORDER BY created_ns DESC, id DESC LIMIT " + strconv.Itoa(storeutil.Limit(opts.Limit)), nil
}

func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// StripJSONNul replaces \u0000 escapes inside JSON strings with \ufffd.
// Escaped backslashes are honored, so a literal "\\u0000" is preserved.
func StripJSONNul(s string) string {
	if !strings.Contains(s, `\u0000`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		if strings.HasPrefix(s[i:], `\u0000`) {
			b.WriteString(`\ufffd`)
			i += len(`\u0000`) - 1
			continue
		}
		b.WriteByte(c)
		b.WriteByte(s[i+1])
		i++
	}
	return b.String()
}
