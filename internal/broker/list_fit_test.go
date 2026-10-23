package broker

import "testing"

// Past the limit, the decisions after the cursor are kept oldest first,
// and the cursor goes back to just before the first left out.
func TestFitKeepsOldestDecisions(t *testing.T) {
	var reqs []RequestInfo
	for i := 250; i >= 1; i-- {
		reqs = append(reqs, RequestInfo{ID: uint64(i), State: StateDenied, Seq: uint64(i + 1)})
	}
	reqs = append(reqs, RequestInfo{ID: 251, State: StatePending})
	kept, cur := Fit(reqs, 1, 300, func(RequestInfo) bool { return true }, MaxList)
	if len(kept) != MaxList || kept[0].ID != MaxList || kept[len(kept)-1].ID != 1 || cur != MaxList+1 {
		t.Fatalf("kept %d (%d..%d), cursor %d", len(kept), kept[0].ID, kept[len(kept)-1].ID, cur)
	}
	// Under the limit: everything, newest first, the cursor as it was.
	kept, cur = Fit(reqs[240:], 1, 300, func(RequestInfo) bool { return true }, MaxList)
	if len(kept) != 11 || kept[0].ID != 251 || cur != 300 {
		t.Fatalf("kept %d, first %d, cursor %d", len(kept), kept[0].ID, cur)
	}
}
