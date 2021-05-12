package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const headHeaderPath = "/eth/v1/beacon/headers/head"
const protoArrayPath = "/lighthouse/proto_array"
const pollingDuration = 1 * time.Second
const participationEntriesCount = 20

type WeakSubjectivityData struct {
	CurrentEpoch int    `json:"current_epoch"`
	IsSafe       bool   `json:"is_safe"`
	Checkpoint   string `json:"ws_checkpoint"`
	WSPeriod     int    `json:"ws_period"`
}

type Monitor struct {
	config *Config
	nodes  []*Node

	forkChoiceSummary         *ForkChoiceNode
	currentForkChoiceProvider *Node
	forkchoiceLock            sync.Mutex

	participation                []Participation
	currentParticipationProvider *Node
	participationLock            sync.Mutex

	justifiedCheckpoint Checkpoint
	finalizedCheckpoint Checkpoint

	depositContractBalance int

	weakSubjectivityData WeakSubjectivityData
	weakSubjectivityLock sync.Mutex

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
		if node.isSyncing {
			go node.doFetchSyncStatus()
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
			go func() {
				justified, finalized, err := m.currentForkChoiceProvider.fetchFinalityCheckpoints()
				if err != nil {
					log.Println(err)
					return
				}

				m.justifiedCheckpoint = justified
				m.finalizedCheckpoint = finalized
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
		return m.currentForkChoiceProvider.doFetchSyncStatus()
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

// fetchLatestParticipation gets the participation data for the current complete epoch
// NOTE: it is expensive to ask for historical data so we keep a cache of entries for the frontend
func (m *Monitor) fetchLatestParticipation() error {
	currentSlot := computeCurrentSlot(m.config.Eth2.GenesisTime, m.config.Eth2.SecondsPerSlot)
	currentEpoch := int(currentSlot / m.config.Eth2.SlotsPerEpoch)
	// provider only has data for the `targetEpoch` at the latest
	targetEpoch := currentEpoch - 1
	provider := m.currentParticipationProvider
	currentParticipation, previousParticipation, err := provider.doFetchParticipation(targetEpoch)
	if err != nil {
		return err
	}
	m.participationLock.Lock()
	data := m.participation
	if len(data) != 0 {
		if data[len(data)-1].Epoch == previousParticipation.Epoch {
			data[len(data)-1] = previousParticipation
		} else {
			data = append(data, previousParticipation)
		}
	} else {
		data = append(data, previousParticipation)
	}
	data = append(data, currentParticipation)

	if len(data) > participationEntriesCount {
		data = data[1:]
	}

	m.participation = data
	m.participationLock.Unlock()
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
	Eth1    string `json:"eth1"`
	Version string `json:"version"`
	Slot    string `json:"slot"`
	Root    string `json:"root"`
	Healthy bool   `json:"healthy"`
	Syncing *bool  `json:"syncing"`
}

type monitorResp struct {
	Nodes     []nodeResp `json:"nodes"`
	Justified Checkpoint `json:"justified_checkpoint"`
	Finalized Checkpoint `json:"finalized_checkpoint"`
}

func (m *Monitor) sendMonitorState(w http.ResponseWriter, r *http.Request) {
	var nodes []nodeResp
	for _, node := range m.nodes {
		response := nodeResp{
			ID:      node.id,
			Eth1:    node.eth1,
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
		nodes = append(nodes, response)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	resp := monitorResp{
		Nodes:     nodes,
		Justified: m.justifiedCheckpoint,
		Finalized: m.finalizedCheckpoint,
	}

	enc := json.NewEncoder(w)
	err := enc.Encode(&resp)
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

type Participation struct {
	Epoch             int      `json:"epoch"`
	ParticipationRate float64  `json:"participation_rate"`
	JustificationRate float64  `json:"justification_rate"`
	HeadRate          *float64 `json:"head_rate"`
}

type participationResponse struct {
	Data []Participation `json:"data"`
}

func (m *Monitor) sendParticipationData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	m.participationLock.Lock()
	data := make([]Participation, len(m.participation))
	copy(data, m.participation)
	m.participationLock.Unlock()

	sort.Slice(data, func(i, j int) bool { return data[i].Epoch > data[j].Epoch })

	resp := participationResponse{
		Data: data,
	}

	enc := json.NewEncoder(w)
	err := enc.Encode(resp)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (m *Monitor) sendDepositContractData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	resp := make(map[string]int)

	resp["balance"] = m.depositContractBalance

	enc := json.NewEncoder(w)
	err := enc.Encode(resp)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

func (m *Monitor) sendWSData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	m.weakSubjectivityLock.Lock()
	data := m.weakSubjectivityData
	m.weakSubjectivityLock.Unlock()

	enc := json.NewEncoder(w)
	err := enc.Encode(&data)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
}

var cssFile = regexp.MustCompile(".css$")

func (m *Monitor) serveAPI() {
	http.HandleFunc("/spec", m.sendSpec)

	http.HandleFunc("/chain-monitor", m.sendMonitorState)

	http.HandleFunc("/fork-choice", m.sendForkChoice)

	http.HandleFunc("/participation", m.sendParticipationData)

	http.HandleFunc("/deposit-contract", m.sendDepositContractData)

	http.HandleFunc("/ws-data", m.sendWSData)

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
	currentSlot := computeCurrentSlot(genesisTime, secondsPerSlot)
	nextSlot := currentSlot + 1
	nextSlotInSeconds := nextSlot * secondsPerSlot
	nextSlotStart := nextSlotInSeconds + genesisTime

	duration := nextSlotStart - int(time.Now().Unix())

	time.Sleep(time.Duration(duration) * time.Second)
}

func waitUntilNextEpoch(genesisTime int, secondsPerSlot int, slotsPerEpoch int) {
	currentSlot := computeCurrentSlot(genesisTime, secondsPerSlot)
	currentEpoch := int(currentSlot / slotsPerEpoch)
	nextEpoch := currentEpoch + 1
	nextEpochInSeconds := nextEpoch * slotsPerEpoch * secondsPerSlot
	nextSlotStart := nextEpochInSeconds + genesisTime

	duration := nextSlotStart - int(time.Now().Unix())

	time.Sleep(time.Duration(duration) * time.Second)
}

func (m *Monitor) startParticipationPoll() {
	err := m.fetchLatestParticipation()
	if err != nil {
		m.errc <- err
		return
	}

	config := m.config.Eth2
	secondsPerEpoch := config.SecondsPerSlot * config.SlotsPerEpoch
	for {
		time.Sleep(time.Duration(secondsPerEpoch) * time.Second)

		err := m.fetchLatestParticipation()
		if err != nil {
			m.errc <- err
			return
		}
	}
}

const depositContractBalanceURLFmt = "https://api.etherscan.io/api?module=account&action=balance&address=0x00000000219ab540356cBB839Cbe05303d7705Fa&tag=latest&apikey=%s"

func (m *Monitor) updateDepositContractBalance() {
	url := fmt.Sprintf(depositContractBalanceURLFmt, m.config.EtherscanAPIKey)
	resp, err := http.Get(url)
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

	balanceData, ok := data["result"].(string)
	if !ok {
		return
	}

	balance, err := strconv.ParseFloat(balanceData, 64)
	if err != nil {
		return
	}

	roundedBalance := int(balance / math.Pow(10, 18))

	m.depositContractBalance = roundedBalance
}

func (m *Monitor) startDepositContractMonitor() {
	m.updateDepositContractBalance()
	for {
		time.Sleep(30 * time.Minute)

		m.updateDepositContractBalance()
	}
}

func (m *Monitor) getCurrentEpoch() int {
	currentSlot := computeCurrentSlot(m.config.Eth2.GenesisTime, m.config.Eth2.SecondsPerSlot)
	return int(currentSlot / m.config.Eth2.SlotsPerEpoch)
}

func (m *Monitor) updateWSData() error {
	endpoint := m.config.WSProviderEndpoint
	resp, err := http.Get(endpoint)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data := WeakSubjectivityData{}
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&data)
	if err != nil {
		return err
	}

	if data.CurrentEpoch != m.getCurrentEpoch() {
		return errors.New("wrong epoch from ws provider...")
	}

	m.weakSubjectivityLock.Lock()
	m.weakSubjectivityData = data
	m.weakSubjectivityLock.Unlock()
	return nil
}

func (m *Monitor) startWSProviderMonitor() {
	err := m.updateWSData()
	if err != nil {
		log.Println(err)
	}
	for {
		waitUntilNextEpoch(m.config.Eth2.GenesisTime, m.config.Eth2.SecondsPerSlot, m.config.Eth2.SlotsPerEpoch)

		err = m.updateWSData()
		if err != nil {
			log.Println(err)
		}
	}
}

func (m *Monitor) Start() error {
	go func() {
		log.Println("synchronizing to next slot")
		waitUntilNextSlot(m.config.Eth2.GenesisTime, m.config.Eth2.SecondsPerSlot)
		log.Println("aligned to slot, continuting")
		m.startHeadMonitor()
	}()
	go func() {
		if m.currentForkChoiceProvider != nil {
			log.Println("starting participation monitor")
			err := m.fetchLatestParticipation()
			if err != nil {
				log.Println(err)
			}
			waitUntilNextEpoch(m.config.Eth2.GenesisTime, m.config.Eth2.SecondsPerSlot, m.config.Eth2.SlotsPerEpoch)
			m.startParticipationPoll()
		}
	}()
	go func() {
		if m.config.EtherscanAPIKey != "" {
			log.Println("starting deposit contract monitor")
			m.startDepositContractMonitor()
		}
	}()
	go func() {
		if m.config.WSProviderEndpoint != "" {
			log.Println("starting weak subjectivity provider monitor")
			err := m.updateWSData()
			if err != nil {
				log.Println(err)
			}
			waitUntilNextEpoch(m.config.Eth2.GenesisTime, m.config.Eth2.SecondsPerSlot, m.config.Eth2.SlotsPerEpoch)
			m.startWSProviderMonitor()
		}
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
	var participationProvider *Node
	for _, endpoint := range config.Endpoints {
		node, err := nodeAtEndpoint(endpoint.Addr, endpoint.Eth1, time.Duration(config.MillisecondsTimeout))
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
			participationProvider = node
		}
		nodes = append(nodes, node)
	}

	m := &Monitor{config: config, nodes: nodes, currentForkChoiceProvider: forkChoiceProvider, currentParticipationProvider: participationProvider, errc: make(chan error)}

	if m.currentForkChoiceProvider == nil {
		log.Println("warn: no lighthouse node provided so fork choice endpoint will be empty (requires lighthouse protoarray)")
	} else {
		err := m.buildLatestForkChoiceSummary()
		if err != nil {
			log.Println(err)
			forkChoiceProvider = nil
		}
		justified, finalized, err := m.currentForkChoiceProvider.fetchFinalityCheckpoints()
		if err != nil {
			log.Println(err)
		} else {
			m.justifiedCheckpoint = justified
			m.finalizedCheckpoint = finalized
		}
	}

	if m.currentParticipationProvider == nil {
		log.Println("warn: no lighthouse node provided so participation endpoint will be empty (requires lighthouse protoarray)")
	} else {
		err := m.fetchLatestParticipation()
		if err != nil {
			log.Println(err)
			participationProvider = nil
		}
	}

	return m
}
