// Copyright 2023 ChainSafe Systems (ON)
// SPDX-License-Identifier: LGPL-3.0-only

package grandpa

import (
	"fmt"

	"github.com/tidwall/btree"
	"golang.org/x/exp/constraints"
	"golang.org/x/exp/slices"
)

type voteGraphEntry[
	Hash constraints.Ordered,
	Number constraints.Integer,
	voteNode voteNodeI[voteNode, Vote],
	Vote any,
] struct {
	number Number
	// ancestor hashes in reverse order, e.g. ancestors[0] is the parent
	// and the last entry is the hash of the parent vote-node.
	ancestors      []Hash
	descendants    []Hash // descendent vote-nodes
	cumulativeVote voteNode
}

// whether the given hash, number pair is a direct ancestor of this node.
// `nil` signifies that the graph must be traversed further back.
func (vge voteGraphEntry[Hash, Number, voteNode, Vote]) inDirectAncestry(hash Hash, num Number) *bool {
	h := vge.ancestorBlock(num)
	if h == nil {
		return nil
	}
	b := *h == hash
	return &b
}

// Get ancestor block by number. Returns `nil` if there is no block
// by that number in the direct ancestry.
func (vge voteGraphEntry[Hash, Number, voteNode, Vote]) ancestorBlock(num Number) (h *Hash) {
	if num >= vge.number {
		return nil
	}
	offset := vge.number - num - 1
	if int(offset) >= len(vge.ancestors) {
		return nil
	}
	ancestor := vge.ancestors[int(offset)]
	return &ancestor
}

// get ancestor vote-node.
func (vge voteGraphEntry[Hash, Number, voteNode, Vote]) ancestorNode() *Hash {
	if len(vge.ancestors) == 0 {
		return nil
	}
	h := vge.ancestors[len(vge.ancestors)-1]
	return &h
}

// VoteGraph maintains a DAG of blocks in the chain which have votes attached to them,
// and vote data which is accumulated along edges.
type VoteGraph[
	Hash constraints.Ordered,
	Number constraints.Unsigned,
	voteNode voteNodeI[voteNode, Vote],
	Vote any,
] struct {
	entries            *btree.Map[Hash, voteGraphEntry[Hash, Number, voteNode, Vote]]
	heads              *btree.Set[Hash]
	base               Hash
	baseNumber         Number
	newDefaultvoteNode func() voteNode
}

// NewVoteGraph creates a new `VoteGraph` with base node as given.
func NewVoteGraph[
	Hash constraints.Ordered,
	Number constraints.Unsigned,
	voteNode voteNodeI[voteNode, Vote],
	Vote any,
](
	baseHash Hash,
	baseNumber Number,
	baseNode voteNode,
	newDefaultvoteNode func() voteNode,
) VoteGraph[Hash, Number, voteNode, Vote] {
	entries := btree.NewMap[Hash, voteGraphEntry[Hash, Number, voteNode, Vote]](2)
	entries.Set(baseHash, voteGraphEntry[Hash, Number, voteNode, Vote]{
		number:         baseNumber,
		ancestors:      make([]Hash, 0),
		descendants:    make([]Hash, 0),
		cumulativeVote: baseNode,
	})
	heads := &btree.Set[Hash]{}
	heads.Insert(baseHash)
	return VoteGraph[Hash, Number, voteNode, Vote]{
		entries:            entries,
		heads:              heads,
		base:               baseHash,
		baseNumber:         baseNumber,
		newDefaultvoteNode: newDefaultvoteNode,
	}
}

// append a vote-node onto the chain-tree. This should only be called if
// no node in the tree keeps the target anyway.
func (vg *VoteGraph[Hash, Number, voteNode, Vote]) append(
	hash Hash,
	num Number,
	chain Chain[Hash, Number],
) (err error) {
	ancestry, err := chain.Ancestry(vg.base, hash)
	if err != nil {
		return err
	}
	ancestry = append(ancestry, vg.base)

	var ancestorIndex *int
	for i, ancestor := range ancestry {
		entry, ok := vg.entries.Get(ancestor)
		if ok {
			entry.descendants = append(entry.descendants, hash)
			vg.entries.Set(ancestor, entry)
			if ancestorIndex == nil {
				ai := i
				ancestorIndex = &ai
				break
			}
		}
	}

	if ancestorIndex == nil {
		panic(fmt.Errorf("base is kept; chain returns ancestry only if the block is a descendent of base;"))
	}

	ancestorHash := ancestry[*ancestorIndex]
	ancestry = ancestry[0 : *ancestorIndex+1]

	vg.entries.Set(hash, voteGraphEntry[Hash, Number, voteNode, Vote]{
		number:         num,
		ancestors:      ancestry,
		descendants:    make([]Hash, 0),
		cumulativeVote: vg.newDefaultvoteNode(),
	})

	vg.heads.Delete(ancestorHash)
	vg.heads.Insert(hash)
	return
}

