package simulator

import (
	"bufio"
	"os"

	"github.com/oasislabs/ekiden/go/scheduler/alg"
)

// LoggingTransactionSource implements the TransactionSource interface and wraps another
// TransactionSource interface.  All requests are passed into the underlying TransactionSource,
// and generated output is logged to a file, for replay later via FileTransactionSource or for
// analysis.
type LoggingTransactionSource struct {
	os *os.File
	bw *bufio.Writer
	ts TransactionSource
}

// NewLoggingTransactionSource is the factory function that constructs a new
// LoggingTransactionSource object.  The transactions generated by the wrapped
// TransactionSource in the formal parameter ts are logged into the file named by the fn formal
// parameter.
func NewLoggingTransactionSource(fn string, ts TransactionSource) (*LoggingTransactionSource, error) {
	// Mode 0666 is safe for logged synthetic transactions; no need for 0600 or less, but
	// gosec complains.  We do not typically have multiple accounts on developer machines,
	// so it's not a big deal to force people to chmod later.
	os, err := os.OpenFile(fn, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &LoggingTransactionSource{os: os, bw: bufio.NewWriter(os), ts: ts}, nil
}

// Get returns the next transaction from the wrapped TransactionSource and, if non-nil, logs it
// before returning it.
func (lts *LoggingTransactionSource) Get(seqno uint) (*alg.Transaction, error) {
	t, e := lts.ts.Get(seqno)
	if e == nil {
		t.Write(lts.bw)
		// We ignore errors from WriteRune, since the convention is to check bw.Flush()
		// -- t.Write may have already ignored an error.  If we returned the error
		// here, then the caller will have to distinguish errors from the logger versus
		// errors from the wrapped TransactionSource.
		_, _ = lts.bw.WriteRune('\n')
	}
	return t, e
}

// Close closes the wrapped TransactionSource, flushes the buffers used to write to the logging
// output file, and closes that file.
func (lts *LoggingTransactionSource) Close() error {
	if err := lts.ts.Close(); err != nil {
		return err
	}
	if err := lts.bw.Flush(); err != nil {
		return err
	}
	if err := lts.os.Close(); err != nil {
		return err
	}
	return nil
}