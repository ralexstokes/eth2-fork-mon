package monitor

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const clientVersionPath = "/eth/v1/node/version"
const nodeIdentityPath = "/eth/v1/node/identity"
const nodeSyncingPath = "/eth/v1/node/syncing"

type HeadRef struct {
	slot string
	root string
}

func (h HeadRef) String() string {
	return fmt.Sprintf("(%s, %s)", h.slot, h.root)
}

// Return some descriptor unique to the peer.
// NOTE we do not want to expose peer ID to the client for security reasons
// so hide it behind a hash.
func idHashOf(peerID string) string {
	h := sha256.New()
	h.Write([]byte(peerID))
	digest := h.Sum(nil)
	return hex.EncodeToString(digest)[:8]
}

func nodeAtEndpoint(endpoint string) (*Node, error) {
	n := &Node{endpoint: endpoint}

	// set timeout for all HTTP requests...
	// in particular, Prysm endpoint can be slow...
	n.client.Timeout = 800 * time.Millisecond

	resp, err := n.client.Get(endpoint + clientVersionPath)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusNotFound {
		// prysm
		versionResp, err := n.client.Get(endpoint + "/eth/v1alpha1/node/version")
		if err != nil {
			return nil, err
		}
		defer versionResp.Body.Close()
		versionData := make(map[string]interface{})
		dec := json.NewDecoder(versionResp.Body)
		err = dec.Decode(&versionData)
		if err != nil {
			return nil, err
		}
		version := versionData["version"].(string)
		n.version = version

		n.id = idHashOf(endpoint)
		return n, nil
	} else if resp.StatusCode == http.StatusLengthRequired {
		// nimbus
		url := n.endpoint

		request, err := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      randomRPCID(),
			"method":  "getNodeVersion",
			"params":  []string{},
		})
		if err != nil {
			return nil, err
		}

		resp, err := n.client.Post(url, "application/json", bytes.NewBuffer(request))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		data := make(map[string]interface{})
		dec := json.NewDecoder(resp.Body)
		err = dec.Decode(&data)
		if err != nil {
			return nil, err
		}
		version := data["result"].(string)
		n.version = version

		n.id = idHashOf(endpoint)
		return n, nil
	}

	defer resp.Body.Close()
	clientResp := make(map[string]interface{})
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&clientResp)
	if err != nil {
		return nil, err
	}
	inner := clientResp["data"].(map[string]interface{})
	version := inner["version"].(string)
	n.version = version

	identityResp, err := n.client.Get(endpoint + nodeIdentityPath)
	if err != nil {
		return nil, err
	}
	defer identityResp.Body.Close()
	identityData := make(map[string]interface{})
	dec = json.NewDecoder(identityResp.Body)
	err = dec.Decode(&identityData)
	if err != nil {
		return nil, err
	}
	inner = identityData["data"].(map[string]interface{})
	peerID := inner["peer_id"].(string)
	n.id = idHashOf(peerID)

	syncResp, err := n.client.Get(endpoint + nodeSyncingPath)
	if err != nil {
		return nil, err
	}
	defer syncResp.Body.Close()
	syncData := make(map[string]interface{})
	dec = json.NewDecoder(syncResp.Body)
	err = dec.Decode(&syncData)
	if err != nil {
		return nil, err
	}
	inner = syncData["data"].(map[string]interface{})
	isSyncing := inner["is_syncing"].(bool)
	n.isSyncing = isSyncing

	return n, nil
}

type Node struct {
	id       string
	endpoint string
	version  string

	latestHead HeadRef
	isHealthy  bool // node responding?
	isSyncing  bool

	client http.Client
}

func (n *Node) String() string {
	return fmt.Sprintf("[healthy: %t] %s at %s has head %s", n.isHealthy, n.version, n.endpoint, n.latestHead)
}

func isPrysm(identifier string) bool {
	return strings.Contains(strings.ToLower(identifier), "prysm")
}

func isNimbus(identifier string) bool {
	return strings.Contains(strings.ToLower(identifier), "nimbus")
}

func decodePrysmRoot(rootAsB64 string) (string, error) {
	rootData, err := base64.StdEncoding.DecodeString(rootAsB64)
	if err != nil {
		return "", err
	}

	return hex.EncodeToString(rootData), nil
}

func (n *Node) doFetchLatestHeadPrysm() error {
	url := n.endpoint + "/eth/v1alpha1/beacon/chainhead"
	resp, err := n.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data := make(map[string]interface{})
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&data)
	if err != nil {
		return err
	}

	root, err := decodePrysmRoot(data["headBlockRoot"].(string))
	if err != nil {
		return err
	}

	root = "0x" + root

	if root == n.latestHead.root {
		return nil
	}

	slot := data["headSlot"].(string)

	// This API can be slow, so if we get an old response,
	// just drop it
	if slot < n.latestHead.slot {
		return nil
	}

	n.latestHead = HeadRef{slot, root}
	return nil
}

func randomRPCID() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "id"
	}
	return hex.EncodeToString(bytes)
}

func (n *Node) doFetchLatestHeadNimbus() error {
	url := n.endpoint

	request, err := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      randomRPCID(),
		"method":  "getChainHead",
		"params":  []string{},
	})
	if err != nil {
		return err
	}

	resp, err := n.client.Post(url, "application/json", bytes.NewBuffer(request))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data := make(map[string]interface{})
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&data)
	if err != nil {
		return err
	}

	resultData := data["result"].(map[string]interface{})
	root := resultData["head_block_root"].(string)
	root = "0x" + root
	if root == n.latestHead.root {
		return nil
	}

	slotNumerical := int(resultData["head_slot"].(float64))
	slot := fmt.Sprintf("%d", slotNumerical)

	n.latestHead = HeadRef{slot, root}
	return nil
}

func (n *Node) fetchLatestHead(wg *sync.WaitGroup) {
	defer wg.Done()

	err := n.doFetchLatestHead()
	if err != nil {
		log.Println(err)
		n.isHealthy = false
	} else {
		n.isHealthy = true
	}
}

func (n *Node) doFetchLatestHead() error {
	if isPrysm(n.version) {
		return n.doFetchLatestHeadPrysm()
	} else if isNimbus(n.version) {
		return n.doFetchLatestHeadNimbus()
	}

	url := n.endpoint + headHeaderPath
	resp, err := n.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	headerResp := make(map[string]interface{})
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&headerResp)
	if err != nil {
		return err
	}

	respData := headerResp["data"].(map[string]interface{})
	root := respData["root"].(string)

	if root == n.latestHead.root {
		return nil
	}

	signedHeader := respData["header"].(map[string]interface{})
	header := signedHeader["message"].(map[string]interface{})
	slot := header["slot"].(string)

	n.latestHead = HeadRef{slot, root}
	return nil
}

type ProtoArrayResp struct {
	Data struct {
		Nodes []ProtoArrayNode `json:"nodes"`
	} `json:"data"`
}

type ProtoArrayNode struct {
	Slot           string   `json:"slot"`
	Root           string   `json:"root"`
	ParentIndex    *float64 `json:"parent"`
	Weight         float64  `json:"weight"`
	BestDescendant float64  `json:"best_descendant"`
}

func (n *Node) fetchProtoArray() ([]ProtoArrayNode, error) {
	url := n.endpoint + protoArrayPath
	resp, err := n.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	protoArrayResp := ProtoArrayResp{}
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&protoArrayResp)
	return protoArrayResp.Data.Nodes, err
}