// introduce a branch to given vote-nodes.
//
// `descendents` is a list of nodes with ancestor-edges containing the given ancestor.
//
// This function panics if any member of `descendents` is not a vote-node
// or does not have ancestor with given hash and number OR if `ancestorHash`
// is already a known entry.

func (vg *VoteGraph[Hash, Number, voteNode, Vote]) introduceBranch(
	descendants []Hash,
	ancestorHash Hash,
	ancestorNumber Number,
) {
	var producedEntry *struct {
		entry voteGraphEntry[Hash, Number, voteNode, Vote]
		hash  *Hash
	}
	var maybeEntry *struct {
		entry voteGraphEntry[Hash, Number, voteNode, Vote]
		hash  *Hash
	}
	for _, descendant := range descendants {
		entry, ok := vg.entries.Get(descendant)
		if !ok {
			panic("this function only invoked with keys of vote-nodes; qed")
		}

		ida := entry.inDirectAncestry(ancestorHash, ancestorNumber)
		if ida == nil || !*ida {
			panic("entry is supposed to be in direct ancestry")
		}

		// example: splitting number 10 at ancestor 4
		// before: [9 8 7 6 5 4 3 2 1]
		// after: [9 8 7 6 5 4], [3 2 1]
		// we ensure the `entry.ancestors` is drained regardless of whether
		// the `newEntry` has already been constructed.
		{
			prevAncestor := entry.ancestorNode()
			var offset uint
			if ancestorNumber > entry.number {
				panic("this function only invoked with direct ancestors; qed")
			} else {
				offset = uint(entry.number - ancestorNumber)
			}
			newAncestors := entry.ancestors[offset:len(entry.ancestors)]
			entry.ancestors = entry.ancestors[0:offset]
			vg.entries.Set(descendant, entry)

			if maybeEntry == nil {
				maybeEntry = &struct {
					entry voteGraphEntry[Hash, Number, voteNode, Vote]
					hash  *Hash
				}{
					entry: voteGraphEntry[Hash, Number, voteNode, Vote]{
						number:         ancestorNumber,
						ancestors:      newAncestors,
						descendants:    make([]Hash, 0),
						cumulativeVote: vg.newDefaultvoteNode(),
					},
					hash: prevAncestor,
				}
			}
			maybeEntry.entry.descendants = append(maybeEntry.entry.descendants, descendant)
			maybeEntry.entry.cumulativeVote.Add(entry.cumulativeVote)
		}
		producedEntry = maybeEntry
	}

	if producedEntry != nil {
		newEntry := producedEntry.entry
		prevAncestor := producedEntry.hash
		if prevAncestor != nil {
			prevancestorNode, _ := vg.entries.Get(*prevAncestor)
			prevancestorNodeDescendants := make([]Hash, 0)
			for _, d := range prevancestorNode.descendants {
				if !slices.Contains(newEntry.descendants, d) {
					prevancestorNodeDescendants = append(prevancestorNodeDescendants, d)
				}
			}
			prevancestorNodeDescendants = append(prevancestorNodeDescendants, ancestorHash)
			prevancestorNode.descendants = prevancestorNodeDescendants
			vg.entries.Set(*producedEntry.hash, prevancestorNode)
		}
		vg.entries.Set(ancestorHash, producedEntry.entry)
	}
}

// Insert a vote with given value into the graph at given hash and number.
func (vg *VoteGraph[Hash, Number, voteNode, Vote]) Insert(
	hash Hash,
	num Number,
	vote any,
	chain Chain[Hash, Number],
) error {
	containing := vg.findContainingNodes(hash, num)
	switch {
	case containing == nil:
		// this entry already exists
	case len(containing) == 0:
		err := vg.append(hash, num, chain)
		if err != nil {
			return err
		}
	default:
		vg.introduceBranch(containing, hash, num)
	}

	// update cumulative vote data.
	// NOTE: below this point, there always exists a node with the given hash and number.
	var inspectingHash = hash
	for {
		activeEntry, ok := vg.entries.Get(inspectingHash)
		if !ok {
			panic("vote-node and its ancestry always exist after initial phase; qed")
		}
		switch vote := vote.(type) {
		case voteNode:
			activeEntry.cumulativeVote.Add(vote)
		case Vote:
			activeEntry.cumulativeVote.AddVote(vote)
		default:
			panic(fmt.Errorf("unsupported type to add to cumulativeVote %T", vote))
		}
		vg.entries.Set(inspectingHash, activeEntry)

		parent := activeEntry.ancestorNode()
		if parent != nil {
			inspectingHash = *parent
		} else {
			break
		}
	}
	return nil
}

