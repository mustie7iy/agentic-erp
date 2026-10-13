package fieldcrypt

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Key management (T14): every tenant has its own data key and fingerprint
// key, generated at provisioning (or at its first rotation for tenants
// older than this), stored wrapped by a master key in tenancy.tenant_key.
// Masters form a ring: the first wraps new tenant keys, the others still
// unwrap. Rotating a tenant re-seals every sealed column under a new data
// key in one transaction; rotating a master is re-wrapping the tenant keys.
// Values sealed before tenant keys existed (format version 1, one
// deployment key) still open through Legacy until their tenant is rotated.

// versionTenantKey is the sealed format with a key id: version, the 16
// bytes of the tenant key id, the nonce, the ciphertext.
const versionTenantKey byte = 2

// Master wraps and unwraps tenant keys: a deployment key here, a KMS later.
type Master interface {
	ID() string
	Wrap(tenant uuid.UUID, key []byte) ([]byte, error)
	Unwrap(tenant uuid.UUID, wrapped []byte) ([]byte, error)
}

// LocalMaster is a master key held by the deployment (ERP_FIELD_KEYS).
type LocalMaster struct {
	id   string
	aead cipher.AEAD
}

// NewLocalMaster derives a wrapping key from a secret of at least 32 bytes.
func NewLocalMaster(id string, secret []byte) (*LocalMaster, error) {
	if id == "" || len(secret) < 32 {
		return nil, errors.New("fieldcrypt: a master key needs an id and a secret of at least 32 bytes")
	}
	block, _ := aes.NewCipher(derive(secret, "erp master key v1")) // a 32-byte key is always valid
	aead, _ := cipher.NewGCM(block)                                // AES always supports GCM
	return &LocalMaster{id: id, aead: aead}, nil
}

// ID implements Master.
func (m *LocalMaster) ID() string { return m.id }

// Wrap implements Master.
func (m *LocalMaster) Wrap(tenant uuid.UUID, key []byte) ([]byte, error) {
	nonce := make([]byte, m.aead.NonceSize())
	rand.Read(nonce) // never fails (crypto/rand)
	return m.aead.Seal(nonce, nonce, key, tenant[:]), nil
}

// Unwrap implements Master.
func (m *LocalMaster) Unwrap(tenant uuid.UUID, wrapped []byte) ([]byte, error) {
	n := m.aead.NonceSize()
	if len(wrapped) < n {
		return nil, ErrCorrupt
	}
	key, err := m.aead.Open(nil, wrapped[:n], wrapped[n:], tenant[:])
	if err != nil {
		return nil, ErrCorrupt
	}
	return key, nil
}

// Column is a sealed column a context owns: re-sealed when its tenant's key
// is rotated. Rows are addressed by the key columns; Fingerprint names the
// fingerprint column of the same value, when there is one; Versioned tables
// bump version on update (platform.touch).
type Column struct {
	Table       string
	Keys        []string // columns identifying a row within the tenant
	Cipher      string
	Fingerprint string
	Versioned   bool
}

var (
	columnsMu sync.Mutex
	columns   []Column
)

// Register declares a sealed column; contexts call it at init. Declaring
// the same column twice registers it once.
func Register(c Column) {
	columnsMu.Lock()
	defer columnsMu.Unlock()
	for _, k := range columns {
		if k.Table == c.Table && k.Cipher == c.Cipher {
			return
		}
	}
	columns = append(columns, c)
	sort.Slice(columns, func(i, j int) bool { return columns[i].Table+columns[i].Cipher < columns[j].Table+columns[j].Cipher })
}

// Columns lists the registered sealed columns.
func Columns() []Column {
	columnsMu.Lock()
	defer columnsMu.Unlock()
	return append([]Column(nil), columns...)
}

// tenantKey is an unwrapped tenant key.
type tenantKey struct {
	id     uuid.UUID
	aead   cipher.AEAD
	mac    []byte
	active bool
}

