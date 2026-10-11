//go:build unix

package collect

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLockIsExclusiveUntilReleased(t *testing.T) {
	db := filepath.Join(t.TempDir(), "x.db")
	unlock, err := Lock(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(db); !errors.Is(err, ErrBusy) {
		t.Fatalf("second lock err = %v, want ErrBusy", err)
	}
	unlock()
	again, err := Lock(db)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}
