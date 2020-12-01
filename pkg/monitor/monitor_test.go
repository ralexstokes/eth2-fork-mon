package monitor

import (
	"testing"
)

func TestPrysmRootDecode(t *testing.T) {
	b64RootStr := "c/e6JLzMQRYQ/sNcwkP8yza/1cqnGTudL3uuLs6AlVM="
	root, err := decodePrysmRoot(b64RootStr)
	if err != nil {
		t.Error(err)
	}

	if root != "73f7ba24bccc411610fec35cc243fccb36bfd5caa7193b9d2f7bae2ece809553" {
		t.Error("decode failed")
	}
}
