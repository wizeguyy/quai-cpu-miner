package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/INFURA/go-ethlibs/jsonrpc"

	"github.com/TwiN/go-color"
	"github.com/dominant-strategies/go-quai/common"
	"github.com/dominant-strategies/go-quai/consensus/blake3pow"
	"github.com/dominant-strategies/go-quai/core/types"
	"github.com/dominant-strategies/go-quai/quaiclient/ethclient"
	"github.com/dominant-strategies/quai-cpu-miner/util"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10
	maxRetryDelay   = 60 * 60 * 4 // 4 hours
	USER_AGENT_VER  = "0.1"
)

var (
	exit = make(chan bool)
)

type Miner struct {
	// Miner config object
	config util.Config

	// Blake3pow consensus engine used to seal a block
	engine *blake3pow.Blake3pow

	// Current header to mine
	header *types.Header

	// RPC client connection to mining proxy
	proxyClient *util.MinerSession

	// RPC client connections to the Quai nodes
	sliceClients SliceClients

	// Channel to receive header updates
	updateCh chan *types.Header

	// Channel to submit completed work
	resultCh chan *types.Header

	// Track previous block number for pretty printing
	previousNumber [common.HierarchyDepth]uint64

	// Tracks the latest JSON RPC ID to send to the proxy or node.
	latestId uint64
}

// Clients for RPC connection to the Prime, region, & zone ports belonging to the
// slice we are actively mining
type SliceClients [common.HierarchyDepth]*ethclient.Client

// Creates a MinerSession object that is connected to the single proxy node.
func connectToProxy(config util.Config) *util.MinerSession {
	proxyConnected := false
	var client *util.MinerSession
	var err error
	for !proxyConnected {
		if config.ProxyURL != "" && !proxyConnected {
			client, err = util.NewMinerConn(config.ProxyURL)
			if err != nil {
				log.Println("Unable to connect to proxy: ", config.ProxyURL)
			} else {
				proxyConnected = true
			}
		}
	}
	return client
}