// attempts to find the containing node keys for the given hash and number.
//
// returns `nil` if there is a node by that key already, and a slice
// (potentially empty) of nodes with the given block in its ancestor-edge
// otherwise.
func (vg *VoteGraph[Hash, Number, voteNode, Vote]) findContainingNodes(hash Hash, num Number) (hashes []Hash) {
	_, ok := vg.entries.Get(hash)
	if ok {
		return nil
	}

	containingKeys := make([]Hash, 0)
	visited := make(map[Hash]interface{})

	for _, head := range vg.heads.Keys() {
		var activeEntry voteGraphEntry[Hash, Number, voteNode, Vote]

		for {
			e, ok := vg.entries.Get(head)
			if !ok {
				break
			}
			activeEntry = e

			_, ok = visited[head]
			// if node has been checked already break
			if ok {
				break
			}
			visited[head] = nil

			da := activeEntry.inDirectAncestry(hash, num)
			switch {
			case da == nil:
				prev := activeEntry.ancestorNode()
				if prev != nil {
					head = *prev
					continue // iterate backwards
				}
			case *da:
				// set containing node and continue search.
				containingKeys = append(containingKeys, head)
			case !*da:
				// nothing in this branch. continue search.
			}
			break
		}
	}
	return containingKeys
}

// a subchain of blocks by hash.
type subChain[Hash comparable, Number constraints.Unsigned] struct {
	hashes     []Hash // forward order
	bestNumber Number
}

func (sc subChain[H, N]) best() *HashNumber[H, N] {
	if len(sc.hashes) == 0 {
		return nil
	}
	return &HashNumber[H, N]{
		sc.hashes[len(sc.hashes)-1],
		sc.bestNumber,
	}
}

func (vg *VoteGraph[Hash, Number, voteNode, Vote]) mustGetEntry(
	hash Hash,
) voteGraphEntry[Hash, Number, voteNode, Vote] {
	entry, ok := vg.entries.Get(hash)
	if !ok {
		panic("descendents always present in node storage; qed")
	}
	return entry
}

type hashvote[Hash constraints.Ordered, voteNode voteNodeI[voteNode, Vote], Vote any] struct {
	hash Hash
	vote voteNode
}

