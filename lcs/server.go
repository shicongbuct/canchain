package lcs

import (
	"crypto/ecdsa"
	"encoding/binary"
	"math"
	"sync"

	"github.com/5uwifi/canchain/can"
	"github.com/5uwifi/canchain/candb"
	"github.com/5uwifi/canchain/common"
	"github.com/5uwifi/canchain/kernel"
	"github.com/5uwifi/canchain/kernel/rawdb"
	"github.com/5uwifi/canchain/kernel/types"
	"github.com/5uwifi/canchain/lcs/flowcontrol"
	"github.com/5uwifi/canchain/lib/log4j"
	"github.com/5uwifi/canchain/lib/p2p"
	"github.com/5uwifi/canchain/lib/p2p/discv5"
	"github.com/5uwifi/canchain/lib/rlp"
	"github.com/5uwifi/canchain/light"
	"github.com/5uwifi/canchain/params"
)

type LcsServer struct {
	lcsCommons

	fcManager   *flowcontrol.ClientManager
	fcCostStats *requestCostStats
	defParams   *flowcontrol.ServerParams
	lcsTopics   []discv5.Topic
	privateKey  *ecdsa.PrivateKey
	quitSync    chan struct{}
}

func NewLcsServer(can *can.CANChain, config *can.Config) (*LcsServer, error) {
	quitSync := make(chan struct{})
	pm, err := NewProtocolManager(can.BlockChain().Config(), light.DefaultServerIndexerConfig, false, config.NetworkId, can.EventMux(), can.Engine(), newPeerSet(), can.BlockChain(), can.TxPool(), can.ChainDb(), nil, nil, nil, quitSync, new(sync.WaitGroup))
	if err != nil {
		return nil, err
	}

	lcsTopics := make([]discv5.Topic, len(AdvertiseProtocolVersions))
	for i, pv := range AdvertiseProtocolVersions {
		lcsTopics[i] = lcsTopic(can.BlockChain().Genesis().Hash(), pv)
	}

	srv := &LcsServer{
		lcsCommons: lcsCommons{
			config:           config,
			chainDb:          can.ChainDb(),
			iConfig:          light.DefaultServerIndexerConfig,
			chtIndexer:       light.NewChtIndexer(can.ChainDb(), nil, params.CHTFrequencyServer, params.HelperTrieProcessConfirmations),
			bloomTrieIndexer: light.NewBloomTrieIndexer(can.ChainDb(), nil, params.BloomBitsBlocks, params.BloomTrieFrequency),
			protocolManager:  pm,
		},
		quitSync:  quitSync,
		lcsTopics: lcsTopics,
	}

	logger := log4j.New()

	chtV1SectionCount, _, _ := srv.chtIndexer.Sections()
	chtV2SectionCount := chtV1SectionCount / (params.CHTFrequencyClient / params.CHTFrequencyServer)
	if chtV2SectionCount != 0 {
		chtLastSection := chtV2SectionCount - 1
		chtLastSectionV1 := (chtLastSection+1)*(params.CHTFrequencyClient/params.CHTFrequencyServer) - 1
		chtSectionHead := srv.chtIndexer.SectionHead(chtLastSectionV1)
		chtRoot := light.GetChtRoot(pm.chainDb, chtLastSectionV1, chtSectionHead)
		logger.Info("Loaded CHT", "section", chtLastSection, "head", chtSectionHead, "root", chtRoot)
	}
	bloomTrieSectionCount, _, _ := srv.bloomTrieIndexer.Sections()
	if bloomTrieSectionCount != 0 {
		bloomTrieLastSection := bloomTrieSectionCount - 1
		bloomTrieSectionHead := srv.bloomTrieIndexer.SectionHead(bloomTrieLastSection)
		bloomTrieRoot := light.GetBloomTrieRoot(pm.chainDb, bloomTrieLastSection, bloomTrieSectionHead)
		logger.Info("Loaded bloom trie", "section", bloomTrieLastSection, "head", bloomTrieSectionHead, "root", bloomTrieRoot)
	}

	srv.chtIndexer.Start(can.BlockChain())
	pm.server = srv

	srv.defParams = &flowcontrol.ServerParams{
		BufLimit:    300000000,
		MinRecharge: 50000,
	}
	srv.fcManager = flowcontrol.NewClientManager(uint64(config.LightServ), 10, 1000000000)
	srv.fcCostStats = newCostStats(can.ChainDb())
	return srv, nil
}

