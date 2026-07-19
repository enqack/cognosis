package write

import "sync"

// PathLocks serializes writes to one vault path across every writer in the
// process.
//
// It is shared rather than owned by the Pipeline because the Pipeline is not
// the only writer: lifecycle.Engine.rewrite writes vault files directly. That
// was harmless while every writer was single-shot — a caller supplying whole
// content has nothing that can go stale. Pipeline.Edit is the first
// read-modify-write, and it made the gap load-bearing:
//
//	Edit reads notes/foo.md at T0, compile_lifecycle reinforces the same note
//	and writes at T1, Edit writes its modified copy of the T0 bytes at T2.
//
// The reinforce is silently reverted. `confidence` and `last_reinforced` are
// canonical frontmatter, so that is a lost update to the source of truth, not
// index skew — and reconciliation cannot repair it, because the file and the
// index agree on the wrong value. Both tools report success.
//
// The zero value is not usable; construct with NewPathLocks and give the same
// instance to every writer.
type PathLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewPathLocks() *PathLocks {
	return &PathLocks{locks: map[string]*sync.Mutex{}}
}

// Lock acquires the lock for one vault-relative path and returns its release.
// Different paths proceed independently.
func (p *PathLocks) Lock(rel string) func() {
	l := p.lockFor(rel)
	l.Lock()
	return l.Unlock
}

// LockTwo acquires both paths in a fixed global order and returns their
// release. Ordering by path is what makes a two-path operation (a stage move,
// which writes the destination and deletes the source) safe against another
// one running the opposite direction — locking in call order would deadlock
// the pair.
func (p *PathLocks) LockTwo(a, b string) func() {
	if a == b {
		return p.Lock(a)
	}
	if b < a {
		a, b = b, a
	}
	first, second := p.lockFor(a), p.lockFor(b)
	first.Lock()
	second.Lock()
	return func() {
		second.Unlock()
		first.Unlock()
	}
}

func (p *PathLocks) lockFor(rel string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	l, ok := p.locks[rel]
	if !ok {
		l = &sync.Mutex{}
		p.locks[rel] = l
	}
	return l
}
