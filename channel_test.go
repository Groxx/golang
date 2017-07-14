// Sanity checks to reveal / prove components of logic, like how channels behave.
// *Hopefully* anything reliable remains so when combined in other ways, but tests should generally be clear and fine-grained.
//
// If these fail, it likely means that Go was upgraded and has new behavior, or different platforms behave differently.
// PLEASE let me know if you encounter this!

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCancelStructuresUnreliableAcrossGoroutines(t *testing.T) {
	t.Parallel()

	// select constructions
	singleLevelSelect := func(ctx context.Context, jobs chan struct{}) bool {
		select {
		case <-jobs:
			return true
		case <-ctx.Done():
			return false
		}
	}
	nestedSelect := func(ctx context.Context, jobs chan struct{}) bool {
		select {
		case <-ctx.Done():
			return false
		default:
			// nested or following outside the select, makes no difference.
			// probably compiles to the same thing anyway.
			select {
			case <-jobs:
				return true
			case <-ctx.Done():
				return false
			}
		}
	}

	// unblock behaviors
	cancelFirst := func(cancel func(), jobs chan struct{}) {
		// looks kinda like it should work in some cases
		cancel()
		jobs <- struct{}{}
	}
	submitFirst := func(cancel func(), jobs chan struct{}) {
		// intentionally in a "that looks like it should fail" order
		jobs <- struct{}{}
		cancel()
	}

	tests := []struct {
		name           string
		inGoroutine    bool
		expectReliable bool
		selecter       func(ctx context.Context, jobs chan struct{}) bool
		runner         func(cancel func(), jobs chan struct{})
		tries          int
		retries        int
	}{
		// ---------------------------------------------------
		// Single-level selects are fairly reliably unreliable
		// ---------------------------------------------------
		{
			name:           "Single-level cancel is unreliable",
			inGoroutine:    true,
			expectReliable: false,
			selecter:       singleLevelSelect,
			runner:         submitFirst,
			tries:          10,
			retries:        10,
		},
		{
			name:           "Single-level cancel is unreliable, even when canceling first",
			inGoroutine:    true,
			expectReliable: false,
			selecter:       singleLevelSelect,
			runner:         cancelFirst,
			tries:          10,
			retries:        10,
		},
		// --------------------------------------------------------------------
		// Nested selects are less likely to misbehave, allow trying more times
		// --------------------------------------------------------------------
		{
			name:           "Nested cancel is unreliable",
			inGoroutine:    true,
			expectReliable: false,
			selecter:       nestedSelect,
			runner:         submitFirst,
			tries:          100,
			retries:        100,
		},
		// This one is kinda the nail in the coffin for nested selects.
		//
		// I suspect it'll come to be viewed as Go's double-checked-locking equivalent - a common construct that's
		// fatally flawed, but propagates EVERYWHERE.
		{
			name:           "Nested cancel is unreliable, even when canceling first",
			inGoroutine:    true,
			expectReliable: false,
			selecter:       nestedSelect,
			runner:         cancelFirst,
			tries:          100,
			retries:        100,
		},
		// -----------------------------------------------------------
		// Even on a single thread, single-level select is unreliable.
		// But that's not too surprising, given other tests in here.
		// -----------------------------------------------------------
		{
			name:           "Single-threaded single-level cancel is unreliable",
			inGoroutine:    false,
			expectReliable: false,
			selecter:       singleLevelSelect,
			runner:         submitFirst,
			tries:          10,
			retries:        10,
		},
		{
			name:           "Single-threaded single-level cancel is unreliable, even when canceling first",
			inGoroutine:    false,
			expectReliable: false,
			selecter:       singleLevelSelect,
			runner:         cancelFirst,
			tries:          10,
			retries:        10,
		},
		// ===================================================================
		// This is the ONLY reliable construct + use, but unfortunately single-
		// -threaded canceling and submitting is pretty unrealistic / useless.
		//
		// Note that because these are reliable, they will try `tries * retries`
		// times (stopping early if they are *not* reliable).  They do not
		// require too many.
		// ===================================================================
		{
			name:           "Single-threaded nested cancel is reliable, even when submitting first, though unrealistic",
			inGoroutine:    false,
			expectReliable: true,
			selecter:       nestedSelect,
			runner:         submitFirst,
			tries:          10,
			retries:        100,
		},
		{
			name:           "Single-threaded nested cancel is reliable, though unrealistic",
			inGoroutine:    false,
			expectReliable: true,
			selecter:       nestedSelect,
			runner:         cancelFirst,
			tries:          10,
			retries:        100,
		},
	}
	for _, tmp := range tests {
		test := tmp // copy, or it'll be shared between loops

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			a := assert.New(t)

			didWork := 0
			canceled := 0

			runFlaky(test.tries, test.retries, func() {
				jobs := make(chan struct{}, 1)
				result := make(chan bool, 1)

				ctx, cancel := context.WithCancel(context.Background())

				if test.inGoroutine {
					// start the goroutine first, more unpredictable results (and more real-world-y).
					// note that this is not actually required though, it fails in either order.
					go func() {
						result <- test.selecter(ctx, jobs)
					}()
					test.runner(cancel, jobs)
				} else {
					test.runner(cancel, jobs)
					result <- test.selecter(ctx, jobs)
				}

				if <-result {
					didWork += 1
				} else {
					canceled += 1
				}
			}, func() bool {
				// stop once we've shown both are possible (or when retries are exhausted)
				return didWork != 0 && canceled != 0
			})

			if test.expectReliable {
				a.Equal(0, didWork, "Should never pull from jobs")
				a.NotEqual(0, canceled, "Should always pull from Done")
			} else {
				a.NotEqual(0, didWork, "Should pull from jobs sometimes")
				a.NotEqual(0, canceled, "Should pull from Done sometimes")
			}
		})
	}
}

