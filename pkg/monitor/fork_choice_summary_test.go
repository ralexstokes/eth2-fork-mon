package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"
)

func TestHumanizeRoot(t *testing.T) {
	root := fmt.Sprintf("0x%s", hash("a"))
	if humanizeRoot(root) != "ca97..48bb" {
		t.Error("humanize root is broken")
	}
}

func hash(input string) string {
	h := sha256.New()
	h.Write([]byte(input))
	digest := h.Sum(nil)
	return hex.EncodeToString(digest)
}

func TestCanBuildTree(t *testing.T) {
	firstIndex := float64(0)
	secondIndex := float64(1)
	protoArrayData := []ProtoArrayNode{
		{Slot: "0", Root: hash("0")},
		{Slot: "1", Root: hash("1"), ParentIndex: &firstIndex},
		{Slot: "2", Root: hash("2"), ParentIndex: &secondIndex},
		{Slot: "3", Root: hash("3"), ParentIndex: &firstIndex},
	}

	tree := rollProtoArray(protoArrayData, 3)

	expectedTree := ForkChoiceNode{
		Children: []ForkChoiceNode{
			{
				Children: []ForkChoiceNode{
					{Slot: "2", Root: hash("2")},
				},
				Slot: "1",
				Root: hash("1"),
			},
			{
				Slot: "3",
				Root: hash("3"),
			},
		},
		Slot: "0",
		Root: hash("0"),
	}

	if !reflect.DeepEqual(tree, expectedTree) {
		t.Log(tree)
		t.Error("did not compute the expected tree")
	}
}

func TestCanCompactSingleChildren(t *testing.T) {
	var tests = []struct {
		input  ForkChoiceNode
		output ForkChoiceNode
	}{
		{
			ForkChoiceNode{Slot: "0"},
			ForkChoiceNode{Slot: "0"},
		},
		{
			ForkChoiceNode{Slot: "0", Children: []ForkChoiceNode{
				{Slot: "1"},
			}},
			ForkChoiceNode{Slot: "0", Children: []ForkChoiceNode{
				{Slot: "1"},
			}},
		},
		{
			ForkChoiceNode{Slot: "0", Children: []ForkChoiceNode{
				{Slot: "1", Children: []ForkChoiceNode{
					{Slot: "2", Children: []ForkChoiceNode{
						{Slot: "3"},
					}},
				}},
			}},
			ForkChoiceNode{Slot: "0", Children: []ForkChoiceNode{
				{Slot: "3"},
			}, CountCollapsedBlocks: 2},
		},
		{
			ForkChoiceNode{
				Slot: "0",
				Children: []ForkChoiceNode{
					{
						Slot: "1",
						Children: []ForkChoiceNode{
							{Children: []ForkChoiceNode{
								{Slot: "4"},
							}, Slot: "3"},
						},
					},
					{
						Slot: "2",
						Children: []ForkChoiceNode{
							{
								Slot: "5",
								Children: []ForkChoiceNode{
									{Slot: "6",
										Children: []ForkChoiceNode{
											{Slot: "7"},
											{Slot: "8",
												Children: []ForkChoiceNode{
													{Slot: "9",
														Children: []ForkChoiceNode{
															{Slot: "10"},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			ForkChoiceNode{
				Slot: "0",
				Children: []ForkChoiceNode{
					{
						Slot: "1",
						Children: []ForkChoiceNode{
							{Slot: "4"},
						},
						CountCollapsedBlocks: 1,
					},
					{
						Slot: "2",
						Children: []ForkChoiceNode{
							{Slot: "6",
								Children: []ForkChoiceNode{
									{Slot: "7"},
									{Slot: "8",
										Children: []ForkChoiceNode{
											{Slot: "10"},
										},
										CountCollapsedBlocks: 1,
									},
								},
							},
						},
						CountCollapsedBlocks: 1,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		result := compactSingleChildren(tt.input)
		if !reflect.DeepEqual(result, tt.output) {
			t.Errorf("wanted %v but got %v", tt.output, result)
		}
	}
}
