package daemonproto

import (
	"maps"
	"slices"
	"time"
)

// What needs the user, as status carries it: one list, computed on the
// host, so that the app and the page show it and decide nothing.
const (
	// MaxAttention bounds StatusResult.Attention.
	MaxAttention = 64
	// MaxAttentionText bounds an item's text.
	MaxAttentionText = 300
)

// The kinds of AttentionItem.
const (
	AttentionRequests   = "requests"    // requests pending in the environment
	AttentionProposals  = "proposals"   // proposal files pending in the environment
	AttentionLinkFailed = "link_failed" // the link was wanted and failed
	AttentionVnodes     = "vnodes"      // the VM holds many of the Mac's vnodes
	AttentionCheckpoint = "checkpoint"  // a checkpoint error, low space, shrink or exposed store
	AttentionImageStale = "image_stale" // the environment's image is out of date or switched
	AttentionUpdate     = "update"      // a newer caboose release is out
)

// attentionKinds are the kinds, and whether an item of the kind may name
// no environment (it is about the Mac or the install as a whole).
var attentionKinds = map[string]bool{
	AttentionRequests:   false,
	AttentionProposals:  false,
	AttentionLinkFailed: false,
	AttentionVnodes:     true,
	AttentionCheckpoint: false,
	AttentionImageStale: false,
	AttentionUpdate:     true,
}

// AttentionKinds are the kinds an item may have, sorted.
func AttentionKinds() []string {
	return slices.Sorted(maps.Keys(attentionKinds))
}

// AttentionItem is one thing that needs the user.
type AttentionItem struct {
	// Env is the environment it is about; empty for the kinds that are
	// about the Mac or the install (vnodes, update).
	Env  string `json:"env,omitempty"`
	Kind string `json:"kind"`
	// Text is host-written and printable, never the sandbox's words
	// uncut.
	Text string `json:"text"`
	// Route is the web page the item opens, as a notification's route.
	Route string `json:"route,omitempty"`
	// Since is when it began to need the user, kept while it does.
	Since time.Time `json:"since"`
}

// Validate checks the item: a known kind, an environment where the kind
// needs one, a printable bounded text and a route the page takes.
func (a *AttentionItem) Validate() error {
	allowEmpty, known := attentionKinds[a.Kind]
	if !known {
		return bad("unknown attention kind")
	}
	if a.Env == "" && !allowEmpty || a.Env != "" && !ValidEnv(a.Env) {
		return bad("bad environment name in an attention item")
	}
	if err := checkText("attention text", a.Text, MaxAttentionText); err != nil {
		return err
	}
	if !ValidRoute(a.Route) {
		return bad("bad route in an attention item")
	}
	return nil
}

func validateAttention(items []AttentionItem) error {
	if len(items) > MaxAttention {
		return bad("over %d attention items", MaxAttention)
	}
	for i := range items {
		if err := items[i].Validate(); err != nil {
			return err
		}
	}
	return nil
}
