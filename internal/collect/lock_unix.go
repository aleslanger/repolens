//go:build unix

package collect

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrBusy means another collection holds the lock of the same database.
var ErrBusy = errors.New("jiný sběr do stejné databáze právě běží")

// Lock takes an exclusive, non-blocking lock next to the database so the CLI, the timer and the
// server never collect into one database at once. The lock dies with the process.
func Lock(dbPath string) (unlock func(), err error) {
	f, err := os.OpenFile(dbPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("lock: %w", err)
	}
	// Closing the descriptor releases the lock.
	return func() { f.Close() }, nil
}
