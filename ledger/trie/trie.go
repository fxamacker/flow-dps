// Copyright 2021 Optakt Labs OÜ
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package trie

import (
	"github.com/gammazero/deque"
	"github.com/rs/zerolog"
	"lukechampine.com/blake3"

	"github.com/onflow/flow-go/ledger"
	"github.com/onflow/flow-go/ledger/common/bitutils"
	"github.com/onflow/flow-go/ledger/common/hash"

	"github.com/optakt/flow-dps/models/dps"
)

const maxDepth = 255

// Trie is a modified Patricia-Merkle Trie, which is the storage layer of the Flow ledger.
// It uses a payload store to retrieve and persist ledger payloads.
type Trie struct {
	log   zerolog.Logger
	root  Node
	store dps.Store
}

// NewEmptyTrie creates a new trie without a root node, with the given payload store.
func NewEmptyTrie(log zerolog.Logger, store dps.Store) *Trie {
	t := Trie{
		log:   log.With().Str("subcomponent", "trie").Logger(),
		root:  nil,
		store: store,
	}

	return &t
}

// NewTrie creates a new trie using the given root node and payload store.
func NewTrie(log zerolog.Logger, root Node, store dps.Store) *Trie {
	t := Trie{
		log:   log.With().Str("subcomponent", "trie").Logger(),
		root:  root,
		store: store,
	}

	return &t
}

// RootNode returns the root node of the trie.
func (t *Trie) RootNode() Node {
	return t.root
}

func (t *Trie) Store() dps.Store {
	return t.store
}

// RootHash returns the hash of the trie's root node.
func (t *Trie) RootHash() ledger.RootHash {
	if t.root == nil {
		return ledger.RootHash(ledger.GetDefaultHashForHeight(ledger.NodeMaxHeight))
	}

	return ledger.RootHash(t.root.Hash())
}

// TODO: Add method to add multiple paths and payloads at once and parallelize insertions that do not conflict.
//  See https://github.com/optakt/flow-dps/issues/517

// Insert adds a new leaf to the trie. While doing so, since the trie is optimized, it might
// restructure the trie by adding new extensions, branches, or even moving other nodes
// to different heights along the way.
func (t *Trie) Insert(path ledger.Path, payload *ledger.Payload) {

	var previous *Node
	current := &t.root
	depth := uint8(0)
	for {
		switch node := (*current).(type) {

		// When the trie is empty, we start with a single root node that is
		// `nil`. This is the basis from which we build out the trie.
		case nil:

			// When we have reached maximum depth, we can simple put the leaf
			// node into this location and we are done with insertion.
			if depth == maxDepth {
				leaf := Leaf{
					hash:    ledger.ComputeCompactValue(hash.Hash(path), payload.Value, 256),
					payload: blake3.Sum256(payload.Value),
				}
				previous = current
				*current = &leaf
				return
			}

			// If we have not reached maximum depth, we have reached a part of
			// the trie that is empty, and we can reach the leaf's insertion
			// path by inserting an extension node for the rest of the path.
			extension := Extension{
				hash:  [32]byte{},
				dirty: true,
				path:  path[:],
				count: maxDepth - depth,
				child: nil,
			}
			previous = current
			current = &(extension.child)
			depth = maxDepth
			continue

		// Most of the nodes in a sparse trie will initially be made up of
		// extension nodes. They skip a part of the path where there are no
		// branches in order to reduce the number of nodes we need to traverse.
		case *Extension:

			// At this point, we count the number of common bits so we can
			// compare it against the number of available bits on the extension.
			available := uint8(len(node.path))
			common := uint8(0)
			for i := depth; i < depth+node.count; i++ {
				if bitutils.Bit(path[:], int(i)) != bitutils.Bit(node.path[:], int(i)) {
					break
				}
				common++
			}

			// If we have all of the bits in common, we have a simple edge case,
			// where we can simple skip to the end of the extension.
			if common == available {
				node.dirty = true
				previous = current
				current = &node.child
				depth = depth + available
				continue
			}

			// Otherwise, we always have to create a fork in the path to our
			// leaf; one of the sides will remain `nil`, which is where we will
			// continue our traversal. The other side will contain whatever is
			// left of the extension node.
			branch := Branch{
				hash:  [32]byte{},
				dirty: true,
			}

			// If we have all but one bit in common, we have the branch on the
			// last bit, so the correct child for the previous extension side
			// of the new branch will point to the previous child of the branch.
			// Otherwise, we need to create a new branch with the remainder of
			// the previous path.
			var other Node
			if available-common == 1 {
				other = node.child
			} else {
				other = &Extension{
					hash:  [32]byte{},
					dirty: true,
					path:  node.path,
					count: available - common - 1,
					child: node.child,
				}
			}

			// If we have no bits in common, the first bit of the extension
			// should be replaced with the branch node, and the extension will
			// be garbage-collected. Otherwise, the extension points to the
			// branch, with a reduced path length.
			if common == 0 {
				*previous = &branch
			} else {
				node.child = &branch
				node.count = common
				node.dirty = true
			}

			// Finally, we just have to point the wrong side of the branch,
			// which we will not follow, back at the previously existing path.
			previous = current
			if bitutils.Bit(node.path, int(common)) == 0 {
				branch.left = other
				current = &branch.right
			} else {
				branch.right = other
				current = &branch.left
			}
			continue

		// Once the trie fills up more, we will have a lot of branch nodes,
		// where there are nodes on both the left and the right side. We can
		// simply continue iteration to the correct side.
		case *Branch:

			// If the key bit at the index i is a 0, move on to the left child,
			// otherwise the right child.
			if bitutils.Bit(path[:], int(depth)) == 0 {
				current = &node.left
			} else {
				current = &node.right
			}
			node.dirty = true
			depth++
			continue

		// Finally, if we find a leaf, it means that the payload inserted at
		// this leaf has probably been updated, so let's overwrite it by simply
		// setting it to nil.
		case *Leaf:

			*current = nil
			continue
		}
	}
}

