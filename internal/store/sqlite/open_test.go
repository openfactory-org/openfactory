package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestConcurrentFirstOpen opens a new database file from several goroutines
// at once; every open must succeed (the switch to WAL can report SQLITE_BUSY).
func TestConcurrentFirstOpen(t *testing.T) {
	for round := 0; round < 10; round++ {
		path := filepath.Join(t.TempDir(), "openfactory.db")
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, err := OpenStore(context.Background(), path)
				if err != nil {
					t.Errorf("round %d: concurrent first open: %v", round, err)
					return
				}
				_ = s.Close()
			}()
		}
		wg.Wait()
	}
}
