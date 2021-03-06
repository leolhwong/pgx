package pgx

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
)

type TxIsoLevel string

// Transaction isolation levels
const (
	Serializable    = TxIsoLevel("serializable")
	RepeatableRead  = TxIsoLevel("repeatable read")
	ReadCommitted   = TxIsoLevel("read committed")
	ReadUncommitted = TxIsoLevel("read uncommitted")
)

type TxAccessMode string

// Transaction access modes
const (
	ReadWrite = TxAccessMode("read write")
	ReadOnly  = TxAccessMode("read only")
)

type TxDeferrableMode string

// Transaction deferrable modes
const (
	Deferrable    = TxDeferrableMode("deferrable")
	NotDeferrable = TxDeferrableMode("not deferrable")
)

const (
	TxStatusInProgress      = 0
	TxStatusCommitFailure   = -1
	TxStatusRollbackFailure = -2
	TxStatusCommitSuccess   = 1
	TxStatusRollbackSuccess = 2
)

type TxOptions struct {
	IsoLevel       TxIsoLevel
	AccessMode     TxAccessMode
	DeferrableMode TxDeferrableMode
}

func (txOptions *TxOptions) beginSQL() string {
	if txOptions == nil {
		return "begin"
	}

	buf := &bytes.Buffer{}
	buf.WriteString("begin")
	if txOptions.IsoLevel != "" {
		fmt.Fprintf(buf, " isolation level %s", txOptions.IsoLevel)
	}
	if txOptions.AccessMode != "" {
		fmt.Fprintf(buf, " %s", txOptions.AccessMode)
	}
	if txOptions.DeferrableMode != "" {
		fmt.Fprintf(buf, " %s", txOptions.DeferrableMode)
	}

	return buf.String()
}

var ErrTxClosed = errors.New("tx is closed")

// ErrTxCommitRollback occurs when an error has occurred in a transaction and
// Commit() is called. PostgreSQL accepts COMMIT on aborted transactions, but
// it is treated as ROLLBACK.
var ErrTxCommitRollback = errors.New("commit unexpectedly resulted in rollback")

// Begin starts a transaction with the default transaction mode for the
// current connection. To use a specific transaction mode see BeginEx.
func (c *Conn) Begin() (*Tx, error) {
	return c.BeginEx(context.Background(), nil)
}

// BeginEx starts a transaction with txOptions determining the transaction
// mode. Unlike database/sql, the context only affects the begin command. i.e.
// there is no auto-rollback on context cancelation.
func (c *Conn) BeginEx(ctx context.Context, txOptions *TxOptions) (*Tx, error) {
	_, err := c.ExecEx(ctx, txOptions.beginSQL(), nil)
	if err != nil {
		// begin should never fail unless there is an underlying connection issue or
		// a context timeout. In either case, the connection is possibly broken.
		c.die(errors.New("failed to begin transaction"))
		return nil, err
	}

	tx := &Tx{conn: c, LocalStore: make(map[string]interface{})}
	c.tx = tx
	return tx, nil
}

// Tx represents a database transaction.
//
// All Tx methods return ErrTxClosed if Commit or Rollback has already been
// called on the Tx.
type Tx struct {
	conn         *Conn
	connPool     *ConnPool
	err          error
	status       int8
	beforeCommit func(*Tx)
	afterClose   func(*Tx)
	LocalStore   map[string]interface{} // transaction based storage in case we need to store local states regarding the transaction
}

// Commit commits the transaction
func (tx *Tx) Commit() error {
	return tx.CommitEx(context.Background())
}

// CommitEx commits the transaction with a context.
func (tx *Tx) CommitEx(ctx context.Context) error {
	if tx.status != TxStatusInProgress {
		return ErrTxClosed
	}

	if tx.beforeCommit != nil {
		tx.beforeCommit(tx)
	}

	commandTag, err := tx.conn.ExecEx(ctx, "commit", nil)
	if err == nil && commandTag == "COMMIT" {
		tx.status = TxStatusCommitSuccess
	} else if err == nil && commandTag == "ROLLBACK" {
		tx.status = TxStatusCommitFailure
		tx.err = ErrTxCommitRollback
	} else {
		tx.status = TxStatusCommitFailure
		tx.err = err
		// A commit failure leaves the connection in an undefined state
		tx.conn.die(errors.New("commit failed"))
	}

	if tx.connPool != nil {
		tx.connPool.Release(tx.conn)
	}

	if tx.afterClose != nil {
		tx.afterClose(tx)
	}
	tx.conn.tx = nil // whether it succeeded or not, remove tx from conn's reference
	return tx.err
}