// connectToSlice takes in a config and retrieves the Prime, Region, and Zone client
// that is used for mining in a slice.
func connectToSlice(config util.Config) SliceClients {
	var err error
	loc := config.Location
	clients := SliceClients{}
	primeConnected := false
	regionConnected := false
	zoneConnected := false
	for !primeConnected || !regionConnected || !zoneConnected {
		if config.PrimeURL != "" && !primeConnected {
			clients[common.PRIME_CTX], err = ethclient.Dial(config.PrimeURL)
			if err != nil {
				log.Println("Unable to connect to node:", "Prime", config.PrimeURL)
			} else {
				primeConnected = true
			}
		}
		if config.RegionURLs[loc.Region()] != "" && !regionConnected {
			clients[common.REGION_CTX], err = ethclient.Dial(config.RegionURLs[loc.Region()])
			if err != nil {
				log.Println("Unable to connect to node:", "Region", config.RegionURLs[loc.Region()])
			} else {
				regionConnected = true
			}
		}
		if config.ZoneURLs[loc.Region()][loc.Zone()] != "" && !zoneConnected {
			clients[common.ZONE_CTX], err = ethclient.Dial(config.ZoneURLs[loc.Region()][loc.Zone()])
			if err != nil {
				log.Println("Unable to connect to node:", "Zone", config.ZoneURLs[loc.Region()][loc.Zone()])
			} else {
				zoneConnected = true
			}
		}
	}
	return clients
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func main() {
	// Load config
	config, err := util.LoadConfig("..")
	if err != nil {
		log.Print("Could not load config: ", err)
		return
	}
	// Parse mining location from args
	if len(os.Args) > 2 {
		raw := os.Args[1:3]
		region, _ := strconv.Atoi(raw[0])
		zone, _ := strconv.Atoi(raw[1])
		config.Location = common.Location{byte(region), byte(zone)}
	}
	// Build manager config
	blake3Config := blake3pow.Config{
		NotifyFull: true,
	}
	blake3Engine := blake3pow.New(blake3Config, nil, false)
	m := &Miner{
		config:         config,
		engine:         blake3Engine,
		header:         types.EmptyHeader(),
		updateCh:       make(chan *types.Header, resultQueueSize),
		resultCh:       make(chan *types.Header, resultQueueSize),
		previousNumber: [common.HierarchyDepth]uint64{0, 0, 0},
	}
	log.Println("Starting pprof server")
	EnablePprof(config.Location)
	log.Println("Starting Quai cpu miner in location ", config.Location)
	if config.Proxy {
		m.proxyClient = connectToProxy(config)
		go m.fetchPendingHeaderProxy()
		go m.startProxyListener()
		go m.subscribeProxy()
	} else {
		m.sliceClients = connectToSlice(config)
		go m.fetchPendingHeaderNode()
		// No separate call needed to start listeners.
		go m.subscribeNode()
	}
	go m.resultLoop()
	go m.miningLoop()
	go m.hashratePrinter()
	<-exit
}

// subscribeProxy subscribes to the head of the mining nodes in order to pass
// the most up to date block to the miner within the manager.
func (m *Miner) subscribeProxy() error {
	address := m.config.RewardAddress
	password := m.config.Password

	msg, err := jsonrpc.MakeRequest(int(m.incrementLatestID()), "quai_submitLogin", address, password)
	if err != nil {
		log.Fatalf("Unable to create login request: %v", err)
	}

	return m.proxyClient.SendTCPRequest(*msg)
}

func (m *Miner) startProxyListener() {
	m.proxyClient.ListenTCP(m.updateCh)
}

// Subscribes to the zone node in order to get pending header updates.
func (m *Miner) subscribeNode() {
	if _, err := m.sliceClients[common.ZONE_CTX].SubscribePendingHeader(context.Background(), m.updateCh); err != nil {
		log.Fatal("Failed to subscribe to pending header events", err)
	}
}

// Gets the latest pending header from the proxy.
// This only runs upon initialization, further proxy pending headers are received in listenTCP.
func (m *Miner) fetchPendingHeaderProxy() {
	retryDelay := 1 // Start retry at 1 second
	for {
		msg, err := jsonrpc.MakeRequest(int(m.incrementLatestID()), "quai_getPendingHeader", nil)
		if err != nil {
			log.Fatalf("Unable to make pending header request: %v", err)
		}
		err = m.proxyClient.SendTCPRequest(*msg)
		header := <-m.updateCh

		if err != nil {
			log.Println("Pending block not found error: ", err)
			time.Sleep(time.Duration(retryDelay) * time.Second)
			retryDelay *= 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		} else {
			m.updateCh <- header
			break
		}
	}
}

// Gets the latest pending header from the zone client.
func (m *Miner) fetchPendingHeaderNode() {
	retryDelay := 1 // Start retry at 1 second
	for {
		header, err := m.sliceClients[common.ZONE_CTX].GetPendingHeader(context.Background())
		if err != nil {
			log.Println("Pending block not found error: ", err)
			time.Sleep(time.Duration(retryDelay) * time.Second)
			retryDelay *= 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		} else {
			m.updateCh <- header
			break
		}
	}
}

// miningLoop iterates on a new header and passes the result to m.resultCh. The result is called within the method.
func (m *Miner) miningLoop() error {
	var (
		stopCh chan struct{}
	)
	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case header := <-m.updateCh:
			// Mine the header here
			// Return the valid header with proper nonce and mix digest
			// Interrupt previous sealing operation
			interrupt()
			stopCh = make(chan struct{})
			number := [common.HierarchyDepth]uint64{header.NumberU64(common.PRIME_CTX), header.NumberU64(common.REGION_CTX), header.NumberU64(common.ZONE_CTX)}
			primeStr := fmt.Sprint(number[common.PRIME_CTX])
			regionStr := fmt.Sprint(number[common.REGION_CTX])
			zoneStr := fmt.Sprint(number[common.ZONE_CTX])
			if number != m.previousNumber {
				if number[common.PRIME_CTX] != m.previousNumber[common.PRIME_CTX] {
					primeStr = color.Ize(color.Red, primeStr)
					regionStr = color.Ize(color.Red, regionStr)
					zoneStr = color.Ize(color.Red, zoneStr)
				} else if number[common.REGION_CTX] != m.previousNumber[common.REGION_CTX] {
					regionStr = color.Ize(color.Yellow, regionStr)
					zoneStr = color.Ize(color.Yellow, zoneStr)
				} else if number[common.ZONE_CTX] != m.previousNumber[common.ZONE_CTX] {
					zoneStr = color.Ize(color.Blue, zoneStr)
				}
				log.Println("Mining Block: ", fmt.Sprintf("[%s %s %s]", primeStr, regionStr, zoneStr), "location", header.Location(), "difficulty", header.Difficulty())
			}
			m.previousNumber = [common.HierarchyDepth]uint64{header.NumberU64(common.PRIME_CTX), header.NumberU64(common.REGION_CTX), header.NumberU64(common.ZONE_CTX)}
			header.SetTime(uint64(time.Now().Unix()))
			if err := m.engine.Seal(header, m.resultCh, stopCh); err != nil {
				log.Println("Block sealing failed", "err", err)
			}
		}
	}
}