// Keyring seals with each tenant's own key and opens values sealed with any
// key it can unwrap, or with Legacy.
type Keyring struct {
	Masters []Master // the first wraps new tenant keys; all unwrap
	Legacy  *Keys    // opens and fingerprints for tenants that have no key yet; nil when there are none
	// Pool and AppRole load tenant keys outside a request (workers, tests);
	// inside one, the request's transaction is used.
	Pool    *database.Pool
	AppRole string

	mu     sync.Mutex
	active map[uuid.UUID]*tenantKey // by tenant
	known  map[uuid.UUID]*tenantKey // by key id
}

// ErrNoMaster means a tenant key is wrapped by a master this ring does not
// have (a key removed from ERP_FIELD_KEYS too early).
var ErrNoMaster = errors.New("fieldcrypt: the tenant key is wrapped by a master key this server does not have")

// ErrNoKey means the tenant has no key and the ring has no legacy key.
var ErrNoKey = errors.New("fieldcrypt: the tenant has no key")

func (r *Keyring) master(id string) (Master, bool) {
	for _, m := range r.Masters {
		if m.ID() == id {
			return m, true
		}
	}
	return nil, false
}

// unwrap turns a row of tenancy.tenant_key into keys in memory.
func (r *Keyring) unwrap(tenant, id uuid.UUID, masterID string, data, mac []byte, status string) (*tenantKey, error) {
	m, ok := r.master(masterID)
	if !ok {
		return nil, fmt.Errorf("%w (%s)", ErrNoMaster, masterID)
	}
	dk, err1 := m.Unwrap(tenant, data)
	mk, err2 := m.Unwrap(tenant, mac)
	if err := errors.Join(err1, err2); err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(dk) // 32 bytes, generated here
	aead, _ := cipher.NewGCM(block)
	return &tenantKey{id: id, aead: aead, mac: mk, active: status == "active"}, nil
}

