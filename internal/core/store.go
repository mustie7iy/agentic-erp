package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/mustie7iy/agentic-erp/internal/workflow"
)

// Store is an in-memory system of record persisted as a JSON snapshot.
// Callers that share a Store across goroutines (e.g. agent tool handlers)
// must hold Mu for the duration of each operation.
type Store struct {
	Mu sync.Mutex `json:"-"`

	Seq            int64                        `json:"seq"`
	Companies      map[string]*Company          `json:"companies"`
	Accounts       map[string]*GLAccount        `json:"accounts"` // key: company/number
	Vendors        map[string]*Vendor           `json:"vendors"`
	PurchaseOrders map[string]*PurchaseOrder    `json:"purchaseOrders"`
	GoodsReceipts  map[string]*GoodsReceipt     `json:"goodsReceipts"`
	Invoices       map[string]*SupplierInvoice  `json:"invoices"`
	Journal        map[string]*JournalEntry     `json:"journal"`
	Tasks          map[string]*ReviewTask       `json:"tasks"`
	Requisitions   map[string]*Requisition      `json:"requisitions"`
	Contracts      map[string]*Contract         `json:"contracts"`
	MDChanges      map[string]*MasterDataChange `json:"masterDataChanges"`
	PaymentRuns    map[string]*PaymentRun       `json:"paymentRuns"`
	Workflows      map[string]*workflow.Request `json:"workflows"`
	Outputs        map[string]*OutputDocument   `json:"outputs"`

	index *index // derived lookups, never persisted
}

func NewStore() *Store {
	return &Store{
		Companies:      map[string]*Company{},
		Accounts:       map[string]*GLAccount{},
		Vendors:        map[string]*Vendor{},
		PurchaseOrders: map[string]*PurchaseOrder{},
		GoodsReceipts:  map[string]*GoodsReceipt{},
		Invoices:       map[string]*SupplierInvoice{},
		Journal:        map[string]*JournalEntry{},
		Tasks:          map[string]*ReviewTask{},
		Requisitions:   map[string]*Requisition{},
		Contracts:      map[string]*Contract{},
		MDChanges:      map[string]*MasterDataChange{},
		PaymentRuns:    map[string]*PaymentRun{},
		Workflows:      map[string]*workflow.Request{},
		Outputs:        map[string]*OutputDocument{},
	}
}

func accountKey(companyCode, number string) string { return companyCode + "/" + number }

func (s *Store) Account(companyCode, number string) *GLAccount {
	return s.Accounts[accountKey(companyCode, number)]
}

func (s *Store) PutAccount(a *GLAccount) { s.Accounts[accountKey(a.CompanyCode, a.Number)] = a }

func (s *Store) NextID(prefix string) string {
	s.Seq++
	return fmt.Sprintf("%s-%08d", prefix, s.Seq)
}

func (s *Store) ReceiptsFor(poID string) []*GoodsReceipt {
	var out []*GoodsReceipt
	for _, id := range s.idx().grByPO[poID] {
		if g := s.GoodsReceipts[id]; g != nil && g.POID == poID {
			out = append(out, g)
		}
	}
	return out
}

// SortedKeys gives deterministic iteration order over a map.
func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (s *Store) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ") // plain data: cannot fail
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LoadStore(path string) (*Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := NewStore()
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("load store %s: %w", path, err)
	}
	return s, nil
}
