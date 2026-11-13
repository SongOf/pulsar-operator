// Package store preserves DiskFleet's SQLite plan/audit format and implements
// its local simulation backend. It never opens a block device.
package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type Store struct {
	root string
	db   *sql.DB
}

var _ model.Backend = (*Store)(nil)

// New opens the same state.sqlite3 file and tables as the Python implementation.
// Every physical connection uses BEGIN IMMEDIATE and a busy timeout; separate
// Store instances/processes therefore serialize allocation and plan commits too.
func New(root string) (*Store, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	if absolute, err = filepath.EvalSymlinks(absolute); err != nil {
		return nil, err
	}
	dsn := &url.URL{Scheme: "file", Path: filepath.Join(absolute, "state.sqlite3")}
	query := url.Values{}
	query.Set("_txlock", "immediate")
	query.Add("_pragma", "busy_timeout(30000)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	// The original backend serialized even read transactions. Keep this local
	// bound; SQLite still coordinates other API instances and Python processes.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{root: absolute, db: db}
	err = s.Transaction(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS objects (kind TEXT, id TEXT, node TEXT, name TEXT, state TEXT, data TEXT, PRIMARY KEY(kind,id));
CREATE INDEX IF NOT EXISTS objects_lookup ON objects(kind,node,state);
CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, time TEXT, level TEXT, action TEXT, message TEXT, node TEXT);
CREATE TABLE IF NOT EXISTS plans (id TEXT PRIMARY KEY, expires REAL, consumed INTEGER DEFAULT 0, data TEXT);`)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Put(ctx context.Context, tx *sql.Tx, kind string, obj model.Object) error {
	id, err := model.Identifier(obj["id"])
	if err != nil {
		return err
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	state, ok := obj["state"]
	if !ok {
		state = obj["status"]
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO objects(kind,id,node,name,state,data) VALUES (?,?,?,?,?,?) ON CONFLICT(kind,id) DO UPDATE SET node=excluded.node,name=excluded.name,state=excluded.state,data=excluded.data`, kind, id, model.String(obj["node"]), model.String(obj["name"]), model.String(state), string(data))
	return err
}

// Get returns (nil,nil) when an object is absent. Public demo operations translate
// this to 404; the cluster backend uses absence for durable release recovery.
func (s *Store) Get(ctx context.Context, tx *sql.Tx, kind, key string) (model.Object, error) {
	if _, err := model.Identifier(key); err != nil {
		return nil, err
	}
	var data string
	if err := tx.QueryRowContext(ctx, "SELECT data FROM objects WHERE kind=? AND id=?", kind, key).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return decodeObject(data)
}

func (s *Store) getRequired(ctx context.Context, tx *sql.Tx, kind, key string) (model.Object, error) {
	value, err := s.Get(ctx, tx, kind, key)
	if err == nil && value == nil {
		return nil, model.NewProblem(404, "资源不存在")
	}
	return value, err
}

func (s *Store) Delete(ctx context.Context, tx *sql.Tx, kind, id string) error {
	if _, err := model.Identifier(id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM objects WHERE kind=? AND id=?", kind, id)
	return err
}

func (s *Store) Event(ctx context.Context, tx *sql.Tx, action, message, node, level string) error {
	if level == "" {
		level = "info"
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO events(time,level,action,message,node) VALUES (?,?,?,?,?)", model.Now(), level, action, message, node)
	return err
}

func (s *Store) Audit(ctx context.Context, action, message, node, level string) error {
	return s.Transaction(ctx, func(tx *sql.Tx) error { return s.Event(ctx, tx, action, message, node, level) })
}

func options(opts model.ListOptions) (model.ListOptions, error) {
	if opts.Limit == 0 {
		opts.Limit = 50
	}
	if opts.Limit < 1 || opts.Offset < 0 {
		return opts, model.NewProblem(400, "无效分页参数")
	}
	return opts, nil
}

func (s *Store) List(ctx context.Context, kind string, opts model.ListOptions) (model.Page, error) {
	opts, err := options(opts)
	if err != nil {
		return model.Page{}, err
	}
	page := model.Page{Items: []model.Object{}, Limit: opts.Limit, Offset: opts.Offset}
	terms, values := []string{"kind=?"}, []any{kind}
	for _, field := range []struct{ name, value string }{{"node", opts.Node}, {"state", opts.State}} {
		if field.value != "" {
			terms = append(terms, field.name+"=?")
			values = append(values, field.value)
		}
	}
	if opts.Query != "" {
		terms = append(terms, `(name LIKE ? ESCAPE '!' OR data LIKE ? ESCAPE '!')`)
		escaped := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(opts.Query)
		values = append(values, "%"+escaped+"%", "%"+escaped+"%")
	}
	where := strings.Join(terms, " AND ")
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM objects WHERE "+where, values...).Scan(&page.Total); err != nil {
			return err
		}
		args := append(append([]any{}, values...), opts.Limit, opts.Offset)
		items, err := queryObjects(ctx, tx, "SELECT data FROM objects WHERE "+where+" ORDER BY node,name LIMIT ? OFFSET ?", args...)
		if err != nil {
			return err
		}
		page.Items = items
		if kind == "nodes" {
			for _, item := range items {
				pools, err := queryObjects(ctx, tx, "SELECT data FROM objects WHERE kind='pools' AND node=?", item["name"])
				if err != nil {
					return err
				}
				var allocated int64
				for _, pool := range pools {
					allocated += model.Int64(pool["allocatedBytes"])
				}
				item["allocatedBytes"] = allocated
			}
		}
		return nil
	})
	return page, err
}

