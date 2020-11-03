package monitor

func buildTree(node ForkChoiceNode, nodes map[string]ForkChoiceNode, childrenIndex map[string][]string) ForkChoiceNode {
	children := childrenIndex[node.Root]

	for _, childRoot := range children {
		emptyChild := nodes[childRoot]
		child := buildTree(emptyChild, nodes, childrenIndex)
		node.Children = append(node.Children, child)
	}

	// NOTE: If there is a single-child extension (recursively) from the
	// "best descendant" in the protoarray, then it is not marked as such
	// and implictly inferred from the fact that there are no forks...
	// Check for this condition here so we capture the full canonical chain
	if len(node.Children) == 1 {
		child := &node.Children[0]
		if node.IsCanonical && !child.IsCanonical {
			child.IsCanonical = true
		}
	}

	return node
}

// Turn the flat proto_array data into a nested block tree
func rollProtoArray(protoArrayData []ProtoArrayNode, canonicalHeadIndex float64) ForkChoiceNode {
	// root to children
	childrenIndex := make(map[string][]string)
	nodes := make(map[string]ForkChoiceNode)
	for _, protoNode := range protoArrayData {
		root := protoNode.Root

		isCanonical := protoNode.BestDescendant == canonicalHeadIndex
		node := ForkChoiceNode{
			Slot:        protoNode.Slot,
			Root:        root,
			Weight:      protoNode.Weight,
			IsCanonical: isCanonical,
		}

		if protoNode.ParentIndex != nil {
			parentIndex := int(*protoNode.ParentIndex)
			parentRoot := protoArrayData[parentIndex].Root
			children, ok := childrenIndex[parentRoot]
			if !ok {
				children = []string{}
			}
			children = append(children, root)
			childrenIndex[parentRoot] = children
		}

		nodes[root] = node
	}

	rootNode := nodes[protoArrayData[0].Root]

	return buildTree(rootNode, nodes, childrenIndex)
}

// Summarize the full block tree by only keeping the root, heads and fork points in between
func compactSingleChildren(node ForkChoiceNode) ForkChoiceNode {
	if len(node.Children) == 1 {
		child := node.Children[0]
		for len(child.Children) == 1 {
			child = child.Children[0]
			node.CountCollapsedBlocks += 1
		}
		node.Children[0] = child
	}
	for i, child := range node.Children {
		node.Children[i] = compactSingleChildren(child)
	}
	return node
}

func computeSummary(protoArrayData []ProtoArrayNode, canonicalHeadIndex float64) ForkChoiceNode {
	blockTree := rollProtoArray(protoArrayData, canonicalHeadIndex)
	return blockTree
	// return compactSingleChildren(blockTree)
}
