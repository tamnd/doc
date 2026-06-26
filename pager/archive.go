package pager

import "github.com/tamnd/doc/wal"

// CommitEvent carries the page images one commit made durable, handed to a
// CommitObserver from inside Commit (spec 2061 doc 18 §13.5). The Frames slice and
// its payloads belong to the pager; an observer that needs to keep them past the
// callback must copy them.
type CommitEvent struct {
	CommitLSN   uint64          // frame LSN of the commit's last frame
	DBSizePages uint32          // database size in pages as of this commit
	Frames      []wal.PageImage // the page images this commit wrote, in log order
}

// CommitObserver receives one CommitEvent per group commit, in commit order.
type CommitObserver func(CommitEvent)

// SetCommitObserver installs (or clears, with nil) the commit observer. It is meant
// to be set once at open before writes begin; the call takes the pager lock so it is
// safe to call on a live pager, but a commit racing the swap may use either value.
func (p *Pager) SetCommitObserver(obs CommitObserver) {
	p.mu.Lock()
	p.observer = obs
	p.mu.Unlock()
}

// DurableLSN returns the frame LSN of the last committed frame, the commit LSN of the
// most recent commit. A caller that reads it right after its own commit, on the
// single-writer commit path, sees that commit's LSN.
func (p *Pager) DurableLSN() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.walFlushedLSN
}
