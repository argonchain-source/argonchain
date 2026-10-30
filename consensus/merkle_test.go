package consensus

import (
	"errors"
	"testing"
)

func ids(n int) []Hash {
	out := make([]Hash, n)
	for i := range out {
		out[i] = TaggedHash("test/id", []byte{byte(i), byte(i >> 8)})
	}
	return out
}

func TestMerkleEmptyInvalid(t *testing.T) {
	if _, err := MerkleRoot(nil); !errors.Is(err, ErrMerkleEmpty) {
		t.Fatal("empty tree accepted")
	}
}

func TestMerkleSingleLeafIsLeafHash(t *testing.T) {
	id := ids(1)[0]
	root, _ := MerkleRoot([]Hash{id})
	if root != TaggedHash(TagMerkleLeaf, id[:]) {
		t.Fatal("single leaf root must be the tagged leaf hash, not the raw id")
	}
	if root == id {
		t.Fatal("root must not equal the raw id")
	}
}

func TestMerkleStructure(t *testing.T) {
	l := ids(3)
	a := TaggedHash(TagMerkleLeaf, l[0][:])
	b := TaggedHash(TagMerkleLeaf, l[1][:])
	c := TaggedHash(TagMerkleLeaf, l[2][:])
	ab := TaggedHash(TagMerkleNode, a[:], b[:])
	// odd leaf c is promoted, NOT duplicated
	want := TaggedHash(TagMerkleNode, ab[:], c[:])
	got, _ := MerkleRoot(l)
	if got != want {
		t.Fatal("3-leaf structure wrong")
	}
}

// CVE-2012-2459 class: in Bitcoin, [a,b,c] and [a,b,c,c] have the same root.
func TestMerkleNoDuplicateLeafCollision(t *testing.T) {
	l := ids(3)
	r3, _ := MerkleRoot(l)
	r4, _ := MerkleRoot(append(append([]Hash{}, l...), l[2]))
	if r3 == r4 {
		t.Fatal("duplicate-leaf collision possible")
	}
	l5 := ids(5)
	r5, _ := MerkleRoot(l5)
	r6, _ := MerkleRoot(append(append([]Hash{}, l5...), l5[4]))
	if r5 == r6 {
		t.Fatal("duplicate-leaf collision possible (5/6)")
	}
}

func TestMerkleOrderMatters(t *testing.T) {
	l := ids(4)
	r1, _ := MerkleRoot(l)
	swapped := []Hash{l[1], l[0], l[2], l[3]}
	r2, _ := MerkleRoot(swapped)
	if r1 == r2 {
		t.Fatal("order-insensitive root")
	}
}

func TestMerkleDeterministicAcrossSizes(t *testing.T) {
	seen := map[Hash]int{}
	for n := 1; n <= 40; n++ {
		r, err := MerkleRoot(ids(n))
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[r]; dup {
			t.Fatalf("roots for n=%d and n=%d collide", prev, n)
		}
		seen[r] = n
	}
}