// Demonstrate that select-reads are indeterminate when multiple choices are valid.
func TestIndeterminateChanReads(t *testing.T) {
	t.Parallel()
	a := assert.New(t)

	bufferedWon := 0
	closedWon := 0

	runFlaky(10, 10, func() {
		buffered := make(chan bool, 1)
		buffered <- true

		closed := make(chan bool)
		close(closed)

		select {
		case <-buffered:
			bufferedWon += 1
		case <-closed:
			closedWon += 1
		}
	}, func() bool {
		// stop if none dominated && they're not identical (proving indeterminism and not just round-robin)
		return bufferedWon != 0 && closedWon != 0 && bufferedWon != closedWon
	})

	// assert that none won 100% of the time
	a.NotEqual(0, closedWon, "buffered does not usually always win, try rerunning")
	a.NotEqual(0, bufferedWon, "closed does not usually always win, try rerunning")
	// assert that they're not evenly matched (not totally useful, but is evidence for "real" indeterminism)
	a.NotEqual(bufferedWon, closedWon, "closed and buffered are not usually evenly matched, try rerunning")
}

// Demonstrate that read/write decision is indeterminate when both are possible
func TestIndeterminateChanReadWrite(t *testing.T) {
	t.Parallel()
	a := assert.New(t)

	readerWon := 0
	writerWon := 0

	runFlaky(10, 10, func() {
		reader := make(chan bool, 1)
		reader <- true
		writer := make(chan bool, 1)

		select {
		case <-reader:
			readerWon += 1
		case writer <- true:
			writerWon += 1
		}
	}, func() bool {
		// stop if none dominated && they're not identical (proving indeterminism and not just round-robin)
		return readerWon != 0 && writerWon != 0 && readerWon != writerWon
	})

	// assert that none won 100% of the time
	a.NotEqual(0, writerWon, "reader does not usually always win, try rerunning")
	a.NotEqual(0, readerWon, "writer does not usually always win, try rerunning")
	// assert that they're not evenly matched (not totally useful, but is evidence for "real" indeterminism)
	a.NotEqual(readerWon, writerWon, "reader and writer are not usually evenly matched, try rerunning")
}

// Demonstrate that read/write decision on one channel is indeterminate when both are possible on the same channel
func TestIndeterminateSameChanReadWrite(t *testing.T) {
	t.Parallel()
	a := assert.New(t)

	readWon := 0
	writeWon := 0

	runFlaky(10, 10, func() {
		c := make(chan bool, 2)
		c <- true

		select {
		case <-c:
			readWon += 1
		case c <- true:
			writeWon += 1
		}
	}, func() bool {
		// stop if none dominated && they're not identical (proving indeterminism and not just round-robin)
		return readWon != 0 && writeWon != 0 && readWon != writeWon
	})

	// assert that none won 100% of the time
	a.NotEqual(0, writeWon, "read does not usually always win, try rerunning")
	a.NotEqual(0, readWon, "write does not usually always win, try rerunning")
	// assert that they're not evenly matched (not totally useful, but is evidence for "real" indeterminism)
	a.NotEqual(readWon, writeWon, "read and write are not usually evenly matched, try rerunning")
}

