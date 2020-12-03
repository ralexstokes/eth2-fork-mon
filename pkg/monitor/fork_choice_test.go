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