// Leaves iterates through the trie and returns its leaf nodes.
func (t *Trie) Leaves() []*Leaf {
	queue := deque.New()

	root := t.RootNode()
	if root != nil {
		queue.PushBack(root)
	}

	var leaves []*Leaf
	for queue.Len() > 0 {
		node := queue.PopBack().(Node)
		switch n := node.(type) {
		case *Leaf:
			leaves = append(leaves, n)
		case *Extension:
			queue.PushBack(n.child)
		case *Branch:
			queue.PushBack(n.left)
			queue.PushBack(n.right)
		}
	}

	return leaves
}

// UnsafeRead read payloads for the given paths.
// CAUTION: If a given path is missing from the trie, this function panics.
func (t *Trie) UnsafeRead(paths []ledger.Path) []*ledger.Payload {
	payloads := make([]*ledger.Payload, len(paths)) // pre-allocate slice for the result
	for i := range paths {
		payloads[i] = t.read(paths[i])
	}
	return payloads
}

func (t *Trie) read(path ledger.Path) *ledger.Payload {
	current := &t.root
	depth := uint8(0)
	for {
		switch node := (*current).(type) {
		case *Branch:
			if bitutils.Bit(path[:], int(depth)) == 0 {
				current = &node.left
			} else {
				current = &node.right
			}
			depth++
			continue

		case *Extension:

			available := uint8(len(node.path))
			common := uint8(0)
			for i := depth; i < depth+node.count; i++ {
				if bitutils.Bit(path[:], int(i)) != bitutils.Bit(node.path[:], int(i)) {
					break
				}
				common++
			}
			if available != common {
				return nil
			}

			current = &node.child
			depth += node.count
			continue

		case *Leaf:

			payload, err := t.store.Retrieve(node.Hash())
			if err != nil {
				return nil
			}

			return payload

		case nil:
			return nil
		}
	}
}

// Converts depth into Flow Go inverted height (where 256 is root).
func nodeHeight(depth uint8) uint16 {
	return ledger.NodeMaxHeight - uint16(depth)
}

// commonBits returns the number of matching bits within two paths.
func commonBits(path1, path2 ledger.Path) uint16 {
	for i := uint16(0); i < ledger.NodeMaxHeight; i++ {
		if bitutils.Bit(path1[:], int(i)) != bitutils.Bit(path2[:], int(i)) {
			return i
		}
	}

	return ledger.NodeMaxHeight
}
