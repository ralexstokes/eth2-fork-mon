package monitor

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const headHeaderPath = "/eth/v1/beacon/headers/head"
const protoArrayPath = "/lighthouse/proto_array"
const pollingDuration = 1 * time.Second

type Monitor struct {
	config *Config
	nodes  []*Node

	forkChoiceSummary         *ForkChoiceNode
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
			go func() {
				err := m.buildLatestForkChoiceSummary()
				if err != nil {
					log.Println(err)
				}
			}()
		}
	}

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

func (m *Monitor) buildLatestForkChoiceSummary() error {
	if m.currentForkChoiceProvider.isSyncing {
		return nil
	}

	protoArray, err := m.currentForkChoiceProvider.fetchProtoArray()
	if err != nil {
		return err
	}

	root := protoArray[0]
	headIndex := root.BestDescendant
	summary := computeSummary(protoArray, headIndex)

	m.forkchoiceLock.Lock()
	defer m.forkchoiceLock.Unlock()
	m.forkChoiceSummary = &summary

	return nil
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
	Syncing *bool `json:"syncing"`
}

func (m *Monitor) sendHeads(w http.ResponseWriter, r *http.Request) {
	var resp []nodeResp
	for _, node := range m.nodes {
		response := nodeResp{
			ID:      node.id,
			Version: node.version,
			Slot:    node.latestHead.slot,
			Root:    node.latestHead.root,
			Healthy: node.isHealthy,
			Syncing: &node.isSyncing,
		}
		if isPrysm(response.Version) {
			response.Syncing = nil
		}
		if isNimbus(response.Version) {
			response.Syncing = nil
		}
		resp = append(resp, response)
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

type ForkChoiceNode struct {
	Children    []ForkChoiceNode `json:"children"`
	Slot        string           `json:"slot"`
	Root        string           `json:"root"`
	Weight      float64          `json:"weight"`
	IsCanonical bool             `json:"is_canonical"`
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
	BlockTree ForkChoiceNode `json:"block_tree"`
}

func (m *Monitor) sendForkChoice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	m.forkchoiceLock.Lock()
	forkChoiceSummary := m.forkChoiceSummary
	m.forkchoiceLock.Unlock()

	resp := forkChoiceResponse{}
	if forkChoiceSummary != nil {
		forkChoiceForBrowser := pruneForBrowser(*forkChoiceSummary, m.config.Eth2.GenesisTime, m.config.Eth2.SlotsPerEpoch, m.config.Eth2.SecondsPerSlot)
		resp.BlockTree = forkChoiceForBrowser
	}

	enc := json.NewEncoder(w)
	err := enc.Encode(resp)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

var cssFile = regexp.MustCompile(".css$")

func (m *Monitor) serveAPI() {
	http.HandleFunc("/spec", m.sendSpec)

	http.HandleFunc("/chain-monitor", m.sendHeads)

	http.HandleFunc("/fork-choice", m.sendForkChoice)

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

func (m *Monitor) Start() error {
	go func() {
		log.Println("synchronizing to next slot")
		waitUntilNextSlot(m.config.Eth2.GenesisTime, m.config.Eth2.SecondsPerSlot)
		log.Println("aligned to slot, continuting")
		m.startHeadMonitor()
	}()
	return nil
}

func (m *Monitor) Serve() error {
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

	m := &Monitor{config: config, nodes: nodes, currentForkChoiceProvider: forkChoiceProvider, errc: make(chan error)}

	if forkChoiceProvider == nil {
		log.Println("warn: no lighthouse node provided so fork choice endpoint will be empty (requires lighthouse protoarray)")
	} else {
		err := m.buildLatestForkChoiceSummary()
		if err != nil {
			log.Println(err)
			forkChoiceProvider = nil
		}
	}

	return m
}