// given a key, node pair (which must correspond), assuming this node fulfils the condition,
// this function will find the highest point at which its descendents merge, which may be the
// node itself.
func (vg *VoteGraph[Hash, Number, voteNode, Vote]) ghostFindMergePoint( //skipcq: GO-R1005
	nodeKey Hash, activeNode *voteGraphEntry[Hash, Number, voteNode, Vote], forceConstrain *HashNumber[Hash, Number],
	condition func(voteNode) bool) subChain[Hash, Number] {

	var descendantNodes []voteGraphEntry[Hash, Number, voteNode, Vote]
	for _, descendant := range activeNode.descendants {
		switch {
		case forceConstrain == nil:
			descendantNodes = append(descendantNodes, vg.mustGetEntry(descendant))
		default:
			ida := vg.mustGetEntry(descendant).inDirectAncestry(forceConstrain.Hash, forceConstrain.Number)
			switch {
			case ida == nil:
			case !*ida:
			case *ida:
				descendantNodes = append(descendantNodes, vg.mustGetEntry(descendant))
			}

		}
	}

	baseNumber := activeNode.number
	bestNumber := activeNode.number

	descendantBlocks := make([]hashvote[Hash, voteNode, Vote], 0)
	hashes := []Hash{nodeKey}

	// TODO: for long ranges of blocks this could get inefficient
	var offset Number
	for {
		offset = offset + 1

		var newBest *Hash
		for _, dNode := range descendantNodes {
			dBlock := dNode.ancestorBlock(baseNumber + offset)
			if dBlock == nil {
				continue
			}
			idx, ok := slices.BinarySearchFunc(
				descendantBlocks,
				hashvote[Hash, voteNode, Vote]{hash: *dBlock},
				func(a, b hashvote[Hash, voteNode, Vote]) int {
					switch {
					case a.hash == b.hash:
						return 0
					case a.hash > b.hash:
						return 1
					case a.hash < b.hash:
						return -1
					default:
						panic("unreachable")
					}
				},
			)
			if ok {
				descendantBlocks[idx].vote.Add(dNode.cumulativeVote)
				if condition(descendantBlocks[idx].vote) {
					newBest = dBlock
					break
				}
			} else {
				if idx == len(descendantBlocks) {
					descendantBlocks = append(descendantBlocks, hashvote[Hash, voteNode, Vote]{
						hash: *dBlock,
						vote: dNode.cumulativeVote.Copy(),
					})
				} else if idx < len(descendantBlocks) {
					descendantBlocks = append(
						descendantBlocks[:idx],
						append([]hashvote[Hash, voteNode, Vote]{{
							hash: *dBlock,
							vote: dNode.cumulativeVote.Copy(),
						}}, descendantBlocks[idx:]...)...)
				} else {
					panic("unreachable")
				}
			}
		}

		if newBest != nil {
			bestNumber = bestNumber + 1
			descendantBlocks = make([]hashvote[Hash, voteNode, Vote], 0)
			retained := make([]voteGraphEntry[Hash, Number, voteNode, Vote], 0)
			for _, descendant := range descendantNodes {
				ida := descendant.inDirectAncestry(*newBest, bestNumber)
				if ida != nil && *ida {
					retained = append(retained, descendant)
				}
			}
			descendantNodes = retained
			hashes = append(hashes, *newBest)
		} else {
			break
		}
	}

	return subChain[Hash, Number]{
		hashes:     hashes,
		bestNumber: bestNumber,
	}
}

type hashVoteGraphEntry[
	Hash constraints.Ordered,
	Number constraints.Integer,
	voteNode voteNodeI[voteNode, Vote],
	Vote any,
] struct {
	hash  Hash
	entry voteGraphEntry[Hash, Number, voteNode, Vote]
}

// FindGHOST will find the best GHOST descendent of the given block.
// Pass a closure used to evaluate the cumulative vote value.
//
// The GHOST (hash, number) returned will be the block with highest number for which the
// cumulative votes of descendents and itself causes the closure to evaluate to true.
//
// This assumes that the evaluation closure is one which returns true for at most a single
// descendent of a block, in that only one fork of a block can be "heavy"
// enough to trigger the threshold.
//
// Returns `nil` when the given `currentBest` does not fulfil the condition.
func (vg *VoteGraph[Hash, Number, voteNode, Vote]) FindGHOST( //skipcq: GO-R1005
	currentBest *HashNumber[Hash, Number],
	condition func(voteNode) bool,
) *HashNumber[Hash, Number] {
	var getNode = func(hash Hash) *voteGraphEntry[Hash, Number, voteNode, Vote] {
		entry, ok := vg.entries.Get(hash)
		if !ok {
			panic("node either base or referenced by other in graph; qed")
		}
		return &entry
	}

	var nodeKey Hash
	var forceConstrain bool

	if currentBest == nil {
		nodeKey = vg.base
		forceConstrain = false
	} else {
		containing := vg.findContainingNodes(currentBest.Hash, currentBest.Number)
		switch {
		case containing == nil:
			nodeKey = currentBest.Hash
			forceConstrain = false
		case len(containing) > 0:
			ancestor := getNode(containing[0]).ancestorNode()
			if ancestor == nil {
				panic("node containing non-node in history always has ancestor; qed")
			}
			nodeKey = *ancestor
			forceConstrain = true
		default:
			nodeKey = vg.base
			forceConstrain = false
		}
	}

	activeNode := getNode(nodeKey)

	if !condition(activeNode.cumulativeVote) {
		return nil
	}

	// breadth-first search starting from this node.
loop:
	for {
		var nextDescendant *hashVoteGraphEntry[Hash, Number, voteNode, Vote]
		filteredDescendants := make([]*hashVoteGraphEntry[Hash, Number, voteNode, Vote], 0)

		for _, descendant := range activeNode.descendants {
			if forceConstrain && currentBest != nil {
				node := getNode(descendant)
				ida := node.inDirectAncestry(currentBest.Hash, currentBest.Number)
				switch {
				case ida == nil:
				case !*ida:
				case *ida:
					filteredDescendants = append(filteredDescendants, &hashVoteGraphEntry[Hash, Number, voteNode, Vote]{
						hash:  descendant,
						entry: *node,
					})
				}
			} else {
				node := getNode(descendant)
				filteredDescendants = append(filteredDescendants, &hashVoteGraphEntry[Hash, Number, voteNode, Vote]{
					hash:  descendant,
					entry: *node,
				})
			}
		}

		for _, hvge := range filteredDescendants {
			if condition(hvge.entry.cumulativeVote) {
				nextDescendant = &hashVoteGraphEntry[Hash, Number, voteNode, Vote]{
					hash:  hvge.hash,
					entry: hvge.entry,
				}
				break
			}
		}

		switch nextDescendant {
		case nil:
			break loop
		default:
			forceConstrain = false
			nodeKey = nextDescendant.hash
			activeNode = &nextDescendant.entry
		}

	}

	var hn *HashNumber[Hash, Number]
	if forceConstrain {
		hn = currentBest
	}

	return vg.ghostFindMergePoint(nodeKey, activeNode, hn, condition).best()
}