// query runs fn with a transaction: the request's, or one of the pool's in
// the tenant's scope.
func (r *Keyring) query(ctx context.Context, tenant uuid.UUID, fn func(tx pgx.Tx) error) error {
	if tx, ok := reqctx.TxFrom(ctx); ok {
		return fn(tx)
	}
	if r.Pool == nil {
		return ErrNoKey
	}
	return r.Pool.InTx(ctx, database.Scope{Role: r.AppRole, TenantID: tenant, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error { return fn(tx) })
}

const keyColumns = `id, master_key_id, wrapped_data_key, wrapped_mac_key, status`

// load reads the tenant's keys and caches them; the active one is returned
// (nil when the tenant has none).
func (r *Keyring) load(ctx context.Context, tenant uuid.UUID) (*tenantKey, error) {
	var active *tenantKey
	err := r.query(ctx, tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT `+keyColumns+` FROM tenancy.tenant_key WHERE tenant_id = $1`, tenant)
		var id uuid.UUID
		var masterID, status string
		var data, mac []byte
		var unwrapErr error
		_, err := pgx.ForEachRow(rows, []any{&id, &masterID, &data, &mac, &status}, func() error {
			k, err := r.unwrap(tenant, id, masterID, data, mac, status)
			if err != nil {
				unwrapErr = fmt.Errorf("fieldcrypt: key %s of tenant %s: %w", id, tenant, err)
				return err
			}
			r.remember(tenant, k)
			if k.active {
				active = k
			}
			return nil
		})
		if unwrapErr != nil {
			return unwrapErr
		}
		if err != nil {
			return fmt.Errorf("fieldcrypt: read keys of tenant %s: %w", tenant, err)
		}
		return nil
	})
	if errors.Is(err, ErrNoKey) {
		return nil, nil // no way to look: a legacy-only ring
	}
	return active, err
}

func (r *Keyring) remember(tenant uuid.UUID, k *tenantKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.known == nil {
		r.known, r.active = map[uuid.UUID]*tenantKey{}, map[uuid.UUID]*tenantKey{}
	}
	r.known[k.id] = k
	if k.active {
		r.active[tenant] = k
	}
}

// current is the tenant's active key, from memory or the database; nil
// when the tenant has none (legacy).
func (r *Keyring) current(ctx context.Context, tenant uuid.UUID) (*tenantKey, error) {
	r.mu.Lock()
	k, ok := r.active[tenant]
	r.mu.Unlock()
	if ok {
		return k, nil
	}
	return r.load(ctx, tenant)
}

// byID finds a key by id, loading the tenant's keys when unknown.
func (r *Keyring) byID(ctx context.Context, tenant, id uuid.UUID) (*tenantKey, error) {
	r.mu.Lock()
	k, ok := r.known[id]
	r.mu.Unlock()
	if ok {
		return k, nil
	}
	if _, err := r.load(ctx, tenant); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if k, ok := r.known[id]; ok {
		return k, nil
	}
	return nil, ErrCorrupt
}

// Seal encrypts the normalized value with the tenant's key.
func (r *Keyring) Seal(ctx context.Context, tenant uuid.UUID, value string) ([]byte, error) {
	k, err := r.current(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if k == nil {
		if r.Legacy == nil {
			return nil, ErrNoKey
		}
		return r.Legacy.Seal(tenant, value), nil
	}
	return seal(k, tenant, value), nil
}

func seal(k *tenantKey, tenant uuid.UUID, value string) []byte {
	nonce := make([]byte, k.aead.NonceSize())
	rand.Read(nonce) // never fails (crypto/rand)
	out := append([]byte{versionTenantKey}, k.id[:]...)
	out = append(out, nonce...)
	return k.aead.Seal(out, nonce, []byte(Normalize(value)), tenant[:])
}

// Open decrypts a sealed value of the tenant, whichever key sealed it.
func (r *Keyring) Open(ctx context.Context, tenant uuid.UUID, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", ErrCorrupt
	}
	if sealed[0] == version {
		if r.Legacy == nil {
			return "", ErrCorrupt
		}
		return r.Legacy.Open(tenant, sealed)
	}
	if sealed[0] != versionTenantKey || len(sealed) < 1+16 {
		return "", ErrCorrupt
	}
	id, _ := uuid.FromBytes(sealed[1:17]) // 16 bytes always parse
	k, err := r.byID(ctx, tenant, id)
	if err != nil {
		return "", err
	}
	n := k.aead.NonceSize()
	if len(sealed) < 17+n {
		return "", ErrCorrupt
	}
	plain, err := k.aead.Open(nil, sealed[17:17+n], sealed[17+n:], tenant[:])
	if err != nil {
		return "", ErrCorrupt
	}
	return string(plain), nil
}

// Fingerprint is the keyed hash of the normalized value under the tenant's
// fingerprint key (Legacy's for tenants without a key).
func (r *Keyring) Fingerprint(ctx context.Context, tenant uuid.UUID, value string) (string, error) {
	k, err := r.current(ctx, tenant)
	if err != nil {
		return "", err
	}
	if k == nil {
		if r.Legacy == nil {
			return "", ErrNoKey
		}
		return r.Legacy.Fingerprint(tenant, value), nil
	}
	return fingerprint(k.mac, tenant, value), nil
}

func fingerprint(mac []byte, tenant uuid.UUID, value string) string {
	m := hmac.New(sha256.New, mac)
	m.Write(tenant[:])
	m.Write([]byte(Normalize(value)))
	return hex.EncodeToString(m.Sum(nil))
}

// generate makes and stores a new active tenant key in tx, retiring the
// current one, and returns it.
func (r *Keyring) generate(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (*tenantKey, error) {
	if len(r.Masters) == 0 {
		return nil, errors.New("fieldcrypt: no master key to wrap tenant keys with (ERP_FIELD_KEYS)")
	}
	data, mac := make([]byte, 32), make([]byte, 32)
	rand.Read(data) // never fails (crypto/rand)
	rand.Read(mac)
	primary := r.Masters[0]
	wrappedData, err := primary.Wrap(tenant, data)
	if err != nil {
		return nil, err
	}
	wrappedMac, err := primary.Wrap(tenant, mac)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tenancy.tenant_key SET status = 'retired', retired_at = now(), version = version + 1 WHERE tenant_id = $1 AND status = 'active'`, tenant); err != nil {
		return nil, fmt.Errorf("fieldcrypt: retire active key of tenant %s: %w", tenant, err)
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO tenancy.tenant_key (tenant_id, id, master_key_id, wrapped_data_key, wrapped_mac_key, status) VALUES ($1, $2, $3, $4, $5, 'active')`,
		tenant, id, primary.ID(), wrappedData, wrappedMac); err != nil {
		return nil, fmt.Errorf("fieldcrypt: insert key %s of tenant %s: %w", id, tenant, err)
	}
	block, _ := aes.NewCipher(data)
	aead, _ := cipher.NewGCM(block)
	return &tenantKey{id: id, aead: aead, mac: mac, active: true}, nil
}

// Provision gives a new tenant its key, in the provisioning transaction.
func (r *Keyring) Provision(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
	k, err := r.generate(ctx, tx, tenant)
	if err != nil {
		return err
	}
	r.remember(tenant, k)
	return nil
}

// Rotation is what a rotation did.
type Rotation struct {
	KeyID    uuid.UUID `json:"key_id"`
	Previous uuid.UUID `json:"previous_key_id"` // uuid.Nil when the tenant was on the legacy key
	Resealed int       `json:"resealed"`        // rows re-sealed
	Master   string    `json:"master_key_id"`
	At       time.Time `json:"rotated_at"`
}

// Rotate gives the tenant a new key and re-seals every registered sealed
// column under it, in tx (the tenant's scope, locked by the caller). The
// previous keys stay known, so values not yet re-sealed (none, after a
// successful rotation) still open.
func (r *Keyring) Rotate(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (Rotation, error) {
	ctx = reqctx.WithTx(ctx, tx) // lookups during the rotation use this transaction
	previous, err := r.current(ctx, tenant)
	if err != nil {
		return Rotation{}, err
	}
	k, err := r.generate(ctx, tx, tenant)
	if err != nil {
		return Rotation{}, err
	}
	rot := Rotation{KeyID: k.id, Master: r.Masters[0].ID(), At: time.Now().UTC()}
	if previous != nil {
		rot.Previous = previous.id
	}
	for _, c := range Columns() {
		n, err := r.reseal(ctx, tx, tenant, c, k)
		if err != nil {
			return Rotation{}, fmt.Errorf("fieldcrypt: re-sealing %s.%s: %w", c.Table, c.Cipher, err)
		}
		rot.Resealed += n
	}
	r.remember(tenant, k)
	return rot, nil
}

// reseal rewrites one column's rows under k.
func (r *Keyring) reseal(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, c Column, k *tenantKey) (int, error) {
	keys := strings.Join(c.Keys, ", ")
	rows, _ := tx.Query(ctx, `SELECT `+keys+`, `+c.Cipher+` FROM `+c.Table+` WHERE tenant_id = $1`, tenant)
	type row struct {
		keys   []any
		sealed []byte
	}
	var all []row
	ptrs := make([]any, len(c.Keys)+1)
	for i := range ptrs {
		ptrs[i] = new(any)
	}
	_, err := pgx.ForEachRow(rows, ptrs, func() error {
		rw := row{keys: make([]any, len(c.Keys))}
		for i := range c.Keys {
			rw.keys[i] = *(ptrs[i].(*any))
		}
		rw.sealed = append([]byte(nil), (*(ptrs[len(c.Keys)].(*any))).([]byte)...)
		all = append(all, rw)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("read rows: %w", err)
	}
	for _, rw := range all {
		plain, err := r.Open(ctx, tenant, rw.sealed)
		if err != nil {
			return 0, err
		}
		set := []string{c.Cipher + " = $1"}
		args := []any{seal(k, tenant, plain)}
		if c.Fingerprint != "" {
			set = append(set, fmt.Sprintf("%s = $%d", c.Fingerprint, len(args)+1))
			args = append(args, fingerprint(k.mac, tenant, plain))
		}
		if c.Versioned {
			set = append(set, "version = version + 1")
		}
		var where []string
		for i, name := range c.Keys {
			where = append(where, fmt.Sprintf("%s = $%d", name, len(args)+1))
			args = append(args, rw.keys[i])
		}
		if _, err := tx.Exec(ctx, `UPDATE `+c.Table+` SET `+strings.Join(set, ", ")+` WHERE tenant_id = $`+fmt.Sprint(len(args)+1)+` AND `+strings.Join(where, " AND "),
			append(args, tenant)...); err != nil {
			return 0, fmt.Errorf("update row %v: %w", rw.keys, err)
		}
	}
	return len(all), nil
}

// Rewrapped is what a re-wrap did.
type Rewrapped struct {
	Keys   int    `json:"keys"` // tenant keys now wrapped by Master that another master wrapped before
	Master string `json:"master_key_id"`
}

// Rewrap wraps every key of the tenant (active and retired) that another
// master wrapped under the ring's first master, in tx (the tenant's scope).
// The keys themselves do not change, so nothing is re-sealed: this is how
// tenant keys move onto a new master (a KMS put in front of the ring) and
// how the master it replaces leaves the ring.
func (r *Keyring) Rewrap(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) (Rewrapped, error) {
	if len(r.Masters) == 0 {
		return Rewrapped{}, errors.New("fieldcrypt: no master key to wrap tenant keys with (ERP_FIELD_KEYS or ERP_FIELD_KMS_KEY)")
	}
	primary := r.Masters[0]
	out := Rewrapped{Master: primary.ID()}
	rows, _ := tx.Query(ctx, `SELECT id, master_key_id, wrapped_data_key, wrapped_mac_key FROM tenancy.tenant_key WHERE tenant_id = $1 AND master_key_id <> $2 ORDER BY created_at FOR UPDATE`, tenant, primary.ID())
	type wrapped struct {
		id        uuid.UUID
		master    string
		data, mac []byte
	}
	var all []wrapped
	var w wrapped
	if _, err := pgx.ForEachRow(rows, []any{&w.id, &w.master, &w.data, &w.mac}, func() error {
		all = append(all, wrapped{w.id, w.master, append([]byte(nil), w.data...), append([]byte(nil), w.mac...)})
		return nil
	}); err != nil {
		return Rewrapped{}, fmt.Errorf("fieldcrypt: read keys of tenant %s to rewrap: %w", tenant, err)
	}
	for _, w := range all {
		m, ok := r.master(w.master)
		if !ok {
			return Rewrapped{}, fmt.Errorf("%w (%s)", ErrNoMaster, w.master)
		}
		dk, err1 := m.Unwrap(tenant, w.data)
		mk, err2 := m.Unwrap(tenant, w.mac)
		if err := errors.Join(err1, err2); err != nil {
			return Rewrapped{}, err
		}
		data, err := primary.Wrap(tenant, dk)
		if err != nil {
			return Rewrapped{}, err
		}
		mac, err := primary.Wrap(tenant, mk)
		if err != nil {
			return Rewrapped{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE tenancy.tenant_key SET master_key_id = $1, wrapped_data_key = $2, wrapped_mac_key = $3, version = version + 1 WHERE tenant_id = $4 AND id = $5`,
			primary.ID(), data, mac, tenant, w.id); err != nil {
			return Rewrapped{}, fmt.Errorf("fieldcrypt: rewrap key %s of tenant %s: %w", w.id, tenant, err)
		}
		out.Keys++
	}
	return out, nil
}
