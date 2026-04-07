// Package render implements a region-based frame compositor for the TUI.
//
// The compositor caches rendered regions (content, divider, tail, bottom)
// and only recomposes the frame string when a region actually changes.
// Bubble Tea's built-in renderer handles line-level terminal diffing —
// this module prevents the CPU cost of regenerating unchanged content.
package render

import (
	"hash/fnv"
	"strings"
)

// RegionID identifies a composited section of the frame.
type RegionID int

const (
	RegionContent RegionID = iota // scrollable log lines
	RegionDivider                 // horizontal divider + tail junction
	RegionTail                    // tail connector lines
	RegionBottom                  // tonto image + input area (side-by-side)
	regionCount
)

// Compositor assembles regions into a frame and tracks dirtiness.
type Compositor struct {
	regions [regionCount]region
	cached  string // last Compose() output
	dirty   bool   // any region changed since last Compose
}

type region struct {
	lines []string
	hash  uint64
	set   bool // true once SetRegion has been called at least once
}

// New creates a compositor. All regions start dirty.
func New() *Compositor {
	return &Compositor{dirty: true}
}

// Reset marks all regions as unset, forcing full regeneration.
// Call on resize or screen transitions.
func (c *Compositor) Reset() {
	for i := range c.regions {
		c.regions[i] = region{}
	}
	c.cached = ""
	c.dirty = true
}

// SetRegion updates a region's content. Hashes the lines and compares
// with the previous hash — if unchanged, the region is not marked dirty.
// Returns true if the content actually changed.
func (c *Compositor) SetRegion(id RegionID, lines []string) bool {
	h := hashLines(lines)
	r := &c.regions[id]
	if r.set && r.hash == h {
		return false // unchanged
	}
	r.lines = lines
	r.hash = h
	r.set = true
	c.dirty = true
	return true
}

// Compose joins all regions into a single string. If nothing changed
// since the last call, returns the cached string.
func (c *Compositor) Compose() string {
	if !c.dirty && c.cached != "" {
		return c.cached
	}

	var total int
	for i := range c.regions {
		total += len(c.regions[i].lines)
	}

	all := make([]string, 0, total)
	for i := range c.regions {
		all = append(all, c.regions[i].lines...)
	}

	c.cached = strings.Join(all, "\n")
	c.dirty = false
	return c.cached
}

func hashLines(lines []string) uint64 {
	h := fnv.New64a()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return h.Sum64()
}
