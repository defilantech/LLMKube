/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package picker

import (
	"container/list"
	"encoding/binary"
	"hash/fnv"
	"sync"
)

// BlockHashes cuts a prompt stream into full blocks of blockBytes and hashes
// each block together with the hash before it, so a block's hash matches only
// when everything in front of it matched too: a prefix, not a substring. The
// trailing partial block is left out; it changes as soon as the conversation
// grows. A blockBytes of zero or less returns nil.
func BlockHashes(stream []byte, blockBytes int) []uint64 {
	if blockBytes <= 0 {
		return nil
	}
	out := make([]uint64, 0, len(stream)/blockBytes)
	var prev [8]byte
	for off := 0; off+blockBytes <= len(stream); off += blockBytes {
		h := fnv.New64a()
		_, _ = h.Write(prev[:])
		_, _ = h.Write(stream[off : off+blockBytes])
		x := mix64(h.Sum64())
		out = append(out, x)
		binary.LittleEndian.PutUint64(prev[:], x)
	}
	return out
}

// PrefixIndex remembers, per endpoint, the prompt blocks this replica sent
// there, as an approximation of what the pod holds in its prefix cache. It
// never sees the engine's cache: a block counts as cached from the moment its
// request is placed until maxBlocks newer blocks push it out. Safe for
// concurrent use.
type PrefixIndex struct {
	mu        sync.Mutex
	maxBlocks int
	eps       map[string]*blockLRU
}

type blockLRU struct {
	order *list.List // front is the most recently recorded; values are uint64
	at    map[uint64]*list.Element
}

// NewPrefixIndex returns an empty index that keeps at most maxBlocks blocks
// per endpoint (at least one).
func NewPrefixIndex(maxBlocks int) *PrefixIndex {
	return &PrefixIndex{maxBlocks: max(maxBlocks, 1), eps: map[string]*blockLRU{}}
}

// Matched returns how many leading blocks of chain the endpoint holds.
func (x *PrefixIndex) Matched(endpoint string, chain []uint64) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	l := x.eps[endpoint]
	if l == nil {
		return 0
	}
	n := 0
	for _, h := range chain {
		if _, ok := l.at[h]; !ok {
			break
		}
		n++
	}
	return n
}

// Record marks chain as sent to the endpoint, most recent first, evicting the
// endpoint's least recently recorded blocks past maxBlocks.
func (x *PrefixIndex) Record(endpoint string, chain []uint64) {
	if len(chain) == 0 {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	l := x.eps[endpoint]
	if l == nil {
		l = &blockLRU{order: list.New(), at: map[uint64]*list.Element{}}
		x.eps[endpoint] = l
	}
	for _, h := range chain {
		if e, ok := l.at[h]; ok {
			l.order.MoveToFront(e)
			continue
		}
		l.at[h] = l.order.PushFront(h)
	}
	for l.order.Len() > x.maxBlocks {
		e := l.order.Back()
		delete(l.at, e.Value.(uint64))
		l.order.Remove(e)
	}
}

// Retain forgets every endpoint not in live, so a pod that left the set (and
// any later pod at its address) starts with no claimed cache.
func (x *PrefixIndex) Retain(live []string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.eps) == 0 {
		return
	}
	keep := make(map[string]struct{}, len(live))
	for _, ep := range live {
		keep[ep] = struct{}{}
	}
	for ep := range x.eps {
		if _, ok := keep[ep]; !ok {
			delete(x.eps, ep)
		}
	}
}

// Len returns how many blocks the index holds for the endpoint.
func (x *PrefixIndex) Len(endpoint string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	if l := x.eps[endpoint]; l != nil {
		return l.order.Len()
	}
	return 0
}