func (s *LcsServer) Protocols() []p2p.Protocol {
	return s.makeProtocols(ServerProtocolVersions)
}

func (s *LcsServer) Start(srvr *p2p.Server) {
	s.protocolManager.Start(s.config.LightPeers)
	if srvr.DiscV5 != nil {
		for _, topic := range s.lcsTopics {
			topic := topic
			go func() {
				logger := log4j.New("topic", topic)
				logger.Info("Starting topic registration")
				defer logger.Info("Terminated topic registration")

				srvr.DiscV5.RegisterTopic(topic, s.quitSync)
			}()
		}
	}
	s.privateKey = srvr.PrivateKey
	s.protocolManager.blockLoop()
}

func (s *LcsServer) SetBloomBitsIndexer(bloomIndexer *kernel.ChainIndexer) {
	bloomIndexer.AddChildIndexer(s.bloomTrieIndexer)
}

func (s *LcsServer) Stop() {
	s.chtIndexer.Close()
	s.fcCostStats.store()
	s.fcManager.Stop()
	go func() {
		<-s.protocolManager.noMorePeers
	}()
	s.protocolManager.Stop()
}

type requestCosts struct {
	baseCost, reqCost uint64
}

type requestCostTable map[uint64]*requestCosts

type RequestCostList []struct {
	MsgCode, BaseCost, ReqCost uint64
}

func (list RequestCostList) decode() requestCostTable {
	table := make(requestCostTable)
	for _, e := range list {
		table[e.MsgCode] = &requestCosts{
			baseCost: e.BaseCost,
			reqCost:  e.ReqCost,
		}
	}
	return table
}

type linReg struct {
	sumX, sumY, sumXX, sumXY float64
	cnt                      uint64
}

const linRegMaxCnt = 100000

func (l *linReg) add(x, y float64) {
	if l.cnt >= linRegMaxCnt {
		sub := float64(l.cnt+1-linRegMaxCnt) / linRegMaxCnt
		l.sumX -= l.sumX * sub
		l.sumY -= l.sumY * sub
		l.sumXX -= l.sumXX * sub
		l.sumXY -= l.sumXY * sub
		l.cnt = linRegMaxCnt - 1
	}
	l.cnt++
	l.sumX += x
	l.sumY += y
	l.sumXX += x * x
	l.sumXY += x * y
}

func (l *linReg) calc() (b, m float64) {
	if l.cnt == 0 {
		return 0, 0
	}
	cnt := float64(l.cnt)
	d := cnt*l.sumXX - l.sumX*l.sumX
	if d < 0.001 {
		return l.sumY / cnt, 0
	}
	m = (cnt*l.sumXY - l.sumX*l.sumY) / d
	b = (l.sumY / cnt) - (m * l.sumX / cnt)
	return b, m
}

func (l *linReg) toBytes() []byte {
	var arr [40]byte
	binary.BigEndian.PutUint64(arr[0:8], math.Float64bits(l.sumX))
	binary.BigEndian.PutUint64(arr[8:16], math.Float64bits(l.sumY))
	binary.BigEndian.PutUint64(arr[16:24], math.Float64bits(l.sumXX))
	binary.BigEndian.PutUint64(arr[24:32], math.Float64bits(l.sumXY))
	binary.BigEndian.PutUint64(arr[32:40], l.cnt)
	return arr[:]
}

func linRegFromBytes(data []byte) *linReg {
	if len(data) != 40 {
		return nil
	}
	l := &linReg{}
	l.sumX = math.Float64frombits(binary.BigEndian.Uint64(data[0:8]))
	l.sumY = math.Float64frombits(binary.BigEndian.Uint64(data[8:16]))
	l.sumXX = math.Float64frombits(binary.BigEndian.Uint64(data[16:24]))
	l.sumXY = math.Float64frombits(binary.BigEndian.Uint64(data[24:32]))
	l.cnt = binary.BigEndian.Uint64(data[32:40])
	return l
}

