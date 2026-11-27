package fleet

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func (c *Controller) Server(address string) *http.Server {
	roots := x509.NewCertPool()
	roots.AddCert(c.ca)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/coop-workers/enroll", c.identity)
	mux.HandleFunc("POST /v1/coop-workers/renew", c.identity)
	mux.HandleFunc("POST /v1/coop-workers/poll", c.poll)
	mux.HandleFunc("POST /v1/coop-workers/jobs/{job}/source-grants", c.sourceGrant)
	mux.HandleFunc("PUT /v1/coop-workers/commands/{command}/response-body", c.uploadResponse)
	return &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: roots}}
}

// Check command ownership before accepting a potentially large body, then again
// in the write transaction so revocation during transfer wins.
func (c *Controller) uploadResponse(w http.ResponseWriter, r *http.Request) {
	id, hash := peer(r)
	if id == "" {
		w.WriteHeader(403)
		return
	}
	ref := workerproto.BodyReference{SHA256: r.Header.Get("X-Coop-Body-SHA256"), ByteSize: r.ContentLength}
	if ref.Validate() != nil {
		w.WriteHeader(400)
		return
	}
	if ref.ByteSize > 32<<20 {
		w.WriteHeader(413)
		return
	}
	command := r.PathValue("command")
	const authority = "SELECT 1 FROM commands c JOIN workers w ON w.id=c.worker WHERE c.id=? AND w.id=? AND w.fingerprint=? AND w.revoked=0 AND c.result IS NULL"
	var found int
	if err := c.db.QueryRowContext(r.Context(), authority, command, id, hash).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(403)
		} else {
			w.WriteHeader(503)
		}
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ref.ByteSize))
	if err != nil || int64(len(body)) != ref.ByteSize || fingerprint(body) != ref.SHA256 {
		w.WriteHeader(400)
		return
	}
	tx, err := c.db.BeginTx(r.Context(), nil)
	if err != nil {
		w.WriteHeader(503)
		return
	}
	defer tx.Rollback()
	if err = tx.QueryRow(authority, command, id, hash).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			w.WriteHeader(403)
		} else {
			w.WriteHeader(503)
		}
		return
	}
	if _, err = tx.Exec("INSERT OR IGNORE INTO bodies VALUES(?,?,?)", command, ref.SHA256, body); err != nil {
		w.WriteHeader(503)
		return
	}
	var saved string
	if err = tx.QueryRow("SELECT sha FROM bodies WHERE command=?", command).Scan(&saved); err != nil {
		w.WriteHeader(503)
		return
	}
	if saved != ref.SHA256 {
		w.WriteHeader(409)
		return
	}
	if err = tx.Commit(); err != nil {
		w.WriteHeader(503)
		return
	}
	answer(w, 200, ref)
}

