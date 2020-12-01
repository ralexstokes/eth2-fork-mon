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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emicklei/dot"
)

const clientVersionPath = "/eth/v1/node/version"
const nodeIdentityPath = "/eth/v1/node/identity"
const headHeaderPath = "/eth/v1/beacon/headers/head"
const headsPath = "/eth/v1/debug/beacon/heads"
const protoArrayPath = "/lighthouse/proto_array"
const pollingDuration = 1 * time.Second
const slotDuration = 12 * time.Second

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

	if resp.StatusCode == http.StatusNotFound {
		// prysm
		versionResp, err := http.Get(endpoint + "/eth/v1alpha1/node/version")
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

		resp, err := http.Post(url, "application/json", bytes.NewBuffer(request))
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

	return n, nil
}

type Node struct {
	id       string // hash of peer id
	endpoint string
	version  string

	latestHead HeadRef
	isHealthy  bool // node responding?

	// knownHeads []HeadRef
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
	resp, err := http.Get(url)
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

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(request))
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

// func (n *Node) fetchLatestHeads(wg *sync.WaitGroup) {
// 	defer wg.Done()

// 	err := n.doFetchLatestHeads()
// 	if err != nil {
// 		log.Println(err)
// 	}
// }

// // Returns known tips
// func (n *Node) doFetchLatestHeads() error {
// 	url := n.endpoint + headsPath
// 	resp, err := http.Get(url)
// 	if err != nil {
// 		return err
// 	}
// 	defer resp.Body.Close()
// 	headsResp := make(map[string]interface{})
// 	dec := json.NewDecoder(resp.Body)
// 	err = dec.Decode(&headsResp)
// 	if err != nil {
// 		return err
// 	}

// 	headsData := headsResp["data"].([]interface{})

// 	var heads []HeadRef
// 	for _, headData := range headsData {
// 		headData := headData.(map[string]interface{})
// 		slot := headData["slot"].(string)
// 		root := headData["root"].(string)
// 		heads = append(heads, HeadRef{slot, root})
// 	}

// 	n.knownHeads = heads

// 	return nil
// }

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
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	protoArrayResp := ProtoArrayResp{}
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&protoArrayResp)
	return protoArrayResp.Data.Nodes, err
}

type Monitor struct {
	config *Config
	nodes  []*Node

	knownHeadCount int

	forkChoiceSummary         ForkChoiceNode
	forkChoiceTotalWeight     float64
	currentForkChoiceProvider *Node
	forkchoiceLock            sync.Mutex

	errc chan error
}

