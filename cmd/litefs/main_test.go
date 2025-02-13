// go:build linux
package main_test

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	main "github.com/superfly/litefs/cmd/litefs"
	"github.com/superfly/litefs/internal/testingutil"
	"gopkg.in/yaml.v3"
)

var debug = flag.Bool("debug", false, "enable fuse debugging")

func init() {
	log.SetFlags(0)
	rand.Seed(time.Now().UnixNano())
}

func TestSingleNode(t *testing.T) {
	m0 := newRunningMain(t, t.TempDir(), nil)
	db := testingutil.OpenSQLDB(t, filepath.Join(m0.Config.MountDir, "db"))

	// Create a simple table with a single value.
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if _, err := db.Exec(`INSERT INTO t VALUES (100)`); err != nil {
		t.Fatal(err)
	}

	// Ensure we can retrieve the data back from the database.
	var x int
	if err := db.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil {
		t.Fatal(err)
	} else if got, want := x, 100; got != want {
		t.Fatalf("x=%d, want %d", got, want)
	}
}

func TestMultiNode_Simple(t *testing.T) {
	m0 := newRunningMain(t, t.TempDir(), nil)
	waitForPrimary(t, m0)
	m1 := newRunningMain(t, t.TempDir(), m0)
	db0 := testingutil.OpenSQLDB(t, filepath.Join(m0.Config.MountDir, "db"))
	db1 := testingutil.OpenSQLDB(t, filepath.Join(m1.Config.MountDir, "db"))

	// Create a simple table with a single value.
	if _, err := db0.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if _, err := db0.Exec(`INSERT INTO t VALUES (100)`); err != nil {
		t.Fatal(err)
	}

	var x int
	if err := db0.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil {
		t.Fatal(err)
	} else if got, want := x, 100; got != want {
		t.Fatalf("x=%d, want %d", got, want)
	}

	// Ensure we can retrieve the data back from the database on the second node.
	waitForSync(t, 1, m0, m1)
	if err := db1.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil {
		t.Fatal(err)
	} else if got, want := x, 100; got != want {
		t.Fatalf("x=%d, want %d", got, want)
	}

	// Write another value.
	if _, err := db0.Exec(`INSERT INTO t VALUES (200)`); err != nil {
		t.Fatal(err)
	}

	// Ensure it invalidates the page on the secondary.
	waitForSync(t, 1, m0, m1)
	if err := db1.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&x); err != nil {
		t.Fatal(err)
	} else if got, want := x, 2; got != want {
		t.Fatalf("count=%d, want %d", got, want)
	}
}

func TestMultiNode_ForcedReelection(t *testing.T) {
	m0 := newRunningMain(t, t.TempDir(), nil)
	waitForPrimary(t, m0)
	m1 := newRunningMain(t, t.TempDir(), m0)
	db0 := testingutil.OpenSQLDB(t, filepath.Join(m0.Config.MountDir, "db"))
	db1 := testingutil.OpenSQLDB(t, filepath.Join(m1.Config.MountDir, "db"))

	// Create a simple table with a single value.
	if _, err := db0.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if _, err := db0.Exec(`INSERT INTO t VALUES (100)`); err != nil {
		t.Fatal(err)
	}

	// Wait for sync first.
	waitForSync(t, 1, m0, m1)
	var x int
	if err := db1.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil {
		t.Fatal(err)
	} else if got, want := x, 100; got != want {
		t.Fatalf("x=%d, want %d", got, want)
	}

	// Stop the primary and wait for handoff to secondary.
	t.Log("shutting down primary node")
	if err := db0.Close(); err != nil {
		t.Fatal(err)
	} else if err := m0.Close(); err != nil {
		t.Fatal(err)
	}

	// Second node should eventually become primary.
	t.Log("waiting for promotion of replica")
	waitForPrimary(t, m1)

	// Update record on the new primary.
	t.Log("updating record on new primary")
	if _, err := db1.Exec(`UPDATE t SET x = 200`); err != nil {
		t.Fatal(err)
	}

	// Reopen first node and ensure it propagates changes.
	t.Log("restarting first node as replica")
	m0 = newRunningMain(t, m0.Config.MountDir, m1)
	waitForSync(t, 1, m0, m1)

	db0 = testingutil.OpenSQLDB(t, filepath.Join(m0.Config.MountDir, "db"))

	t.Log("verifying propagation of record update")
	if err := db0.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil {
		t.Fatal(err)
	} else if got, want := x, 200; got != want {
		t.Fatalf("x=%d, want %d", got, want)
	}
}

