package consensus

// MerkleRoot computes the root of the tagged, promote-odd merkle tree
// (SPEC §2.3). Leaves are txids or wtxids.
//
//	leaf  = H("Argonmine/merkle/leaf", id)
//	node  = H("Argonmine/merkle/node", left || right)
//	odd trailing node at any level is promoted unchanged.
//
// Properties this gives us that Bitcoin's tree lacks:
//   - distinct leaf lists always have distinct roots (no CVE-2012-2459);
//   - a leaf hash can never be reinterpreted as a node hash or vice versa.
func MerkleRoot(ids []Hash) (Hash, error) {
	if len(ids) == 0 {
		return Hash{}, ErrMerkleEmpty
	}
	level := make([]Hash, len(ids))
	for i, id := range ids {
		level[i] = TaggedHash(TagMerkleLeaf, id[:])
	}
	for len(level) > 1 {
		next := make([]Hash, 0, (len(level)+1)/2)
		for i := 0; i+1 < len(level); i += 2 {
			next = append(next, TaggedHash(TagMerkleNode, level[i][:], level[i+1][:]))
		}
		if len(level)%2 == 1 {
			next = append(next, level[len(level)-1])
		}
		level = next
	}
	return level[0], nil
}