// Rollback rolls back the transaction. Rollback will return ErrTxClosed if the
// Tx is already closed, but is otherwise safe to call multiple times. Hence, a
// defer tx.Rollback() is safe even if tx.Commit() will be called first in a
// non-error condition.
func (tx *Tx) Rollback() error {
	if tx.status != TxStatusInProgress {
		return ErrTxClosed
	}

	ctx, _ := context.WithTimeout(context.Background(), 15*time.Second)
	_, tx.err = tx.conn.ExecEx(ctx, "rollback", nil)
	if tx.err == nil {
		tx.status = TxStatusRollbackSuccess
	} else {
		tx.status = TxStatusRollbackFailure
		// A rollback failure leaves the connection in an undefined state
		tx.conn.die(errors.New("rollback failed"))
	}

	if tx.connPool != nil {
		tx.connPool.Release(tx.conn)
	}

	if tx.afterClose != nil {
		tx.afterClose(tx)
	}
	tx.conn.tx = nil // whether it succeeded or not, remove tx from conn's reference
	return tx.err
}

// Exec delegates to the underlying *Conn
func (tx *Tx) Exec(sql string, arguments ...interface{}) (commandTag CommandTag, err error) {
	if tx.status != TxStatusInProgress {
		return CommandTag(""), ErrTxClosed
	}

	return tx.conn.Exec(sql, arguments...)
}

// Prepare delegates to the underlying *Conn
func (tx *Tx) Prepare(name, sql string) (*PreparedStatement, error) {
	return tx.PrepareEx(context.Background(), name, sql, nil)
}

// PrepareEx delegates to the underlying *Conn
func (tx *Tx) PrepareEx(ctx context.Context, name, sql string, opts *PrepareExOptions) (*PreparedStatement, error) {
	if tx.status != TxStatusInProgress {
		return nil, ErrTxClosed
	}

	return tx.conn.PrepareEx(ctx, name, sql, opts)
}

// Query delegates to the underlying *Conn
func (tx *Tx) Query(sql string, args ...interface{}) (*Rows, error) {
	if tx.status != TxStatusInProgress {
		// Because checking for errors can be deferred to the *Rows, build one with the error
		err := ErrTxClosed
		return &Rows{closed: true, err: err}, err
	}

	return tx.conn.Query(sql, args...)
}

func (tx *Tx) QueryWithBufferSize(bufferSize int, sql string, args ...interface{}) (*Rows, error) {
	if tx.status != TxStatusInProgress {
		// Because checking for errors can be deferred to the *Rows, build one with the error
		err := ErrTxClosed
		return &Rows{closed: true, err: err}, err
	}

	return tx.conn.QueryWithBufferSize(bufferSize, sql, args...)
}

// QueryRow delegates to the underlying *Conn
func (tx *Tx) QueryRow(sql string, args ...interface{}) *Row {
	rows, _ := tx.Query(sql, args...)
	return (*Row)(rows)
}

// CopyFrom delegates to the underlying *Conn
func (tx *Tx) CopyFrom(tableName Identifier, columnNames []string, rowSrc CopyFromSource) (int, error) {
	if tx.status != TxStatusInProgress {
		return 0, ErrTxClosed
	}

	return tx.conn.CopyFrom(tableName, columnNames, rowSrc)
}

// Status returns the status of the transaction from the set of
// pgx.TxStatus* constants.
func (tx *Tx) Status() int8 {
	return tx.status
}

// Err returns the final error state, if any, of calling Commit or Rollback.
func (tx *Tx) Err() error {
	return tx.err
}

// BeforeCommit adds f to a LIFO queue of functions that will be called when
// just before commit is executed via the Commit function
func (tx *Tx) BeforeCommit(f func(*Tx)) {
	if tx.beforeCommit == nil {
		tx.beforeCommit = f
	} else {
		prevFn := tx.beforeCommit
		tx.beforeCommit = func(tx *Tx) {
			f(tx)
			prevFn(tx)
		}
	}
}

// AfterClose adds f to a LIFO queue of functions that will be called when
// the transaction is closed (either Commit or Rollback).
func (tx *Tx) AfterClose(f func(*Tx)) {
	if tx.afterClose == nil {
		tx.afterClose = f
	} else {
		prevFn := tx.afterClose
		tx.afterClose = func(tx *Tx) {
			f(tx)
			prevFn(tx)
		}
	}
}