func peer(r *http.Request) (string, string) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return "", ""
	}
	cert := r.TLS.VerifiedChains[0][0]
	return cert.Subject.CommonName, fingerprint(cert.Raw)
}
func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (c *Controller) identity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token     string `json:"token"`
		PublicKey string `json:"public_key_pem"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(&struct{}{}) != io.EOF {
		w.WriteHeader(400)
		return
	}
	block, rest := pem.Decode([]byte(req.PublicKey))
	if block == nil || block.Type != "PUBLIC KEY" || len(rest) != 0 {
		w.WriteHeader(400)
		return
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	rsaKey, ok := pub.(*rsa.PublicKey)
	if err != nil || !ok || rsaKey.N.BitLen() < 3072 || rsaKey.N.BitLen() > 4096 || rsaKey.E != 65537 {
		w.WriteHeader(400)
		return
	}
	tx, err := c.db.BeginTx(r.Context(), nil)
	if err != nil {
		w.WriteHeader(503)
		return
	}
	defer tx.Rollback()
	id, certHash := peer(r)
	var pool string
	now := time.Now().UTC().Truncate(time.Second)
	status := http.StatusOK
	if r.URL.Path == "/v1/coop-workers/enroll" {
		if id != "" || req.Token == "" {
			w.WriteHeader(403)
			return
		}
		err = tx.QueryRow("SELECT id,pool FROM workers WHERE invitation=? AND revoked=0 AND expires>?", fingerprint([]byte(req.Token)), now.Unix()).Scan(&id, &pool)
		status = http.StatusCreated
	} else {
		if id == "" || req.Token != "" {
			w.WriteHeader(403)
			return
		}
		err = tx.QueryRow("SELECT pool FROM workers WHERE id=? AND ? IN (fingerprint,renewal_fingerprint) AND revoked=0", id, certHash).Scan(&pool)
	}
	if errors.Is(err, sql.ErrNoRows) {
		w.WriteHeader(403)
		return
	}
	if err != nil {
		w.WriteHeader(503)
		return
	}
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: id}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, c.ca, rsaKey, c.key)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	hash := fingerprint(der)
	if _, err = tx.Exec("UPDATE workers SET fingerprint=?,renewal_fingerprint=nullif(?,''),invitation=NULL WHERE id=?", hash, certHash, id); err != nil {
		w.WriteHeader(503)
		return
	}
	if err = tx.Commit(); err != nil {
		w.WriteHeader(503)
		return
	}
	certPEM := func(b []byte) string { return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b})) }
	answer(w, status, map[string]string{"ca_certificate_pem": certPEM(c.ca.Raw), "certificate_pem": certPEM(der), "certificate_expires_at": leaf.NotAfter.Format(time.RFC3339), "certificate_sha256": hash, "worker_id": id, "workspace_ref": pool})
}

func (c *Controller) poll(w http.ResponseWriter, r *http.Request) {
	id, hash := peer(r)
	if id == "" {
		w.WriteHeader(403)
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, workerproto.MaxDocumentBytes))
	if err != nil {
		w.WriteHeader(413)
		return
	}
	p, err := workerproto.DecodePoll(b)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	if p.Worker.ID != id {
		w.WriteHeader(403)
		return
	}
	hello, err := json.Marshal(p.Worker)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	tx, err := c.db.BeginTx(r.Context(), nil)
	if err != nil {
		w.WriteHeader(503)
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE workers SET hello=?,last_seen=? WHERE id=? AND pool=? AND fingerprint=? AND revoked=0", hello, time.Now().Unix(), id, p.Worker.WorkspaceRef, hash)
	if err != nil {
		w.WriteHeader(503)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		w.WriteHeader(503)
		return
	}
	if n != 1 {
		w.WriteHeader(403)
		return
	}
	// Retain the runtime identity across disconnects. A second registration
	// for the same engine would otherwise multiply the machine's capacity.
	if machine := p.Worker.Capacity.Machine; machine != nil {
		res, err := tx.Exec(`UPDATE workers SET daemon=? WHERE id=? AND (daemon IS NULL OR daemon=?)
		AND NOT EXISTS (SELECT 1 FROM workers old JOIN jobs j ON j.worker=old.id WHERE old.id!=? AND old.daemon=?
		AND j.stage IN ('creating','operation','session','starting','running','cancelling','cancel_revision','uncertain'))`, machine.Daemon, id, machine.Daemon, id, machine.Daemon)
		if err != nil {
			w.WriteHeader(409)
			return
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			w.WriteHeader(409)
			return
		}
	} else {
		var requireMachine bool
		if err = tx.QueryRow("SELECT daemon IS NOT NULL OR slots_only=0 FROM workers WHERE id=?", id).Scan(&requireMachine); err != nil {
			w.WriteHeader(503)
			return
		}
		if requireMachine || r.Header.Get("X-Coop-Machine-Report") == "1" {
			p.Worker.State = "busy"
		}
	}
	response, err := dispatch(tx, p)
	if err != nil {
		w.WriteHeader(503)
		return
	}
	if err = tx.Commit(); err != nil {
		w.WriteHeader(503)
		return
	}
	w.Header().Set("X-Coop-Machine-Report", "1")
	answer(w, 200, response)
}
