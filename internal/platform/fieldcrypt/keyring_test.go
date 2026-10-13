package fieldcrypt_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt/kmstest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func master(t *testing.T, id, secret string) fieldcrypt.Master {
	t.Helper()
	m, err := fieldcrypt.NewLocalMaster(id, []byte(strings.Repeat(secret, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func ring(t *testing.T, masters ...fieldcrypt.Master) *fieldcrypt.Keyring {
	t.Helper()
	legacy, _ := fieldcrypt.New([]byte(strings.Repeat("f", 32)))
	return &fieldcrypt.Keyring{Masters: masters, Legacy: legacy, Pool: pgtest.Pool(t), AppRole: "erp_app"}
}

// inTenant runs fn in a transaction of the tenant's scope and commits.
func inTenant(t *testing.T, r *fieldcrypt.Keyring, tenant uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	if err := r.Pool.InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: tenant}, fn); err != nil {
		t.Fatal(err)
	}
}

func TestMasters(t *testing.T) {
	if _, err := fieldcrypt.NewLocalMaster("", []byte(strings.Repeat("a", 32))); err == nil {
		t.Fatal("master without id")
	}
	if _, err := fieldcrypt.NewLocalMaster("k", []byte("short")); err == nil {
		t.Fatal("master with a short secret")
	}
	m, other := master(t, "k1", "a"), master(t, "k2", "b")
	tenant := uuid.New()
	key := []byte(strings.Repeat("k", 32))
	wrapped, _ := m.Wrap(tenant, key)
	if got, err := m.Unwrap(tenant, wrapped); err != nil || string(got) != string(key) {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := m.Unwrap(uuid.New(), wrapped); err == nil {
		t.Fatal("another tenant unwrapped the key")
	}
	if _, err := other.Unwrap(tenant, wrapped); err == nil {
		t.Fatal("another master unwrapped the key")
	}
	if _, err := m.Unwrap(tenant, wrapped[:3]); err == nil {
		t.Fatal("truncated wrap unwrapped")
	}
}

func TestKeyringSealsWithTenantKeys(t *testing.T) {
	ctx := context.Background()
	r := ring(t, master(t, "k1", "a"))
	ten := pgtest.NewTenant(t)
	// A tenant without a key is on the legacy key: format version 1, legacy fingerprints.
	sealed, err := r.Seal(ctx, ten.ID, "de89 3704")
	if err != nil || sealed[0] != 1 {
		t.Fatalf("%v %v", sealed, err)
	}
	if v, err := r.Open(ctx, ten.ID, sealed); err != nil || v != "DE893704" {
		t.Fatalf("%q %v", v, err)
	}
	if p, err := r.Fingerprint(ctx, ten.ID, "DE893704"); err != nil || p != r.Legacy.Fingerprint(ten.ID, "DE893704") {
		t.Fatalf("%q %v", p, err)
	}
	// Provisioned a key, the tenant seals with it: format version 2 naming the key.
	inTenant(t, r, ten.ID, func(ctx context.Context, tx pgx.Tx) error { return r.Provision(ctx, tx, ten.ID) })
	sealed2, err := r.Seal(ctx, ten.ID, "DE893704")
	if err != nil || sealed2[0] != 2 || len(sealed2) < 17+12 {
		t.Fatalf("%v %v", sealed2, err)
	}
	if v, err := r.Open(ctx, ten.ID, sealed2); err != nil || v != "DE893704" {
		t.Fatalf("%q %v", v, err)
	}
	if v, err := r.Open(ctx, ten.ID, sealed); err != nil || v != "DE893704" {
		t.Fatalf("legacy value after the key: %q %v", v, err)
	}
	p2, _ := r.Fingerprint(ctx, ten.ID, "DE893704")
	if p2 == r.Legacy.Fingerprint(ten.ID, "DE893704") || len(p2) != 64 {
		t.Fatal("fingerprint still legacy")
	}
	// Another ring with the same master opens it (another replica); one without the master cannot.
	if v, err := ring(t, master(t, "k1", "a")).Open(ctx, ten.ID, sealed2); err != nil || v != "DE893704" {
		t.Fatalf("other replica: %q %v", v, err)
	}
	if _, err := ring(t, master(t, "k2", "b")).Open(ctx, ten.ID, sealed2); !errors.Is(err, fieldcrypt.ErrNoMaster) {
		t.Fatalf("without the master: %v", err)
	}
	// Corrupt and foreign values.
	for name, bad := range map[string][]byte{"empty": nil, "unknown version": {9, 1, 2}, "short": sealed2[:10], "short for nonce": sealed2[:20],
		"unknown key": append(append([]byte{2}, uuid.New().NodeID()...), sealed2[1:]...), "tampered": append([]byte(nil), sealed2[:len(sealed2)-1]...)} {
		if _, err := r.Open(ctx, ten.ID, bad); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	if _, err := r.Open(ctx, uuid.New(), sealed2); err == nil {
		t.Fatal("another tenant opened the value")
	}
	// Legacy values with a key id the tenant never had: ErrCorrupt.
	fake := append([]byte{2}, uuid.New().NodeID()...)
	if _, err := r.Open(ctx, ten.ID, append(fake, sealed2[17:]...)); !errors.Is(err, fieldcrypt.ErrCorrupt) {
		t.Fatalf("unknown key: %v", err)
	}
	// Inside a request the lookup uses the request's transaction.
	inTenant(t, r, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
		v, err := ring(t, master(t, "k1", "a")).Open(reqctx.WithTx(ctx, tx), ten.ID, sealed2)
		if err != nil || v != "DE893704" {
			t.Fatalf("in request: %q %v", v, err)
		}
		return nil
	})
	// A ring with neither legacy key nor a way to look up tenant keys has no key.
	bare := &fieldcrypt.Keyring{}
	if _, err := bare.Seal(ctx, ten.ID, "x"); !errors.Is(err, fieldcrypt.ErrNoKey) {
		t.Fatalf("bare seal: %v", err)
	}
	if _, err := bare.Fingerprint(ctx, ten.ID, "x"); !errors.Is(err, fieldcrypt.ErrNoKey) {
		t.Fatalf("bare fingerprint: %v", err)
	}
	if _, err := bare.Open(ctx, ten.ID, sealed); !errors.Is(err, fieldcrypt.ErrCorrupt) {
		t.Fatalf("bare open legacy: %v", err)
	}
	if _, err := (&fieldcrypt.Keyring{Legacy: r.Legacy}).Open(ctx, ten.ID, sealed2); err == nil {
		t.Fatal("legacy-only ring opened a tenant key value")
	}
	// A master with the right id but another secret cannot unwrap: corrupt, not missing.
	if _, err := ring(t, master(t, "k1", "z")).Open(ctx, ten.ID, sealed2); !errors.Is(err, fieldcrypt.ErrCorrupt) {
		t.Fatalf("wrong secret: %v", err)
	}
	// A ring that cannot load the tenant's key cannot seal or fingerprint for it either.
	no := ring(t, master(t, "k2", "b"))
	if _, err := no.Seal(ctx, ten.ID, "x"); !errors.Is(err, fieldcrypt.ErrNoMaster) {
		t.Fatalf("seal without the master: %v", err)
	}
	if _, err := no.Fingerprint(ctx, ten.ID, "x"); !errors.Is(err, fieldcrypt.ErrNoMaster) {
		t.Fatalf("fingerprint without the master: %v", err)
	}
	// Registering a column twice registers it once.
	before := len(fieldcrypt.Columns())
	fieldcrypt.Register(fieldcrypt.Column{Table: "payments.house_bank", Keys: []string{"id"}, Cipher: "iban_cipher", Versioned: true})
	if len(fieldcrypt.Columns()) != before {
		t.Fatal("registered twice")
	}
	// A master that cannot wrap stops provisioning, whichever of the two keys fails.
	for fail := 1; fail <= 2; fail++ {
		inTenant(t, r, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
			bad := &fieldcrypt.Keyring{Masters: []fieldcrypt.Master{&failingMaster{failAt: fail}}}
			if err := bad.Provision(ctx, tx, uuid.New()); err == nil || !strings.Contains(err.Error(), "hsm down") {
				t.Fatalf("wrap %d: %v", fail, err)
			}
			return nil
		})
	}
	// Provisioning a key needs a master.
	inTenant(t, r, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := (&fieldcrypt.Keyring{}).Provision(ctx, tx, ten.ID); err == nil {
			t.Fatal("provisioned without a master")
		}
		return nil
	})
}

// failingMaster fails its n-th wrap.
type failingMaster struct{ calls, failAt int }

func (f *failingMaster) ID() string { return "hsm" }
func (f *failingMaster) Wrap(uuid.UUID, []byte) ([]byte, error) {
	f.calls++
	if f.calls == f.failAt {
		return nil, errors.New("hsm down")
	}
	return []byte("wrapped"), nil
}
func (f *failingMaster) Unwrap(uuid.UUID, []byte) ([]byte, error) { return nil, errors.New("hsm down") }

func TestRotationResealsEveryColumn(t *testing.T) {
	ctx := context.Background()
	e := fixture.New(t, "payments.run.read") // a configured tenant: house banks sealed with the legacy key
	r := ring(t, master(t, "k1", "a"))
	// A tax identifier sealed and fingerprinted with the legacy key.
	legacyPrint := r.Legacy.Fingerprint(e.Tenant.ID, "DE123456789")
	pgtest.Exec(t, `INSERT INTO partner.tax_identifier (tenant_id, partner_id, scheme, value_cipher, value_fingerprint, value_last4, created_by, updated_by)
		VALUES ($1, $2, 'EU_VAT', $3, $4, '6789', $5, $5)`, e.Tenant.ID, e.Supplier, r.Legacy.Seal(e.Tenant.ID, "DE123456789"), legacyPrint, e.Tenant.Human.ID)
	// A ring without the legacy key cannot re-seal legacy values: the rotation fails and changes nothing.
	noLegacy := &fieldcrypt.Keyring{Masters: []fieldcrypt.Master{master(t, "k1", "a")}, Pool: r.Pool, AppRole: "erp_app"}
	err := noLegacy.Pool.InTx(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := noLegacy.Rotate(ctx, tx, e.Tenant.ID)
		return err
	})
	if !errors.Is(err, fieldcrypt.ErrCorrupt) {
		t.Fatalf("rotation without the legacy key: %v", err)
	}
	var before []byte
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT iban_cipher FROM payments.house_bank WHERE tenant_id = $1 LIMIT 1`, e.Tenant.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before[0] != 1 {
		t.Fatalf("house bank not on the legacy key: %v", before[0])
	}
	var rot fieldcrypt.Rotation
	inTenant(t, r, e.Tenant.ID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rot, err = r.Rotate(ctx, tx, e.Tenant.ID)
		return err
	})
	if rot.Previous != uuid.Nil || rot.Master != "k1" || rot.Resealed != 3 {
		t.Fatalf("%+v", rot)
	}
	// The tax identifier moved to the tenant's fingerprint key, so lookups by fingerprint keep working.
	var cipher []byte
	var print string
	var taxVersion int
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT value_cipher, value_fingerprint, version FROM partner.tax_identifier WHERE tenant_id = $1`, e.Tenant.ID).Scan(&cipher, &print, &taxVersion); err != nil {
		t.Fatal(err)
	}
	if want, _ := r.Fingerprint(ctx, e.Tenant.ID, "DE123456789"); print != want || print == legacyPrint || taxVersion != 2 {
		t.Fatalf("tax identifier after rotation: %s (legacy %s), version %d", print, legacyPrint, taxVersion)
	}
	if v, err := r.Open(ctx, e.Tenant.ID, cipher); err != nil || v != "DE123456789" {
		t.Fatalf("%q %v", v, err)
	}
	var after []byte
	var version int
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT iban_cipher, version FROM payments.house_bank WHERE tenant_id = $1 LIMIT 1`, e.Tenant.ID).Scan(&after, &version); err != nil {
		t.Fatal(err)
	}
	if after[0] != 2 || version != 2 {
		t.Fatalf("after: version byte %d, row version %d", after[0], version)
	}
	iban, err := r.Open(ctx, e.Tenant.ID, after)
	legacy, _ := r.Legacy.Open(e.Tenant.ID, before)
	if err != nil || iban != legacy {
		t.Fatalf("%q %q %v", iban, legacy, err)
	}
	// A second rotation retires the first key; values sealed with it still open.
	first := rot.KeyID
	inTenant(t, r, e.Tenant.ID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rot, err = r.Rotate(ctx, tx, e.Tenant.ID)
		return err
	})
	if rot.Previous != first || rot.KeyID == first {
		t.Fatalf("%+v", rot)
	}
	if v, err := ring(t, master(t, "k1", "a")).Open(ctx, e.Tenant.ID, after); err != nil || v != iban {
		t.Fatalf("retired key: %q %v", v, err)
	}
	var keys []string
	rows, _ := pgtest.Admin(t).Query(ctx, `SELECT status FROM tenancy.tenant_key WHERE tenant_id = $1 ORDER BY created_at`, e.Tenant.ID)
	keys, _ = pgx.CollectRows(rows, pgx.RowTo[string])
	if strings.Join(keys, ",") != "retired,active" {
		t.Fatalf("keys: %v", keys)
	}
	// Rotation under a new master: the old master still unwraps the retired key.
	r2 := ring(t, master(t, "k2", "b"), master(t, "k1", "a"))
	inTenant(t, r2, e.Tenant.ID, func(ctx context.Context, tx pgx.Tx) (err error) {
		rot, err = r2.Rotate(ctx, tx, e.Tenant.ID)
		return err
	})
	if rot.Master != "k2" {
		t.Fatalf("%+v", rot)
	}
	// A rotation that cannot open a value (a master missing) fails and changes nothing.
	only2 := ring(t, master(t, "k2", "b"))
	err = only2.Pool.InTx(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := only2.Rotate(ctx, tx, e.Tenant.ID)
		return err
	})
	if !errors.Is(err, fieldcrypt.ErrNoMaster) {
		t.Fatalf("rotation without the old master: %v", err)
	}
	// Each database statement of a rotation failing in turn is reported, never hidden.
	for n := 1; n < 60; n++ {
		fresh := ring(t, master(t, "k2", "b"), master(t, "k1", "a"))
		err := fresh.Pool.InTx(ctx, database.Scope{Role: "erp_app", TenantID: e.Tenant.ID}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := fresh.Rotate(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}, e.Tenant.ID)
			return err
		})
		if err == nil {
			if n == 1 {
				t.Fatal("rotated without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
}

func TestKMSMasterWrapsTenantKeys(t *testing.T) {
	ctx := context.Background()
	kms := kmstest.New(t)
	r := ring(t, kms.Master(""), master(t, "k1", "a"))
	ten := pgtest.NewTenant(t)
	inTenant(t, r, ten.ID, func(ctx context.Context, tx pgx.Tx) error { return r.Provision(ctx, tx, ten.ID) })
	var masterID string
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT master_key_id FROM tenancy.tenant_key WHERE tenant_id = $1`, ten.ID).Scan(&masterID); err != nil {
		t.Fatal(err)
	}
	if masterID != "kms" {
		t.Fatalf("wrapped by %q", masterID)
	}
	sealed, err := r.Seal(ctx, ten.ID, "DE893704")
	if err != nil || sealed[0] != 2 {
		t.Fatalf("%v %v", sealed, err)
	}
	if e, d := kms.Calls(); e != 2 || d != 0 {
		t.Fatalf("provisioning: %d encrypts, %d decrypts", e, d)
	}
	// Another replica unwraps the two keys once, then works from memory.
	other := ring(t, kms.Master(""))
	for i := 0; i < 3; i++ {
		if v, err := other.Open(ctx, ten.ID, sealed); err != nil || v != "DE893704" {
			t.Fatalf("%q %v", v, err)
		}
	}
	if _, err := other.Fingerprint(ctx, ten.ID, "DE893704"); err != nil {
		t.Fatal(err)
	}
	if e, d := kms.Calls(); e != 2 || d != 2 {
		t.Fatalf("after reads: %d encrypts, %d decrypts", e, d)
	}
	// A ring whose KMS refuses cannot load the tenant's keys.
	kms.Fail = 503
	if _, err := ring(t, kms.Master("")).Open(ctx, ten.ID, sealed); err == nil || !strings.Contains(err.Error(), "503 KMSInternalException") {
		t.Fatalf("kms down: %v", err)
	}
	kms.Fail = 0
}

