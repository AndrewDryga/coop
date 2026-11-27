// Package fleet owns durable controller state for a single operator.
package fleet

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	_ "modernc.org/sqlite"
)

type Controller struct {
	sourceToken func(context.Context, int64) (sourceToken, error)
	db          *sql.DB
	ca          *x509.Certificate
	key         *ecdsa.PrivateKey
}

const schema = `
CREATE TABLE IF NOT EXISTS issuer(id INTEGER PRIMARY KEY CHECK(id=1), cert BLOB NOT NULL, key BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS workers(id TEXT PRIMARY KEY, pool TEXT NOT NULL, invitation TEXT UNIQUE,
 expires INTEGER NOT NULL, fingerprint TEXT, renewal_fingerprint TEXT, revoked INTEGER NOT NULL DEFAULT 0, draining INTEGER NOT NULL DEFAULT 0, last_seen INTEGER, hello BLOB,daemon TEXT,slots_only INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS claimed_daemon ON workers(daemon) WHERE revoked=0;
CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY,pool TEXT NOT NULL,spec BLOB NOT NULL,prompt TEXT NOT NULL,worker TEXT,stage TEXT NOT NULL,session TEXT,turn TEXT,cancel_requested INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS job_accounts(job TEXT NOT NULL,account TEXT NOT NULL,PRIMARY KEY(job,account));
CREATE INDEX IF NOT EXISTS account_jobs ON job_accounts(account,job);
CREATE TABLE IF NOT EXISTS account_limits(account TEXT PRIMARY KEY,permits INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS quota(account TEXT PRIMARY KEY,measured INTEGER NOT NULL,resume INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS commands(id TEXT PRIMARY KEY,job TEXT NOT NULL,worker TEXT NOT NULL,payload BLOB NOT NULL,result BLOB,ready_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS pending_commands ON commands(worker,ready_at) WHERE result IS NULL;
CREATE TABLE IF NOT EXISTS bodies(command TEXT PRIMARY KEY,sha TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS events(job TEXT NOT NULL,sequence INTEGER NOT NULL,data BLOB NOT NULL, PRIMARY KEY(job,sequence));
`

// Open requires a private controller directory. Independent CLI processes and the
// server serialize changes using SQLite transactions.
func Open(dir string) (*Controller, error) {
	if dir == "" {
		return nil, errors.New("specify a private fleet state directory")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("fleet directory must be owner-only")
	}
	path := filepath.Join(dir, "controller.sqlite")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		if err = f.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("fleet database must be a private regular file")
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("_pragma", "busy_timeout(5000)")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	c := &Controller{db: db}
	if err = c.initialize(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Controller) initialize() error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(schema); err != nil {
		return err
	}
	var cert, key []byte
	err = tx.QueryRow("SELECT cert,key FROM issuer WHERE id=1").Scan(&cert, &key)
	if errors.Is(err, sql.ErrNoRows) {
		signer, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			return e
		}
		now := time.Now()
		template := &x509.Certificate{Subject: pkix.Name{CommonName: "Coop fleet workers"}, NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(10, 0, 0), KeyUsage: x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true}
		cert, e = x509.CreateCertificate(rand.Reader, template, template, &signer.PublicKey, signer)
		if e != nil {
			return e
		}
		key, e = x509.MarshalECPrivateKey(signer)
		if e != nil {
			return e
		}
		_, err = tx.Exec("INSERT INTO issuer VALUES(1,?,?)", cert, key)
	}
	if err != nil {
		return err
	}
	c.ca, err = x509.ParseCertificate(cert)
	if err != nil {
		return err
	}
	c.key, err = x509.ParseECPrivateKey(key)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Controller) Close() error { return c.db.Close() }

type Worker struct {
	ID       string          `json:"id"`
	Pool     string          `json:"pool"`
	Online   bool            `json:"online"`
	Revoked  bool            `json:"revoked"`
	Draining bool            `json:"draining"`
	Hello    json.RawMessage `json:"last_report,omitempty"`
}

func (c *Controller) Workers() ([]Worker, error) {
	rows, err := c.db.Query("SELECT id,pool,coalesce(last_seen>=?,0) AND revoked=0,revoked,draining,hello FROM workers ORDER BY id", time.Now().Add(-time.Minute).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Worker
	for rows.Next() {
		var w Worker
		var raw []byte
		if err := rows.Scan(&w.ID, &w.Pool, &w.Online, &w.Revoked, &w.Draining, &raw); err != nil {
			return nil, err
		}
		w.Hello = raw
		result = append(result, w)
	}
	return result, rows.Err()
}

func (c *Controller) Drain(id string, drain bool) error {
	res, err := c.db.Exec("UPDATE workers SET draining=? WHERE id=? AND revoked=0", drain, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if n == 0 && err == nil {
		return errors.New("worker is unknown or revoked")
	}
	return err
}

var identifier = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$")

func fingerprint(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }

func (c *Controller) Invite(id, pool string, slotsOnly bool) (string, error) {
	if !identifier.MatchString(id) || !identifier.MatchString(pool) {
		return "", errors.New("invalid worker or pool name")
	}
	var secret [32]byte
	rand.Read(secret[:])
	token := hex.EncodeToString(secret[:])
	result, err := c.db.Exec(`INSERT INTO workers(id,pool,invitation,expires,slots_only) VALUES(?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET pool=excluded.pool,invitation=excluded.invitation,expires=excluded.expires,
		fingerprint=NULL,renewal_fingerprint=NULL,revoked=0,draining=0,last_seen=NULL,hello=NULL,daemon=NULL,slots_only=excluded.slots_only
	WHERE (workers.revoked=1 OR (workers.fingerprint IS NULL AND workers.expires<=?))
	AND NOT EXISTS(SELECT 1 FROM jobs WHERE worker=workers.id)`, id, pool, fingerprint([]byte(token)), time.Now().Add(time.Hour).Unix(), slotsOnly, time.Now().Unix())
	if err != nil {
		return "", fmt.Errorf("create invitation for %s: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", errors.New("worker identity is in use; revoke an unused identity to replace a lost invitation")
	}
	return token, nil
}

func (c *Controller) Revoke(id string) error {
	result, err := c.db.Exec("UPDATE workers SET revoked=1, invitation=NULL, fingerprint=NULL WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return errors.New("unknown worker")
	}
	return err
}
