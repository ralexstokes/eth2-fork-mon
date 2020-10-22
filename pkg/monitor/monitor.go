package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const clientVersionPath = "/eth/v1/node/version"
const nodeIdentityPath = "/eth/v1/node/identity"
const headHeaderPath = "/eth/v1/beacon/headers/head"
const headsPath = "/eth/v1/debug/beacon/heads"
const pollingDuration = 1 * time.Second

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

	resp, err := http.Get(endpoint + clientVersionPath)
	if err != nil {
		return nil, err
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

	identityResp, err := http.Get(endpoint + nodeIdentityPath)
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

	// load current head
	err = n.doFetchLatestHead()
	return n, err
}

type Node struct {
	id         string // hash of peer id
	endpoint   string
	version    string
	latestHead HeadRef
	knownHeads []HeadRef
}

func (n *Node) String() string {
	return fmt.Sprintf("%s at %s has head %s", n.version, n.endpoint, n.latestHead)
}

func (n *Node) fetchLatestHead(wg *sync.WaitGroup) {
	defer wg.Done()

	err := n.doFetchLatestHead()
	if err != nil {
		log.Println(err)
	}
}

func (n *Node) doFetchLatestHead() error {
	url := n.endpoint + headHeaderPath
	resp, err := http.Get(url)
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

func (n *Node) fetchLatestHeads(wg *sync.WaitGroup) {
	defer wg.Done()

	err := n.doFetchLatestHeads()
	if err != nil {
		log.Println(err)
	}
}

func (n *Node) doFetchLatestHeads() error {
	url := n.endpoint + headsPath
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	headsResp := make(map[string]interface{})
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&headsResp)
	if err != nil {
		return err
	}

	headsData := headsResp["data"].([]interface{})

	var heads []HeadRef
	for _, headData := range headsData {
		headData := headData.(map[string]interface{})
		slot := headData["slot"].(string)
		root := headData["root"].(string)
		heads = append(heads, HeadRef{slot, root})
	}

	n.knownHeads = heads

	return nil
}

type Monitor struct {
	config         *Config
	nodes          []*Node
	knownHeadCount int
	errc           chan error
}

func (m *Monitor) fetchHeads() error {
	var wg sync.WaitGroup

	for _, node := range m.nodes {
		wg.Add(1)
		go node.fetchLatestHead(&wg)
	}

	wg.Wait()
	return nil
}

func (m *Monitor) startHeadMonitor() {
	err := m.fetchHeads()
	if err != nil {
		m.errc <- err
		return
	}

	for {
		time.Sleep(pollingDuration)

		err := m.fetchHeads()
		if err != nil {
			m.errc <- err
			return
		}
	}
}

func (m *Monitor) buildBlockTree() {
	// slot -> set[root]
	allHeads := make(map[int64]map[string]struct{})
	earliestSlot := ^int64(0)
	lastSlot := int64(-1)

	for _, node := range m.nodes {
		for _, head := range node.knownHeads {
			slotAsSomeInt, _ := strconv.Atoi(head.slot)
			slotAsInt := int64(slotAsSomeInt)
			if slotAsInt < earliestSlot {
				earliestSlot = slotAsInt
			}

			if slotAsInt > lastSlot {
				lastSlot = slotAsInt
			}
			headsAtSlot, ok := allHeads[slotAsInt]
			if !ok {
				headsAtSlot = make(map[string]struct{})
			}
			headsAtSlot[head.root] = struct{}{}
			allHeads[slotAsInt] = headsAtSlot
		}
	}

	count := 0
	for i := earliestSlot; i <= lastSlot; i++ {
		roots, ok := allHeads[i]
		if !ok {
			continue
		}
		count += len(roots)
	}
	m.knownHeadCount = count
}

func (m *Monitor) fetchLatestBlockTree() error {
	var wg sync.WaitGroup

	for _, node := range m.nodes {
		wg.Add(1)
		go node.fetchLatestHeads(&wg)
	}

	wg.Wait()

	m.buildBlockTree()

	return nil
}

func (m *Monitor) startBlockTreeMonitor() {
	err := m.fetchLatestBlockTree()
	if err != nil {
		m.errc <- err
		return
	}

	for {
		time.Sleep(pollingDuration)

		err := m.fetchLatestBlockTree()
		if err != nil {
			m.errc <- err
			return
		}
	}
}

func (m *Monitor) sendSpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	enc := json.NewEncoder(w)
	err := enc.Encode(m.config.Eth2)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

type nodeResp struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Slot    string `json:"slot"`
	Root    string `json:"root"`
}

func (m *Monitor) sendHeads(w http.ResponseWriter, r *http.Request) {
	var resp []nodeResp
	for _, node := range m.nodes {
		resp = append(resp, nodeResp{
			ID:      node.id,
			Version: node.version,
			Slot:    node.latestHead.slot,
			Root:    node.latestHead.root,
		})
	}

	resp = append(resp, nodeResp{
		ID:      "0xabc",
		Version: resp[len(resp)-1].Version,
		Slot:    "550334",
		Root:    "0x015919653fb6924f520509fbfda8b54c7b9f5808f39ddfc2c5d560bea416f394",
	})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	enc := json.NewEncoder(w)
	err := enc.Encode(resp)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

type blockTreeResp struct {
	Heads []HeadRef `json:"heads"`
}

func (m *Monitor) sendBlockTree(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	enc := json.NewEncoder(w)
	err := enc.Encode(map[string]int{"head_count": m.knownHeadCount})
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (m *Monitor) serveAPI() {
	http.HandleFunc("/spec", m.sendSpec)
	http.HandleFunc("/heads", m.sendHeads)
	http.HandleFunc("/block-tree", m.sendBlockTree)

	log.Println("listening on port 8080...")
	m.errc <- http.ListenAndServe(":8080", nil)
}

func (m *Monitor) Serve() error {
	go m.startHeadMonitor()
	go m.startBlockTreeMonitor()
	go m.serveAPI()
	return <-m.errc
}

func FromConfig(config *Config) *Monitor {
	var nodes []*Node
	for _, endpoint := range config.Endpoints {
		node, err := nodeAtEndpoint(endpoint)
		if err != nil {
			log.Println(err)
			continue
		}
		nodes = append(nodes, node)
	}

	return &Monitor{config: config, nodes: nodes, errc: make(chan error)}
}
