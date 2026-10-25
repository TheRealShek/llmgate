package store

import (
	"strconv"
	"testing"
	"time"
)

// TestAccountingRetention checks bounded deletion preserves fresh usage, unfinished admissions, and API keys.
func TestAccountingRetention(t *testing.T) {
	pool := usageTestPool(t)

	key, _, err := NewKeyStore(pool).CreateKey(t.Context(), CreateKeyParams{Name: "retention", RequestsPerMin: 60, TokensPerDay: 1000})
	if err != nil {
		t.Fatal(err)
	}

	storage := NewUsageStore(pool)

	var records []AccountingRecord

	for i := range 5 {
		a := admissionRecord()
		a.RequestID = "retention-" + strconv.Itoa(i)

		a.KeyID = key.ID
		if i != 3 {
			a.StartedAt = time.Now().Add(-40 * 24 * time.Hour)
		}

		record := AccountingRecord{Admission: a}

		if i != 4 {
			u := usageRecord()
			u.RequestID, u.KeyID, u.Model, u.Streaming, u.StartedAt = a.RequestID, a.KeyID, a.Model, a.Streaming, a.StartedAt
			record.Usage = &u
		}

		records = append(records, record)
	}

	if err := storage.RecordBatch(t.Context(), records); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	for _, want := range []int64{2, 1, 0} {
		removed, err := storage.PruneAccounting(t.Context(), cutoff, 2)
		if err != nil || removed != want {
			t.Fatal("retention was unbounded or removed fresh data", removed, want, err)
		}
	}

	for table, want := range map[string]int{"usage": 1, "inference_admissions": 2, "api_keys": 1} {
		var count int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != want {
			t.Fatal(table, count, want, err)
		}
	}
}
