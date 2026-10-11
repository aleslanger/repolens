//go:build !unix

package collect

import "errors"

// ErrBusy means another collection holds the lock of the same database.
var ErrBusy = errors.New("jiný sběr do stejné databáze právě běží")

// Lock is a no-op where flock is unavailable; the dashboard still runs one collection at a time.
func Lock(string) (func(), error) { return func() {}, nil }
