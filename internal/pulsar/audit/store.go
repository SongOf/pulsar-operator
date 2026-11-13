// Package audit keeps durable, redacted records of Pulsar management operations.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type Resource struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	Version    string `json:"resourceVersion,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}

type Change struct {
	Path     string `json:"path"`
	Before   any    `json:"before,omitempty"`
	After    any    `json:"after,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

type Record struct {
	Sequence        int64    `json:"sequence"`
	ID              string   `json:"id"`
	OperationID     string   `json:"operationId"`
	StartedAt       string   `json:"startedAt"`
	FinishedAt      string   `json:"finishedAt,omitempty"`
	Source          string   `json:"source"`
	Actor           string   `json:"actor"`
	ClientIP        string   `json:"clientIP,omitempty"`
	Action          string   `json:"action"`
	ClusterUID      string   `json:"clusterUID,omitempty"`
	Resource        Resource `json:"resource"`
	PreviousVersion string   `json:"previousVersion,omitempty"`
	ExpectedVersion string   `json:"expectedVersion,omitempty"`
	ObservedUID     string   `json:"observedUID,omitempty"`
	Result          string   `json:"result"`
	HTTPStatus      int      `json:"httpStatus,omitempty"`
	Error           string   `json:"error,omitempty"`
	Changes         []Change `json:"changes,omitempty"`
	Truncated       bool     `json:"truncated,omitempty"`
}

type Query struct {
	OperationID, Namespace, Name, UID, Source string
	Cursor                                    int64
	Limit                                     int
}

type Page struct {
	Items      []Record `json:"items"`
	NextCursor int64    `json:"nextCursor,omitempty"`
}

type Store struct {
	db   *sql.DB
	lock *os.File
}

func Open(directory string) (*Store, error) {
	path, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(path, ".audit.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("audit state is already in use by another process")
	}
	ok := false
	defer func() {
		if !ok {
			lock.Close()
		}
	}()
	dsn := &url.URL{Scheme: "file", Path: filepath.Join(path, "audit.sqlite3")}
	q := url.Values{"_txlock": {"immediate"}, "_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)"}}
	dsn.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, lock: lock}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS audit (
seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE,
operation_id TEXT NOT NULL, namespace TEXT NOT NULL, name TEXT NOT NULL,
uid TEXT NOT NULL, cluster_uid TEXT NOT NULL, source TEXT NOT NULL,
result TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS audit_operation ON audit(operation_id, seq);
CREATE INDEX IF NOT EXISTS audit_resource ON audit(namespace, name, seq);
CREATE INDEX IF NOT EXISTS audit_uid ON audit(uid, seq);
CREATE INDEX IF NOT EXISTS audit_cluster ON audit(cluster_uid, seq);`)
	if err == nil {
		// A previous process may have died before or after the Kubernetes write.
		// Never infer success or failure from an unfinished local record.
		_, err = db.Exec(`UPDATE audit SET result='Unknown',
data=json_set(data, '$.result', 'Unknown', '$.error', 'process stopped before outcome was recorded')
WHERE result='Started'`)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	ok = true
	return s, nil
}

func (s *Store) Close() error                    { return errors.Join(s.db.Close(), s.lock.Close()) }
func (s *Store) Check(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) Begin(ctx context.Context, record Record) (*Record, error) {
	record.ID, record.StartedAt, record.Result = uuid.NewString(), time.Now().UTC().Format(time.RFC3339Nano), "Started"
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO audit(id,operation_id,namespace,name,uid,cluster_uid,source,result,data) VALUES(?,?,?,?,?,?,?,?,?)`,
		record.ID, record.OperationID, record.Resource.Namespace, record.Resource.Name, record.Resource.UID, record.ClusterUID, record.Source, record.Result, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("persist audit intent: %w", err)
	}
	record.Sequence, err = result.LastInsertId()
	return &record, err
}

func (s *Store) Finish(ctx context.Context, record *Record) error {
	if record.Result != "Succeeded" && record.Result != "Failed" && record.Result != "Unknown" {
		return errors.New("audit result must be terminal")
	}
	// HTTP disconnects and reconcile cancellation must not discard the result.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	record.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE audit SET namespace=?,name=?,uid=?,cluster_uid=?,result=?,data=? WHERE id=? AND result='Started'`,
		record.Resource.Namespace, record.Resource.Name, record.Resource.UID, record.ClusterUID, record.Result, string(encoded), record.ID)
	if err != nil {
		return fmt.Errorf("persist audit result: %w", err)
	}
	rows, err := result.RowsAffected()
	if err == nil && rows != 1 {
		err = errors.New("audit intent is missing or already completed")
	}
	return err
}

// Drop only an unfinished controller intent whose successful API response
// confirms there was no semantic change. Completed audit records stay immutable.
func (s *Store) discardNoop(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, "DELETE FROM audit WHERE id=? AND result='Started'", id)
	return err
}

func (s *Store) List(ctx context.Context, query Query) (Page, error) {
	page := Page{Items: []Record{}}
	if query.Limit == 0 {
		query.Limit = 50
	}
	if query.Limit < 1 || query.Limit > 200 || query.Cursor < 0 {
		return page, errors.New("invalid audit pagination")
	}
	where, values := []string{"1=1"}, []any{}
	for _, field := range []struct{ column, value string }{{"operation_id", query.OperationID}, {"namespace", query.Namespace}, {"name", query.Name}, {"source", query.Source}} {
		if len(field.value) > 253 {
			return page, errors.New("audit filter is too long")
		}
		if field.value != "" {
			where = append(where, field.column+"=?")
			values = append(values, field.value)
		}
	}
	if query.UID != "" {
		where = append(where, "(uid=? OR cluster_uid=?)")
		values = append(values, query.UID, query.UID)
	}
	if query.Cursor != 0 {
		where = append(where, "seq<?")
		values = append(values, query.Cursor)
	}
	values = append(values, query.Limit+1)
	rows, err := s.db.QueryContext(ctx, "SELECT seq,data FROM audit WHERE "+strings.Join(where, " AND ")+" ORDER BY seq DESC LIMIT ?", values...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int64
		var data string
		if err := rows.Scan(&seq, &data); err != nil {
			return page, err
		}
		var record Record
		if err := json.Unmarshal([]byte(data), &record); err != nil {
			return page, err
		}
		record.Sequence = seq
		page.Items = append(page.Items, record)
	}
	if len(page.Items) > query.Limit {
		page.Items = page.Items[:query.Limit]
		page.NextCursor = page.Items[len(page.Items)-1].Sequence
	}
	return page, rows.Err()
}
