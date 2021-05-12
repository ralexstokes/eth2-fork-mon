package monitor

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const clientVersionPath = "/eth/v1/node/version"
const nodeIdentityPath = "/eth/v1/node/identity"
const nodeSyncingPath = "/eth/v1/node/syncing"
const finalityCheckpointsPath = "/eth/v1/beacon/states/head/finality_checkpoints"
const participationPathFmt = "/lighthouse/validator_inclusion/%d/global"

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

func nodeAtEndpoint(endpoint string, eth1 string, msHTTPTimeout time.Duration) (*Node, error) {
	n := &Node{endpoint: endpoint, eth1: eth1}

	// set timeout for all HTTP requests...
	// in particular, Prysm endpoint can be slow...
	n.client.Timeout = msHTTPTimeout * time.Millisecond

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
		version, ok := versionData["version"].(string)
		if !ok {
			return nil, fmt.Errorf("bad version string")
		}
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
	inner, ok := clientResp["data"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("data not a map or missing")
	}
	version, ok := inner["version"].(string)
	if !ok {
		return nil, fmt.Errorf("version not a string")
	}
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
	inner, ok = identityData["data"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("data not a map or missing")
	}
	peerID := inner["peer_id"].(string)
	n.id = idHashOf(peerID)

	err = n.doFetchSyncStatus()
	return n, err
}

func (n *Node) doFetchSyncStatus() error {
	syncResp, err := n.client.Get(n.endpoint + nodeSyncingPath)
	if err != nil {
		return err
	}
	defer syncResp.Body.Close()
	syncData := make(map[string]interface{})
	dec := json.NewDecoder(syncResp.Body)
	err = dec.Decode(&syncData)
	if err != nil {
		return err
	}
	inner, ok := syncData["data"].(map[string]interface{})
	if !ok {
		// error message, likely pre-genesis., just don't change the last status.
		return nil
	}
	if result, ok := inner["is_syncing"].(bool); ok {
		n.isSyncing = result
		return nil
	}
	syncDistanceStr := inner["sync_distance"].(string)
	syncDistance, err := strconv.Atoi(syncDistanceStr)
	if err != nil {
		return err
	}
	n.isSyncing = syncDistance > 1
	return nil
}

type Node struct {
	id       string
	eth1     string
	endpoint string
	version  string

	latestHead HeadRef
	isHealthy  bool // node responding?
	isSyncing  bool

	client http.Client
}

func (n *Node) String() string {
	return fmt.Sprintf("[healthy: %t] %s - %s at %s has head %s", n.isHealthy, n.eth1, n.version, n.endpoint, n.latestHead)
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

	respData, ok := headerResp["data"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("data not a map or missing")
	}
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

type Checkpoint struct {
	Epoch string `json:"epoch"`
	Root  string `json:"root"`
}

func (n *Node) fetchFinalityCheckpoints() (justified Checkpoint, finalized Checkpoint, err error) {
	url := n.endpoint + finalityCheckpointsPath
	resp, err := n.client.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	data := make(map[string]interface{})
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&data)
	if err != nil {
		return
	}

	finalityData, ok := data["data"].(map[string]interface{})
	if !ok {
		err = fmt.Errorf("data not a map or missing")
		return
	}
	justifiedData := finalityData["current_justified"].(map[string]interface{})
	justified.Epoch = justifiedData["epoch"].(string)
	justified.Root = justifiedData["root"].(string)
	finalizedData := finalityData["finalized"].(map[string]interface{})
	finalized.Epoch = finalizedData["epoch"].(string)
	finalized.Root = finalizedData["root"].(string)
	return
}

func (n *Node) doFetchParticipation(epoch int) (current Participation, previous Participation, err error) {
	url := n.endpoint + fmt.Sprintf(participationPathFmt, epoch)
	resp, err := n.client.Get(url)
	if err != nil {
		return
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = errors.New("participation fetch failed")
		return
	}

	defer resp.Body.Close()
	data := make(map[string]interface{})
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&data)
	if err != nil {
		return
	}

	participationData := data["data"].(map[string]interface{})
	currentEpochActiveGwei := participationData["current_epoch_active_gwei"].(float64)
	previousEpochActiveGwei := participationData["previous_epoch_active_gwei"].(float64)
	currentEpochAttestingGwei := participationData["current_epoch_attesting_gwei"].(float64)
	currentEpochTargetAttestingGwei := participationData["current_epoch_target_attesting_gwei"].(float64)
	previousEpochAttestingGwei := participationData["previous_epoch_attesting_gwei"].(float64)
	previousEpochTargetAttestingGwei := participationData["previous_epoch_target_attesting_gwei"].(float64)
	previousEpochHeadAttestingGwei := participationData["previous_epoch_head_attesting_gwei"].(float64)

	current.Epoch = epoch
	current.ParticipationRate = currentEpochAttestingGwei / currentEpochActiveGwei * 100
	current.JustificationRate = currentEpochTargetAttestingGwei / currentEpochActiveGwei * 100

	previous.Epoch = epoch - 1
	previous.ParticipationRate = previousEpochAttestingGwei / previousEpochActiveGwei * 100
	previous.JustificationRate = previousEpochTargetAttestingGwei / previousEpochActiveGwei * 100
	headRate := previousEpochHeadAttestingGwei / previousEpochActiveGwei * 100
	previous.HeadRate = &headRate

	return
}
