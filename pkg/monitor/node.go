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

	// if resp.StatusCode == http.StatusNotFound {
	// 	// prysm
	// 	versionResp, err := n.client.Get(endpoint + "/eth/v1alpha1/node/version")
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	defer versionResp.Body.Close()
	// 	versionData := make(map[string]interface{})
	// 	dec := json.NewDecoder(versionResp.Body)
	// 	err = dec.Decode(&versionData)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	version, ok := versionData["version"].(string)
	// 	if !ok {
	// 		return nil, fmt.Errorf("bad version string")
	// 	}
	// 	n.version = version

	// 	n.id = idHashOf(endpoint)
	// 	return n, nil
	// } else if resp.StatusCode == http.StatusLengthRequired {
	// 	// nimbus
	// 	url := n.endpoint

	// 	request, err := json.Marshal(map[string]interface{}{
	// 		"jsonrpc": "2.0",
	// 		"id":      randomRPCID(),
	// 		"method":  "getNodeVersion",
	// 		"params":  []string{},
	// 	})
	// 	if err != nil {
	// 		return nil, err
	// 	}

	// 	resp, err := n.client.Post(url, "application/json", bytes.NewBuffer(request))
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	defer resp.Body.Close()
	// 	data := make(map[string]interface{})
	// 	dec := json.NewDecoder(resp.Body)
	// 	err = dec.Decode(&data)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	version, ok := data["result"].(string)
	// 	if !ok {
	// 		return nil, fmt.Errorf("bad version string")
	// 	}
	// 	n.version = version

	// 	n.id = idHashOf(endpoint)
	// 	return n, nil
	// }

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
	peerID, ok := inner["peer_id"].(string)
	if !ok {
		return nil, fmt.Errorf("peer id not a string")
	}
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
	syncDistanceStr, ok := inner["sync_distance"].(string)
	if !ok {
		return fmt.Errorf("sync distance not a string")
	}
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

	headBlockRoot, ok := data["headBlockRoot"].(string)
	if !ok {
		return fmt.Errorf("head block root is not a string")
	}

	root, err := decodePrysmRoot(headBlockRoot)
	if err != nil {
		return err
	}

	root = "0x" + root

	if root == n.latestHead.root {
		return nil
	}

	slot, ok := data["headSlot"].(string)
	if !ok {
		return fmt.Errorf("head slot is not a string")
	}

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

	resultData, ok := data["result"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("data is not a map")
	}
	root, ok := resultData["head_block_root"].(string)
	if !ok {
		return fmt.Errorf("head block root is not a string")
	}
	root = "0x" + root
	if root == n.latestHead.root {
		return nil
	}

	slotNumericalFloat, ok := resultData["head_slot"].(float64)
	if !ok {
		return fmt.Errorf("head slot is not a JSON number")
	}
	slotNumerical := int(slotNumericalFloat)
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
	// if isPrysm(n.version) {
	// 	return n.doFetchLatestHeadPrysm()
	// } else if isNimbus(n.version) {
	// 	return n.doFetchLatestHeadNimbus()
	// }

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
	root, ok := respData["root"].(string)
	if !ok {
		return fmt.Errorf("root is not a string")
	}

	if root == n.latestHead.root {
		return nil
	}

	signedHeader, ok := respData["header"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("header is not a map of data")
	}

	header, ok := signedHeader["message"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("inner header message is not a map of data")
	}
	slot := header["slot"].(string)
	if !ok {
		return fmt.Errorf("slot is not a string")
	}

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
	justifiedData, ok := finalityData["current_justified"].(map[string]interface{})
	if !ok {
		err = fmt.Errorf("current justified not a map or missing")
		return
	}
	justified.Epoch, ok = justifiedData["epoch"].(string)
	if !ok {
		err = fmt.Errorf("current justified epoch not a string")
		return
	}
	justified.Root, ok = justifiedData["root"].(string)
	if !ok {
		err = fmt.Errorf("current justified root not a string")
		return
	}
	finalizedData, ok := finalityData["finalized"].(map[string]interface{})
	if !ok {
		err = fmt.Errorf("finalized data not a map or missing")
		return
	}
	finalized.Epoch, ok = finalizedData["epoch"].(string)
	if !ok {
		err = fmt.Errorf("finalized epoch not a string")
		return
	}
	finalized.Root, ok = finalizedData["root"].(string)
	if !ok {
		err = fmt.Errorf("finalized root not a string")
		return
	}
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

	participationData, ok := data["data"].(map[string]interface{})
	if !ok {
		err = fmt.Errorf("participation data not a map or missing")
		return
	}
	currentEpochActiveGwei, ok := participationData["current_epoch_active_gwei"].(float64)
	if !ok {
		err = fmt.Errorf("wrong type for participation data")
		return
	}
	previousEpochActiveGwei, ok := participationData["previous_epoch_active_gwei"].(float64)
	if !ok {
		err = fmt.Errorf("wrong type for participation data")
		return
	}
	currentEpochAttestingGwei, ok := participationData["current_epoch_attesting_gwei"].(float64)
	if !ok {
		err = fmt.Errorf("wrong type for participation data")
		return
	}
	currentEpochTargetAttestingGwei, ok := participationData["current_epoch_target_attesting_gwei"].(float64)
	if !ok {
		err = fmt.Errorf("wrong type for participation data")
		return
	}
	previousEpochAttestingGwei, ok := participationData["previous_epoch_attesting_gwei"].(float64)
	if !ok {
		err = fmt.Errorf("wrong type for participation data")
		return
	}
	previousEpochTargetAttestingGwei, ok := participationData["previous_epoch_target_attesting_gwei"].(float64)
	if !ok {
		err = fmt.Errorf("wrong type for participation data")
		return
	}
	previousEpochHeadAttestingGwei, ok := participationData["previous_epoch_head_attesting_gwei"].(float64)
	if !ok {
		err = fmt.Errorf("wrong type for participation data")
		return
	}

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