func (m *Monitor) fetchHeads() error {
	var wg sync.WaitGroup

	lastBlockTreeHead := HeadRef{}
	for _, node := range m.nodes {
		wg.Add(1)
		if node == m.currentForkChoiceProvider {
			lastBlockTreeHead = node.latestHead
		}
		go node.fetchLatestHead(&wg)
	}

	wg.Wait()

	if m.currentForkChoiceProvider != nil {
		if m.currentForkChoiceProvider.latestHead != lastBlockTreeHead {
			err := m.buildLatestForkChoiceSummary()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (m *Monitor) startHeadMonitor() {
	if m.currentForkChoiceProvider != nil {
		err := m.buildLatestForkChoiceSummary()
		if err != nil {
			m.errc <- err
			return
		}
	}
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

// func (m *Monitor) buildBlockTree() {
// 	// slot -> set[root]
// 	allHeads := make(map[int64]map[string]struct{})
// 	earliestSlot := ^int64(0)
// 	lastSlot := int64(-1)

// 	for _, node := range m.nodes {
// 		for _, head := range node.knownHeads {
// 			slotAsSomeInt, _ := strconv.Atoi(head.slot)
// 			slotAsInt := int64(slotAsSomeInt)
// 			if slotAsInt < earliestSlot {
// 				earliestSlot = slotAsInt
// 			}

// 			if slotAsInt > lastSlot {
// 				lastSlot = slotAsInt
// 			}
// 			headsAtSlot, ok := allHeads[slotAsInt]
// 			if !ok {
// 				headsAtSlot = make(map[string]struct{})
// 			}
// 			headsAtSlot[head.root] = struct{}{}
// 			allHeads[slotAsInt] = headsAtSlot
// 		}
// 	}

// 	count := 0
// 	for i := earliestSlot; i <= lastSlot; i++ {
// 		roots, ok := allHeads[i]
// 		if !ok {
// 			continue
// 		}
// 		count += len(roots)
// 	}
// 	m.knownHeadCount = count
// }

// func (m *Monitor) fetchLatestBlockTree() error {
// 	var wg sync.WaitGroup

// 	for _, node := range m.nodes {
// 		wg.Add(1)
// 		go node.fetchLatestHeads(&wg)
// 	}

// 	wg.Wait()

// 	m.buildBlockTree()

// 	return nil
// }

// func (m *Monitor) startBlockTreeMonitor() {
// 	err := m.fetchLatestBlockTree()
// 	if err != nil {
// 		m.errc <- err
// 		return
// 	}

// 	for {
// 		time.Sleep(pollingDuration)

// 		err := m.fetchLatestBlockTree()
// 		if err != nil {
// 			m.errc <- err
// 			return
// 		}
// 	}
// }

func (m *Monitor) buildLatestForkChoiceSummary() error {
	protoArray, err := m.currentForkChoiceProvider.fetchProtoArray()
	if err != nil {
		return err
	}

	root := protoArray[0]
	headIndex := root.BestDescendant
	summary := computeSummary(protoArray, headIndex)

	totalWeight := extractTotalWeight(protoArray)

	m.forkchoiceLock.Lock()
	defer m.forkchoiceLock.Unlock()

	m.forkChoiceSummary = summary
	m.forkChoiceTotalWeight = totalWeight
	return nil
}

func (m *Monitor) startProtoArrayMonitor() {
	if m.currentForkChoiceProvider == nil {
		return
	}

	err := m.buildLatestForkChoiceSummary()
	if err != nil {
		m.errc <- err
		return
	}

	for {
		time.Sleep(slotDuration)

		err := m.buildLatestForkChoiceSummary()
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
	Healthy bool   `json:"healthy"`
}

func (m *Monitor) sendHeads(w http.ResponseWriter, r *http.Request) {
	var resp []nodeResp
	for _, node := range m.nodes {
		resp = append(resp, nodeResp{
			ID:      node.id,
			Version: node.version,
			Slot:    node.latestHead.slot,
			Root:    node.latestHead.root,
			Healthy: node.isHealthy,
		})
	}

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

type ForkChoiceNode struct {
	Children    []ForkChoiceNode `json:"children"`
	Slot        string           `json:"slot"`
	Root        string           `json:"root"`
	Weight      float64          `json:"weight"`
	IsCanonical bool             `json:"is_canonical"`

	CountCollapsedBlocks int `json:"count_collapsed_blocks,omitempty"`

	countOfSubTree int
}

func humanizeRoot(root string) string {
	if len(root) != 66 { // 32 hex + "0x"
		return root
	}
	return fmt.Sprintf("%s..%s", root[2:6], root[len(root)-4:])
}

func (node *ForkChoiceNode) visit(g *dot.Graph, parentNode *dot.Node) {
	n := g.Node(fmt.Sprintf("(%s,%s) (%d)", node.Slot, humanizeRoot(node.Root), node.CountCollapsedBlocks))
	if node.IsCanonical {
		n.Attr("fillcolor", "#fdfd96")
		n.Attr("style", "filled")
	}
	if parentNode != nil {
		n.Edge(*parentNode)
	}
	for _, child := range node.Children {
		child.visit(g, &n)
	}
}

func countTree(node *ForkChoiceNode) int {
	if node.countOfSubTree != 0 {
		return node.countOfSubTree
	}

	for _, child := range node.Children {
		node.countOfSubTree += countTree(&child)
	}

	node.countOfSubTree += len(node.Children)
	return node.countOfSubTree
}

const epochsToSend = 4

func computeCurrentSlot(genesisTime int, secondsPerSlot int) int {
	t := time.Now().Unix()
	secondsSinceGenesis := t - int64(genesisTime)
	return int(secondsSinceGenesis / int64(secondsPerSlot))
}

func pruneForBrowser(node ForkChoiceNode, genesisTime int, slotsPerEpoch int, secondsPerSlot int) ForkChoiceNode {
	currentSlot := computeCurrentSlot(genesisTime, secondsPerSlot)
	currentEpoch := int(currentSlot / slotsPerEpoch)
	targetEpoch := currentEpoch - epochsToSend
	if targetEpoch < 0 {
		targetEpoch = 0
	}

	targetSlot := targetEpoch * slotsPerEpoch
	slot, err := strconv.Atoi(node.Slot)
	if err != nil {
		log.Println(err)
		return node
	}
	for slot < targetSlot {
		if len(node.Children) == 0 {
			return node
		}
		for _, child := range node.Children {
			if child.IsCanonical {
				node = child
				slot, err = strconv.Atoi(node.Slot)
				if err != nil {
					log.Println(err)
					return node
				}
				break
			}
		}
	}

	return node
}

type forkChoiceResponse struct {
	BlockTree   ForkChoiceNode `json:"block_tree"`
	TotalWeight float64        `json:"total_weight"`
}

func (m *Monitor) sendForkChoice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	m.forkchoiceLock.Lock()
	forkChoiceSummary := m.forkChoiceSummary
	totalWeight := m.forkChoiceTotalWeight
	m.forkchoiceLock.Unlock()
	forkChoiceForBrowser := pruneForBrowser(forkChoiceSummary, m.config.Eth2.GenesisTime, m.config.Eth2.SlotsPerEpoch, m.config.Eth2.SecondsPerSlot)

	resp := forkChoiceResponse{
		BlockTree:   forkChoiceForBrowser,
		TotalWeight: totalWeight,
	}

	enc := json.NewEncoder(w)
	err := enc.Encode(resp)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// log.Println("done serving request for fork choice")
}

func buildDOT(root ForkChoiceNode) *dot.Graph {
	g := dot.NewGraph(dot.Directed)

	root.visit(g, nil)

	return g
}

func (m *Monitor) sendForkChoiceAsDOT(w http.ResponseWriter, r *http.Request) {
	m.forkchoiceLock.Lock()
	defer m.forkchoiceLock.Unlock()

	g := buildDOT(m.forkChoiceSummary)
	g.Write(w)
}

var cssFile = regexp.MustCompile(".css$")

func (m *Monitor) serveAPI() {
	http.HandleFunc("/spec", m.sendSpec)
	http.HandleFunc("/heads", m.sendHeads)

	// m.sendBlockTree is WIP...
	// http.HandleFunc("/block-tree", m.sendBlockTree)

	http.HandleFunc("/fork-choice", m.sendForkChoice)
	http.HandleFunc("/fork-choice-dot", m.sendForkChoiceAsDOT)
	clientServer := http.FileServer(http.Dir(m.config.OutputDir))
	clientServerWithMimeType := func(w http.ResponseWriter, r *http.Request) {
		if cssFile.MatchString(r.RequestURI) {
			w.Header().Set("Content-Type", "text/css")
		}
		clientServer.ServeHTTP(w, r)
	}
	http.HandleFunc("/", clientServerWithMimeType)

	log.Println("listening on port 8080...")
	m.errc <- http.ListenAndServe(":8080", nil)
}

func waitUntilNextSlot(genesisTime int, secondsPerSlot int) {
	now := time.Now().Unix()
	currentSlot := int((int(now) - genesisTime) / secondsPerSlot)
	nextSlot := currentSlot + 1
	nextSlotInSeconds := nextSlot * secondsPerSlot
	nextSlotStart := nextSlotInSeconds + genesisTime

	duration := nextSlotStart - int(time.Now().Unix())

	time.Sleep(time.Duration(duration) * time.Second)
}

func (m *Monitor) Serve() error {
	log.Println("synchronizing to next slot")
	waitUntilNextSlot(m.config.Eth2.GenesisTime, m.config.Eth2.SecondsPerSlot)
	log.Println("aligned to slot, continuting")
	go m.startHeadMonitor()
	// go m.startBlockTreeMonitor()
	// NOTE: explicitly do when we fetch a new head
	// go m.startProtoArrayMonitor()
	go m.serveAPI()
	return <-m.errc
}

func FromConfig(config *Config) *Monitor {
	var nodes []*Node
	var forkChoiceProvider *Node
	for _, endpoint := range config.Endpoints {
		node, err := nodeAtEndpoint(endpoint)
		if err != nil {
			log.Println(err)
			continue
		}

		err = node.doFetchLatestHead()
		if err != nil {
			log.Println(err)
			continue
		}
		node.isHealthy = true
		if strings.Contains(node.version, "Lighthouse") {
			forkChoiceProvider = node
		}
		nodes = append(nodes, node)
	}

	if forkChoiceProvider == nil {
		log.Println("warn: no lighthouse node provided so fork choice endpoint will be empty (requires lighthouse protoarray)")
	}

	return &Monitor{config: config, nodes: nodes, currentForkChoiceProvider: forkChoiceProvider, errc: make(chan error)}
}