func TestMultiNode_EnsureReadOnlyReplica(t *testing.T) {
	m0 := newRunningMain(t, t.TempDir(), nil)
	waitForPrimary(t, m0)
	m1 := newRunningMain(t, t.TempDir(), m0)
	db0 := testingutil.OpenSQLDB(t, filepath.Join(m0.Config.MountDir, "db"))
	db1 := testingutil.OpenSQLDB(t, filepath.Join(m1.Config.MountDir, "db"))

	// Create a simple table with a single value.
	if _, err := db0.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	} else if _, err := db0.Exec(`INSERT INTO t VALUES (100)`); err != nil {
		t.Fatal(err)
	}

	// Ensure we cannot write to the replica.
	waitForSync(t, 1, m0, m1)
	if _, err := db1.Exec(`INSERT INTO t VALUES (200)`); err == nil || err.Error() != `unable to open database file: no such file or directory` {
		t.Fatalf("unexpected error: %s", err)
	}
}

//go:embed etc/litefs.yml
var litefsConfig []byte

//
func TestConfigExample(t *testing.T) {
	config := main.NewConfig()
	if err := yaml.Unmarshal(litefsConfig, &config); err != nil {
		t.Fatal(err)
	}
	if got, want := config.MountDir, "/path/to/mnt"; got != want {
		t.Fatalf("MountDir=%s, want %s", got, want)
	}
	if got, want := config.Debug, false; got != want {
		t.Fatalf("Debug=%v, want %v", got, want)
	}
	if got, want := config.HTTP.Addr, ":20202"; got != want {
		t.Fatalf("HTTP.Addr=%s, want %s", got, want)
	}
	if got, want := config.Consul.URL, "http://localhost:8500"; got != want {
		t.Fatalf("Consul.URL=%s, want %s", got, want)
	}
	if got, want := config.Consul.Key, "litefs/primary"; got != want {
		t.Fatalf("Consul.Key=%s, want %s", got, want)
	}
	if got, want := config.Consul.TTL, 10*time.Second; got != want {
		t.Fatalf("Consul.TTL=%s, want %s", got, want)
	}
	if got, want := config.Consul.LockDelay, 5*time.Second; got != want {
		t.Fatalf("Consul.LockDelay=%s, want %s", got, want)
	}
}

func newMain(tb testing.TB, mountDir string, peer *main.Main) *main.Main {
	tb.Helper()

	tb.Cleanup(func() {
		if err := os.RemoveAll(mountDir); err != nil {
			tb.Fatalf("cannot remove temp directory: %s", err)
		}
	})

	m := main.NewMain()
	m.Config.MountDir = mountDir
	m.Config.Debug = *debug
	m.Config.HTTP.Addr = ":0"
	m.Config.Consul.URL = "http://localhost:8500"
	m.Config.Consul.TTL = 2 * time.Second
	m.Config.Consul.LockDelay = 1 * time.Second

	// Use peer's consul key, if passed in. Otherwise generate one.
	if peer != nil {
		m.Config.Consul.Key = peer.Config.Consul.Key
	} else {
		m.Config.Consul.Key = fmt.Sprintf("%x", rand.Int31())
	}

	// Generate URL from HTTP server after port is assigned.
	m.AdvertiseURLFn = func() string {
		return fmt.Sprintf("http://localhost:%d", m.HTTPServer.Port())
	}

	return m
}

func newRunningMain(tb testing.TB, mountDir string, peer *main.Main) *main.Main {
	tb.Helper()

	m := newMain(tb, mountDir, peer)
	if err := m.Run(context.Background()); err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() {
		if err := m.Close(); err != nil {
			log.Printf("cannot close main: %s", err)
		}
	})

	return m
}

// waitForPrimary waits for m to obtain the primary lease.
func waitForPrimary(tb testing.TB, m *main.Main) {
	tb.Helper()
	tb.Logf("waiting for primary...")
	testingutil.RetryUntil(tb, 1*time.Millisecond, 30*time.Second, func() error {
		if !m.Store.IsPrimary() {
			return fmt.Errorf("not primary")
		}
		return nil
	})
}

// waitForSync waits for all processes to sync to the same TXID.
func waitForSync(tb testing.TB, dbID uint32, mains ...*main.Main) {
	tb.Helper()
	testingutil.RetryUntil(tb, 1*time.Millisecond, 30*time.Second, func() error {
		db0 := mains[0].Store.DB(dbID)
		if db0 == nil {
			return fmt.Errorf("no database on main[0]")
		}

		txID := db0.TXID()
		for i, m := range mains {
			db := m.Store.DB(dbID)
			if db == nil {
				return fmt.Errorf("no database on main[%d]", i)
			}

			if got, want := db.TXID(), txID; got != want {
				return fmt.Errorf("waiting for sync on db(%d): [%d,%d]", dbID, got, want)
			}
		}

		tb.Logf("%d processes synced for db %d at tx %d", len(mains), dbID, txID)
		return nil
	})
}