// WatchHashRate is a simple method to watch the hashrate of our miner and log the output.
func (m *Miner) hashratePrinter() {
	ticker := time.NewTicker(60 * time.Second)
	toSiUnits := func(hr float64) (float64, string) {
		reduced := hr
		order := 0
		for {
			if reduced >= 1000 {
				reduced /= 1000
				order += 3
			} else {
				break
			}
		}
		switch order {
		case 3:
			return reduced, "Kh/s"
		case 6:
			return reduced, "Mh/s"
		case 9:
			return reduced, "Gh/s"
		case 12:
			return reduced, "Th/s"
		default:
			// If reduction didn't work, just return the original
			return hr, "h/s"
		}
	}
	for {
		select {
		case <-ticker.C:
			hashRate := m.engine.Hashrate()
			hr, units := toSiUnits(hashRate)
			log.Println("Current hashrate: ", hr, units)
		}
	}
}

// resultLoop takes in the result and passes to the proper channels for receiving.
func (m *Miner) resultLoop() {
	for {
		select {
		case header := <-m.resultCh:
			_, order, err := m.engine.CalcOrder(header)
			if err != nil {
				log.Println("Mined block had invalid order")
				return
			}
			if !m.config.Proxy {
				for i := common.HierarchyDepth - 1; i >= order; i-- {
					err := m.sendMinedHeaderNodes(i, header)
					if err != nil {
						// Go back to waiting on the next block.
						fmt.Errorf("error submitting block to context %d: %v", order, err)
						continue
					}
				}
			} else {
				// Proxy miner only needs to send to the proxy (stored at zone context).
				go m.sendMinedHeaderProxy(header)
			}
			switch order {
			case common.PRIME_CTX:
				log.Println(color.Ize(color.Red, "PRIME block : "), header.NumberArray(), header.Hash())
			case common.REGION_CTX:
				log.Println(color.Ize(color.Yellow, "REGION block: "), header.NumberArray(), header.Hash())
			case common.ZONE_CTX:
				log.Println(color.Ize(color.Blue, "ZONE block  : "), header.NumberArray(), header.Hash())
			}
		}
	}
}

// Sends the mined header to the proxy.
func (m *Miner) sendMinedHeaderProxy(header *types.Header) error {
	retryDelay := 1 // Start retry at 1 second
	for {
		header_req, err := jsonrpc.MakeRequest(int(m.incrementLatestID()), "quai_receiveMinedHeader", header.RPCMarshalHeader())
		if err != nil {
			log.Fatalf("Could not create json message with header: %v", err)
			return err
		}

		err = m.proxyClient.SendTCPRequest(*header_req)
		if err != nil {
			log.Printf("Unable to send pending header to node: %v", err)
			time.Sleep(time.Duration(retryDelay) * time.Second)
			retryDelay *= 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		} else {
			break
		}
		log.Println("Sent mined header")
	}
	return nil
}

// Sends the mined header to its mining client.
func (m *Miner) sendMinedHeaderNodes(order int, header *types.Header) error {
	return m.sliceClients[order].ReceiveMinedHeader(context.Background(), header)
}

// Used for sequencing JSON RPC messages.
func (m *Miner) incrementLatestID() uint64 {
	cur := m.latestId
	m.latestId += 1
	return cur
}

func EnablePprof(location common.Location) {
	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)
	var port string
	switch {
	// Prime
	case location.Context() == common.PRIME_CTX:
		port = "21000"
		// Regions
	case location.Context() == common.REGION_CTX && location.Region() == 0:
		port = "22000"
	case location.Context() == common.REGION_CTX && location.Region() == 1:
		port = "22001"
	case location.Context() == common.REGION_CTX && location.Region() == 2:
		port = "22002"
	// Zones
	case location.Region() == 0 && location.Zone() == 0:
		port = "23000"
	case location.Region() == 0 && location.Zone() == 1:
		port = "23001"
	case location.Region() == 0 && location.Zone() == 2:
		port = "23002"
	case location.Region() == 1 && location.Zone() == 0:
		port = "23100"
	case location.Region() == 1 && location.Zone() == 1:
		port = "23101"
	case location.Region() == 1 && location.Zone() == 2:
		port = "23102"
	case location.Region() == 2 && location.Zone() == 0:
		port = "23200"
	case location.Region() == 2 && location.Zone() == 1:
		port = "23201"
	case location.Region() == 2 && location.Zone() == 2:
		port = "23202"
	}
	go func() {
		http.ListenAndServe("localhost:"+port, nil)
	}()
}
