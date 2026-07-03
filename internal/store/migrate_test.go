package store_test

import (
	"database/sql"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/store"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

var _ = Describe("schema migration", func() {
	// oldSchema is the pre-traceparent events table (no trace_id/span_id).
	const oldSchema = `CREATE TABLE events (
		request_id TEXT, ts_unix_ms INTEGER, developer_id TEXT, team TEXT,
		provider TEXT, model TEXT, stream INTEGER, status INTEGER,
		input_tokens INTEGER, output_tokens INTEGER, tokens_approximated INTEGER,
		cost_usd REAL, duration_ms INTEGER
	)`

	It("adds trace_id/span_id to a database created by an older version", func() {
		path := filepath.Join(GinkgoT().TempDir(), "old.db")

		// Create a legacy-schema database directly (the sqlite driver is already
		// registered by the store package under test).
		raw, err := sql.Open("sqlite", path)
		Expect(err).NotTo(HaveOccurred())
		_, err = raw.Exec(oldSchema)
		Expect(err).NotTo(HaveOccurred())
		Expect(raw.Close()).To(Succeed())

		// Opening via the store must migrate the table without error...
		st, err := store.Open(path)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = st.Close() }()

		// ...and a trace-carrying event must persist end-to-end.
		st.Emit(telemetry.Event{
			RequestID: "r1", TimestampUnixMs: time.Now().UnixMilli(),
			DeveloperID: "dev-1", Provider: "anthropic", Model: "claude-sonnet-4-5",
			Status: 200, CostUSD: 0.5,
			TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7",
		})

		verify, err := sql.Open("sqlite", path)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = verify.Close() }()
		var tid, sid string
		row := verify.QueryRow(`SELECT trace_id, span_id FROM events WHERE request_id = ?`, "r1")
		Expect(row.Scan(&tid, &sid)).To(Succeed())
		Expect(tid).To(Equal("4bf92f3577b34da6a3ce929d0e0e4736"))
		Expect(sid).To(Equal("00f067aa0ba902b7"))
	})

	It("is idempotent when the columns already exist", func() {
		path := filepath.Join(GinkgoT().TempDir(), "new.db")
		st1, err := store.Open(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(st1.Close()).To(Succeed())
		// Re-opening (columns already present) must not error.
		st2, err := store.Open(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(st2.Close()).To(Succeed())
	})
})