type requestCostStats struct {
	lock  sync.RWMutex
	db    candb.Database
	stats map[uint64]*linReg
}

type requestCostStatsRlp []struct {
	MsgCode uint64
	Data    []byte
}

var rcStatsKey = []byte("_requestCostStats")

func newCostStats(db candb.Database) *requestCostStats {
	stats := make(map[uint64]*linReg)
	for _, code := range reqList {
		stats[code] = &linReg{cnt: 100}
	}

	if db != nil {
		data, err := db.Get(rcStatsKey)
		var statsRlp requestCostStatsRlp
		if err == nil {
			err = rlp.DecodeBytes(data, &statsRlp)
		}
		if err == nil {
			for _, r := range statsRlp {
				if stats[r.MsgCode] != nil {
					if l := linRegFromBytes(r.Data); l != nil {
						stats[r.MsgCode] = l
					}
				}
			}
		}
	}

	return &requestCostStats{
		db:    db,
		stats: stats,
	}
}

func (s *requestCostStats) store() {
	s.lock.Lock()
	defer s.lock.Unlock()

	statsRlp := make(requestCostStatsRlp, len(reqList))
	for i, code := range reqList {
		statsRlp[i].MsgCode = code
		statsRlp[i].Data = s.stats[code].toBytes()
	}

	if data, err := rlp.EncodeToBytes(statsRlp); err == nil {
		s.db.Put(rcStatsKey, data)
	}
}

func (s *requestCostStats) getCurrentList() RequestCostList {
	s.lock.Lock()
	defer s.lock.Unlock()

	list := make(RequestCostList, len(reqList))
	for idx, code := range reqList {
		b, m := s.stats[code].calc()
		if m < 0 {
			b += m
			m = 0
		}
		if b < 0 {
			b = 0
		}

		list[idx].MsgCode = code
		list[idx].BaseCost = uint64(b * 2)
		list[idx].ReqCost = uint64(m * 2)
	}
	return list
}

func (s *requestCostStats) update(msgCode, reqCnt, cost uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()

	c, ok := s.stats[msgCode]
	if !ok || reqCnt == 0 {
		return
	}
	c.add(float64(reqCnt), float64(cost))
}

func (pm *ProtocolManager) blockLoop() {
	pm.wg.Add(1)
	headCh := make(chan kernel.ChainHeadEvent, 10)
	headSub := pm.blockchain.SubscribeChainHeadEvent(headCh)
	go func() {
		var lastHead *types.Header
		lastBroadcastTd := common.Big0
		for {
			select {
			case ev := <-headCh:
				peers := pm.peers.AllPeers()
				if len(peers) > 0 {
					header := ev.Block.Header()
					hash := header.Hash()
					number := header.Number.Uint64()
					td := rawdb.ReadTd(pm.chainDb, hash, number)
					if td != nil && td.Cmp(lastBroadcastTd) > 0 {
						var reorg uint64
						if lastHead != nil {
							reorg = lastHead.Number.Uint64() - rawdb.FindCommonAncestor(pm.chainDb, header, lastHead).Number.Uint64()
						}
						lastHead = header
						lastBroadcastTd = td

						log4j.Debug("Announcing block to peers", "number", number, "hash", hash, "td", td, "reorg", reorg)

						announce := announceData{Hash: hash, Number: number, Td: td, ReorgDepth: reorg}
						var (
							signed         bool
							signedAnnounce announceData
						)

						for _, p := range peers {
							switch p.announceType {

							case announceTypeSimple:
								select {
								case p.announceChn <- announce:
								default:
									pm.removePeer(p.id)
								}

							case announceTypeSigned:
								if !signed {
									signedAnnounce = announce
									signedAnnounce.sign(pm.server.privateKey)
									signed = true
								}

								select {
								case p.announceChn <- signedAnnounce:
								default:
									pm.removePeer(p.id)
								}
							}
						}
					}
				}
			case <-pm.quitSync:
				headSub.Unsubscribe()
				pm.wg.Done()
				return
			}
		}
	}()
}
