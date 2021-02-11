// Copyright 2020 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sqle

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/store/types"
)

type lookupResult struct {
	r   sql.Row
	err error
}

// resultChanWithBacklog is used to receive lookup results. Unlike a normal channel it has a backlog which is written
// to when a channel's buffer is full and can't be written to immediately. It tracks the number of results it is expecting,
// whether any more lookups will be requested, and the number of results that have been written to it.
type resultChanWithBacklog struct {
	writeCount      uint64
	lookupCount     uint64
	isFullyEnqueued uint64

	mu         *sync.Mutex
	resChan    chan lookupResult
	backlog    []lookupResult
	backlogPos int
}

func newResultChanWithBacklog(resChanBuffSize, backlogSize int) *resultChanWithBacklog {
	return &resultChanWithBacklog{
		mu:      &sync.Mutex{},
		backlog: make([]lookupResult, 0, backlogSize),
		resChan: make(chan lookupResult, resChanBuffSize),
	}
}

// LookupEnqueued is called when a lookup is written to the global asyncLookup instance with a reference to this
// instance and a corresponding write should be expected. If this is the final lookup then `done` will be true
func (r *resultChanWithBacklog) LookupEnqueued(done bool) {
	atomic.AddUint64(&r.lookupCount, 1)

	if done {
		atomic.StoreUint64(&r.isFullyEnqueued, 1)
	}
}

// Write is ussed to send a lookup result to the channel.
func (r *resultChanWithBacklog) Write(result lookupResult) {
	// try to write a result to the result channel. If the channel cannot be written to immediately then write it to the backlog
	select {
	case r.resChan <- result:
	default:
		func() {
			r.mu.Lock()
			defer r.mu.Unlock()

			r.backlog = append(r.backlog, result)
		}()
	}

	written := atomic.AddUint64(&r.writeCount, 1)
	fullyEnqueued := atomic.LoadUint64(&r.isFullyEnqueued)

	if fullyEnqueued == 1 {
		lookupCount := atomic.LoadUint64(&r.lookupCount)

		if lookupCount == written {
			close(r.resChan)
		}
	}
}

// safeWrite recovers from panics and returns recoverevd objects
func (r *resultChanWithBacklog) safeWrite(result lookupResult) (recovered interface{}) {
	defer func() {
		recovered = recover()
	}()

	r.Write(result)
	return
}

// Read will read read the next lookupResult from the channel. When all results have been read returns io.EOF
func (r *resultChanWithBacklog) Read(ctx context.Context) (lookupResult, error) {
	// try to read a result from the result channel
	select {
	case res, ok := <-r.resChan:
		if ok {
			return res, nil
		}

		// !ok then the resChan has been closed and results need to be read from the backlog

	case <-ctx.Done():
		return lookupResult{}, ctx.Err()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.backlog) > r.backlogPos {
		res := r.backlog[r.backlogPos]
		r.backlogPos++
		return res, nil
	}

	return lookupResult{}, io.EOF
}

// toLookup represents an table lookup that should be performed by one of the global asyncLookups instance's worker routines
type toLookup struct {
	// key read f
	t          types.Tuple
	tupleToRow func(types.Tuple) (sql.Row, error)
	resChan    *resultChanWithBacklog
}

// a global asyncLookups struct handles all lookups
type asyncLookups struct {
	ctx        context.Context
	toLookupCh chan<- toLookup
}

// newAsyncLookups kicks off a number of worker routines and creates a channel for sending lookups to workers.  The
// routines live for the life of the program
func newAsyncLookups(numWorkers, bufferSize int) *asyncLookups {
	toLookupCh := make(chan toLookup, bufferSize)
	art := &asyncLookups{toLookupCh: toLookupCh}

	for i := 0; i < numWorkers; i++ {
		go func() {
			f := func() {
				var curr toLookup
				var ok bool

				defer func() {
					if r := recover(); r != nil {
						// Attempt to write a failure to the channel and discard any additional panics
						if err, ok := r.(error); ok {
							_ = curr.resChan.safeWrite(lookupResult{r: nil, err: err})
						} else {
							_ = curr.resChan.safeWrite(lookupResult{r: nil, err: fmt.Errorf("%v", r)})
						}
					}

					// if the channel used for lookups is closed then fail spectacularly
					if !ok {
						panic("toLookup channel closed.  All lookups will fail and will result in a deadlock")
					}
				}()

				for {
					curr, ok = <-toLookupCh

					if !ok {
						break
					}

					r, err := curr.tupleToRow(curr.t)
					curr.resChan.Write(lookupResult{r: r, err: err})
				}
			}

			// these routines will run forever unless f is allowed to panic which only happens when the lookup channel is closed
			for {
				f()
			}
		}()
	}

	return art
}

// lookups is a global asyncLookups instance which is used by the indexLookupRowIterAdapter
var lookups *asyncLookups

func init() {
	lookups = newAsyncLookups(runtime.NumCPU(), runtime.NumCPU()*256)
}