func TestBufferedReadOverDefault(t *testing.T) {
	t.Parallel()
	a := assert.New(t)

	c := make(chan bool, 1)
	c <- true
	select {
	case <-c:
	default:
		a.Fail("Buffered channels should read before hitting default")
	}
}

func TestBufferedSendOverDefault(t *testing.T) {
	t.Parallel()
	a := assert.New(t)

	c := make(chan bool, 1)
	select {
	case c <- true:
	default:
		a.Fail("Buffered channels should send before hitting default")
	}
}

func TestClosedReadOverDefault(t *testing.T) {
	t.Parallel()
	a := assert.New(t)

	c := make(chan bool)
	close(c)
	select {
	case <-c:
	default:
		a.Fail("Closed channels should read before hitting default")
	}
}

// Seems obvious, but prove that a nested select works as advertised
func TestNestedSelect(t *testing.T) {
	t.Parallel()

	runner := func(done chan bool, work chan bool) int {
		select {
		case <-done:
			return 0
		default:
			select {
			case <-done:
				return 1
			case <-work:
				return 2
			}
		}
	}
	type testable struct {
		name     string
		top      chan bool
		bottom   chan bool
		expected int
		setup    func(t testable)
	}

	tests := []testable{
		{
			name:     "should read from top when top closed",
			top:      make(chan bool),
			bottom:   make(chan bool),
			expected: 0,
			setup: func(t testable) {
				close(t.top)
			},
		},
		{
			name:     "should read from bottom when bottom closed",
			top:      make(chan bool),
			bottom:   make(chan bool),
			expected: 2,
			setup: func(t testable) {
				close(t.bottom)
			},
		},
		{
			name:     "should read from top when both closed",
			top:      make(chan bool),
			bottom:   make(chan bool),
			expected: 0,
			setup: func(t testable) {
				close(t.top)
				close(t.bottom)
			},
		},
		// note that "top and bottom closed after entering default" is indeterminate,
		// covered in TestIndeterminateChanReads
	}

	for _, tmp := range tests {
		test := tmp // copy
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			a := assert.New(t)

			test.setup(test)
			get := runner(test.top, test.bottom)
			a.Equal(test.expected, get)
		})
	}
}

// Demonstrate that closing channels in an order does not imply an order on a parallel reader goroutine
func TestUnblockOrderNotPreserved(t *testing.T) {
	t.Parallel()

	// Makes sure all branches are chosen, despite "do" doing things in a specific order.
	runTest := func(t *testing.T, do func(first chan bool, second chan bool)) {
		t.Parallel()
		a := assert.New(t)

		choseFirst := 0
		choseSecond := 0

		runFlaky(100, 100, func() {
			result := make(chan bool, 1)

			first := make(chan bool, 1)
			second := make(chan bool, 1)

			go func() {
				select {
				case <-first:
					result <- true
				case <-second:
					result <- false
				}
			}()

			do(first, second)

			if <-result {
				choseFirst += 1
			} else {
				choseSecond += 1
			}

		}, func() bool {
			return choseFirst != 0 && choseSecond != 0
		})

		a.NotEqual(0, choseFirst, "First should be chosen some of the time, after %v attempts", choseSecond)
		a.NotEqual(0, choseSecond, "Second should be chosen some of the time, after %v attempts", choseFirst)
	}

	t.Run("Closing does not imply order", func(t *testing.T) {
		runTest(t, func(first chan bool, second chan bool) {
			close(first)
			close(second)
		})
	})
	t.Run("Sending does not imply order", func(t *testing.T) {
		runTest(t, func(first chan bool, second chan bool) {
			first <- true
			second <- true
		})
	})
}

// simple helper thing to run flaky + batched tests until the first time the flakiness subsides.
func runFlaky(tries, retries int, f func(), worked func() bool) {
	for r := 0; r < retries; r++ {
		for t := 0; t < tries; t++ {
			f()
		}
		if worked() {
			break
		}
	}
}