func (s *Store) Events(ctx context.Context, opts model.ListOptions) (model.Page, error) {
	opts, err := options(opts)
	if err != nil {
		return model.Page{}, err
	}
	page := model.Page{Items: []model.Object{}, Limit: opts.Limit, Offset: opts.Offset}
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&page.Total); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, "SELECT id,time,level,action,message,node FROM events ORDER BY id DESC LIMIT ? OFFSET ?", opts.Limit, opts.Offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var when, level, action, message, node string
			if err := rows.Scan(&id, &when, &level, &action, &message, &node); err != nil {
				return err
			}
			page.Items = append(page.Items, model.Object{"id": id, "time": when, "level": level, "action": action, "message": message, "node": node})
		}
		return rows.Err()
	})
	return page, err
}

func (s *Store) SavePlan(ctx context.Context, data model.Object) (model.Object, error) {
	id := uuid.NewString()
	now := time.Now()
	expires := now.Add(10 * time.Minute)
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM plans WHERE expires < ?", float64(now.Add(-24*time.Hour).UnixNano())/1e9); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO plans(id,expires,data) VALUES (?,?,?)", id, float64(expires.UnixNano())/1e9, string(encoded))
		return err
	})
	if err != nil {
		return nil, err
	}
	result, err := decodeObject(string(encoded))
	if err != nil {
		return nil, err
	}
	result["planId"], result["expiresAt"] = id, expires.UTC().Format(time.RFC3339Nano)
	return result, nil
}

func (s *Store) ReadPlan(ctx context.Context, tx *sql.Tx, id, confirmation string) (model.Object, bool, error) {
	if _, err := model.Identifier(id); err != nil {
		return nil, false, err
	}
	if n := utf8.RuneCountInString(confirmation); n < 1 || n > 512 {
		return nil, false, model.NewProblem(400, "确认文本必须为 1–512 字符的字符串")
	}
	var expires float64
	var consumed int
	var encoded string
	err := tx.QueryRowContext(ctx, "SELECT expires,consumed,data FROM plans WHERE id=?", id).Scan(&expires, &consumed, &encoded)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && expires < float64(time.Now().UnixNano())/1e9) {
		return nil, false, model.NewProblem(409, "计划不存在或已过期，请重新生成")
	}
	if err != nil {
		return nil, false, err
	}
	data, err := decodeObject(encoded)
	if err != nil {
		return nil, false, err
	}
	if confirmation != model.String(data["confirmation"]) {
		return nil, false, model.NewProblem(409, "确认文本不匹配")
	}
	return data, consumed != 0, nil
}

func (s *Store) ConsumePlan(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := model.Identifier(id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE plans SET consumed=1 WHERE id=?", id)
	return err
}

func decodeObject(data string) (model.Object, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(data))
	decoder.UseNumber()
	var value model.Object
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("persisted JSON must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("persisted JSON has trailing data")
	}
	return value, nil
}

func queryObjects(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]model.Object, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []model.Object{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		value, err := decodeObject(data)
		if err != nil {
			return nil, err
		}
		items = append(items, value)
	}
	return items, rows.Err()
}
