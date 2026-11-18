package queue

import (
	"strings"
	"testing"
)

// The idempotency hash must not change across the 0.6 upgrade that added the
// "machine" key: hashForm writes the machine id under the old "connector" key
// alone, so a request 0.5 created is found again on 0.6 rather than refused as
// a key conflict.
func TestIdempotencyHashStableAcrossMachineKey(t *testing.T) {
	spec := Spec{Harness: "claude", FolderRef: "fld_0123456789abcdef", Machine: "con_a", Prompt: "hi", Isolate: true}
	b, err := spec.asked().hashBytes()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"connector":"con_a"`) {
		t.Fatalf("the hash form lacks the old connector key: %s", s)
	}
	if strings.Contains(s, `"machine"`) {
		t.Fatalf("the hash form has the machine key, so the hash changed across the upgrade: %s", s)
	}
	// The connector key keeps its old place, before prompt: the hash is over
	// these bytes, so the order must match 0.5's.
	if strings.Index(s, `"connector"`) > strings.Index(s, `"prompt"`) {
		t.Fatalf("connector must come before prompt, as 0.5 wrote it: %s", s)
	}

	// End to end: a request made with a key is found again by the same key and
	// request, the machine notwithstanding -- not a key conflict.
	q, _ := open(t)
	j1, c1, err := q.Create(DefaultTeam, KindTurn, "idem_1", spec, Links{})
	if err != nil || !c1 {
		t.Fatalf("first create: created=%v err=%v", c1, err)
	}
	j2, c2, err := q.Create(DefaultTeam, KindTurn, "idem_1", spec, Links{})
	if err != nil || c2 || j2.ID != j1.ID {
		t.Fatalf("retry: created=%v id=%s (want %s) err=%v", c2, j2.ID, j1.ID, err)
	}
}
