package knowledge

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestGlobalBudgetAndSlots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "maintenance.sqlite")
	a, err := openMaintenancePath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.DB.Close()
	b, err := openMaintenancePath(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.DB.Close()
	now := time.Now()
	x, err := a.Reserve("a", "stub", "model", now)
	if err != nil {
		t.Fatal(err)
	}
	y, err := b.Reserve("b", "stub", "model", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reserve("c", "stub", "model", now); err != ErrCallSlots {
		t.Fatalf("slots: %v", err)
	}
	a.Finish(x, "failed")
	b.Finish(y, "completed")
	for i := 0; i < 38; i++ {
		id, err := a.Reserve("a", "stub", "model", now)
		if err != nil {
			t.Fatal(err)
		}
		a.Finish(id, "failed")
	}
	if _, err := b.Reserve("a", "stub", "model", now); err != ErrCallBudget {
		t.Fatalf("cap bypass: %v", err)
	}
	if _, err := b.Reserve("a", "stub", "model", now.Add(24*time.Hour+time.Second)); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentReservationsRespectCap(t *testing.T) {
	m, err := openMaintenancePath(filepath.Join(t.TempDir(), "m.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.DB.Close()
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := m.Reserve("r", "p", "m", now)
			if err == nil {
				m.Finish(id, "completed")
			}
		}()
	}
	wg.Wait()
	var n int
	m.DB.QueryRow(`SELECT COUNT(*) FROM knowledge_calls`).Scan(&n)
	if n > 40 {
		t.Fatalf("overspent %d", n)
	}
}
func TestMaintenanceLocalRefreshAndNoCallsWhenUnchanged(t *testing.T) {
	_, _, root := indexedRepo(t)
	m, err := openMaintenancePath(filepath.Join(t.TempDir(), "m.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.DB.Close()
	if err := m.Register(Registration{Root: root, Provider: "stub"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rs, _ := m.Registrations()
	if err := maintenanceTick(m, context.Background(), rs[0], now); err != nil {
		t.Fatal(err)
	}
	rs, _ = m.Registrations()
	if err := maintenanceTick(m, context.Background(), rs[0], now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	rs, _ = m.Registrations()
	if rs[0].Indexed == "" {
		t.Fatal("local refresh not recorded")
	}
	// No model dispatch before 60 seconds of quiet.
	if err := maintenanceTick(m, context.Background(), rs[0], now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	s, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if s.Used != 0 || s.Queued == 0 {
		t.Fatalf("unexpected status %+v", s)
	}
}