// FindAncestor will find the block with the highest block number in the chain with the given head
// which fulfils the given condition.
//
// Returns `nil` if the given head is not in the graph or no node fulfils the
// given condition.
func (vg *VoteGraph[Hash, Number, voteNode, Vote]) FindAncestor(
	hash Hash,
	number Number,
	condition func(voteNode) bool,
) *HashNumber[Hash, Number] {
	for {
		children := vg.findContainingNodes(hash, number)
		if children == nil {
			// The block has a vote-node in the graph.
			node := vg.mustGetEntry(hash)
			// If the weight is sufficient, we are done.
			if condition(node.cumulativeVote) {
				return &HashNumber[Hash, Number]{hash, number}
			}
			// Not enough weight, check the parent block.
			if len(node.ancestors) == 0 {
				return nil
			}
			hash = node.ancestors[0]
			number = node.number - 1
		} else {
			// If there are no vote-nodes below the block in the graph,
			// the block is not in the graph at all.
			if len(children) == 0 {
				return nil
			}
			// The block is "contained" in the graph (i.e. in the ancestry-chain
			// of at least one vote-node) but does not itself have a vote-node.
			// Check if the accumulated weight on all child vote-nodes is sufficient.
			v := vg.newDefaultvoteNode()
			for _, c := range children {
				e := vg.mustGetEntry(c)
				v.Add(e.cumulativeVote)
			}
			if condition(v) {
				return &HashNumber[Hash, Number]{hash, number}
			}

			// Not enough weight, check the parent block.
			child := children[len(children)-1]
			entry := vg.mustGetEntry(child)
			offset := int(entry.number - number)

			if offset >= len(entry.ancestors) {
				// Reached base without sufficient weight.
				return nil
			}
			parent := entry.ancestors[offset]

			hash = parent
			number = number - 1
		}
	}
}

// AdjustBase will adjust the base of the graph. The new base must be an ancestor of the
// old base.
//
// Provide an ancestry proof from the old base to the new. The proof
// should be in reverse order from the old base's parent.
func (vg *VoteGraph[Hash, Number, voteNode, Vote]) AdjustBase(ancestryProof []Hash) {
	if len(ancestryProof) == 0 {
		return // empty nothing to do
	}
	newHash := ancestryProof[len(ancestryProof)-1]

	// not a valid ancestry proof. TODO: error? (TODO copied from rust code)
	if len(ancestryProof) > int(vg.baseNumber) {
		return
	}

	newNumber := vg.baseNumber
	newNumber = newNumber - Number(len(ancestryProof))

	oldEntry := vg.mustGetEntry(vg.base)
	oldEntry.ancestors = append(oldEntry.ancestors, ancestryProof...)
	vg.entries.Set(vg.base, oldEntry)

	entry := voteGraphEntry[Hash, Number, voteNode, Vote]{
		number:         newNumber,
		ancestors:      make([]Hash, 0),
		descendants:    []Hash{vg.base},
		cumulativeVote: oldEntry.cumulativeVote.Copy(),
	}
	vg.entries.Set(newHash, entry)
	vg.base = newHash
	vg.baseNumber = newNumber
}

// Base returns the base block.
func (vg *VoteGraph[Hash, Number, voteNode, Vote]) Base() HashNumber[Hash, Number] {
	return HashNumber[Hash, Number]{
		vg.base,
		vg.baseNumber,
	}
}
