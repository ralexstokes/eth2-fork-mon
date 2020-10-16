package monitor

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const clientVersionPath = "/eth/v1/node/version"
const nodeIdentityPath = "/eth/v1/node/identity"
const headHeaderPath = "/eth/v1/beacon/headers/head"
const pollingDuration = 1 * time.Second

type HeadRef struct {
	slot string
	root string
}

func (h HeadRef) String() string {
	return fmt.Sprintf("(%s, %s)", h.slot, h.root)
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
	n.peerID = peerID

	// load current head
	err = n.doFetchLatestHead()
	return n, err
}

type Node struct {
	peerID     string
	endpoint   string
	version    string
	latestHead HeadRef
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

type Monitor struct {
	config *Config
	nodes  []*Node
	errc   chan error
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

// func (m *Monitor) startTipMonitor() {
// 	// monitor all tips
// 	// every so often, get all _heads_, collate into batch list
// 	// of (slot, root)
// 	// dedupe, provide upon request
// 	// render list
// }

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
			ID:      node.peerID,
			Version: node.version,
			Slot:    node.latestHead.slot,
			Root:    node.latestHead.root,
		})
	}
	enc := json.NewEncoder(w)
	err := enc.Encode(resp)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
}

func (m *Monitor) sendSpec(w http.ResponseWriter, r *http.Request) {
	enc := json.NewEncoder(w)
	err := enc.Encode(m.config.Eth2)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
}

func (m *Monitor) serveAPI() {
	http.HandleFunc("/heads", m.sendHeads)

	http.HandleFunc("/spec", m.sendSpec)

	log.Println("listening on port 8080...")
	m.errc <- http.ListenAndServe(":8080", nil)
}

func (m *Monitor) Serve() error {
	go m.startHeadMonitor()
	// go m.startTipMonitor()
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
