package mysqlstore

import (
	"errors"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestRetryOnLockConflict(t *testing.T) {
	deadlock := &mysql.MySQLError{Number: mysqlDeadlock, Message: "Deadlock found"}
	lockWait := &mysql.MySQLError{Number: mysqlLockWaitTimeout, Message: "Lock wait timeout"}
	duplicate := &mysql.MySQLError{Number: mysqlDuplicateEntry, Message: "Duplicate entry"}
	other := errors.New("connection refused")

	tests := []struct {
		name      string
		errs      []error // returned by successive runs; nil afterwards
		wantRuns  int
		wantError error
	}{
		{name: "AC-R001-08 a deadlock is run again and then succeeds", errs: []error{deadlock}, wantRuns: 2},
		{name: "AC-R001-08 a lock wait timeout is run again and then succeeds", errs: []error{lockWait, deadlock}, wantRuns: 3},
		{name: "AC-R001-08 it gives up after three deadlocks", errs: []error{deadlock, deadlock, deadlock, deadlock}, wantRuns: 3, wantError: deadlock},
		{name: "other MySQL errors are not run again", errs: []error{duplicate}, wantRuns: 1, wantError: duplicate},
		{name: "other errors are not run again", errs: []error{other}, wantRuns: 1, wantError: other},
		{name: "success runs once", wantRuns: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs := 0
			err := retryOnLockConflict(func() error {
				runs++
				if runs <= len(tt.errs) {
					return tt.errs[runs-1]
				}
				return nil
			})
			if runs != tt.wantRuns {
				t.Errorf("runs = %d, want %d", runs, tt.wantRuns)
			}
			if !errors.Is(err, tt.wantError) {
				t.Errorf("err = %v, want %v", err, tt.wantError)
			}
		})
	}
}