func TestRewrap(t *testing.T) {
	ctx := context.Background()
	k1, k2 := master(t, "k1", "a"), master(t, "k2", "b")
	r := ring(t, k1)
	ten := pgtest.NewTenant(t)
	// Two keys wrapped by k1: the retired one and the active one.
	inTenant(t, r, ten.ID, func(ctx context.Context, tx pgx.Tx) error { return r.Provision(ctx, tx, ten.ID) })
	old, _ := r.Seal(ctx, ten.ID, "DE893704")
	inTenant(t, r, ten.ID, func(ctx context.Context, tx pgx.Tx) error { _, err := r.Rotate(ctx, tx, ten.ID); return err })
	sealed, _ := r.Seal(ctx, ten.ID, "DE893704")
	// A KMS master in front: both keys move onto it, unchanged.
	kms := kmstest.New(t)
	moved := ring(t, kms.Master(""), k1)
	var done fieldcrypt.Rewrapped
	inTenant(t, moved, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		done, err = moved.Rewrap(ctx, tx, ten.ID)
		return err
	})
	if done.Keys != 2 || done.Master != "kms" {
		t.Fatalf("%+v", done)
	}
	var n, bumped int
	if err := pgtest.Admin(t).QueryRow(ctx, `SELECT count(*) FILTER (WHERE master_key_id = 'kms'), count(*) FILTER (WHERE version > 1) FROM tenancy.tenant_key WHERE tenant_id = $1`, ten.ID).Scan(&n, &bumped); err != nil {
		t.Fatal(err)
	}
	if n != 2 || bumped != 2 {
		t.Fatalf("%d keys on kms, %d versions bumped", n, bumped)
	}
	// A ring with the KMS alone opens both the retired key's value and the active one's.
	alone := ring(t, kms.Master(""))
	for _, s := range [][]byte{old, sealed} {
		if v, err := alone.Open(ctx, ten.ID, s); err != nil || v != "DE893704" {
			t.Fatalf("%q %v", v, err)
		}
	}
	// Nothing left to move; without the master that wrapped the keys nothing can move.
	inTenant(t, alone, ten.ID, func(ctx context.Context, tx pgx.Tx) error {
		if done, err := alone.Rewrap(ctx, tx, ten.ID); err != nil || done.Keys != 0 {
			t.Fatalf("%+v %v", done, err)
		}
		if _, err := ring(t, k2).Rewrap(ctx, tx, ten.ID); !errors.Is(err, fieldcrypt.ErrNoMaster) {
			t.Fatalf("without the master: %v", err)
		}
		if _, err := (&fieldcrypt.Keyring{}).Rewrap(ctx, tx, ten.ID); err == nil {
			t.Fatal("without any master")
		}
		// A master that cannot wrap, whichever of the two keys fails.
		for fail := 1; fail <= 2; fail++ {
			bad := &fieldcrypt.Keyring{Masters: []fieldcrypt.Master{&failingMaster{failAt: fail}, kms.Master("")}}
			if _, err := bad.Rewrap(ctx, tx, ten.ID); err == nil || !strings.Contains(err.Error(), "hsm down") {
				t.Fatalf("wrap %d: %v", fail, err)
			}
		}
		return nil
	})
	// Each database statement failing in turn is reported.
	for n := 1; n < 10; n++ {
		fresh := ring(t, k2, kms.Master(""))
		err := fresh.Pool.InTx(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := fresh.Rewrap(ctx, &pgtest.FailingTx{Tx: tx, FailAt: n}, ten.ID)
			return err
		})
		if err == nil {
			if n == 1 {
				t.Fatal("re-wrapped without a statement to fail")
			}
			break
		}
		if !errors.Is(err, pgtest.ErrInjected) {
			t.Fatalf("statement %d: %v", n, err)
		}
	}
	// A key wrapped by a master that cannot unwrap stops the re-wrap.
	pgtest.Exec(t, `INSERT INTO tenancy.tenant_key (tenant_id, id, master_key_id, wrapped_data_key, wrapped_mac_key, status, retired_at)
		VALUES ($1, $2, 'hsm', 'x', 'y', 'retired', now())`, ten.ID, uuid.New())
	stuck := &fieldcrypt.Keyring{Masters: []fieldcrypt.Master{k2, kms.Master(""), &failingMaster{}}}
	err := r.Pool.InTx(ctx, database.Scope{Role: "erp_app", TenantID: ten.ID}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := stuck.Rewrap(ctx, tx, ten.ID)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "hsm down") {
		t.Fatalf("unwrap: %v", err)
	}
}
