package networkdb

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	golog "log"
	rnd "math/rand"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/containerd/log"
	"github.com/hashicorp/memberlist"
)

const (
	reapPeriod       = 5 * time.Second
	retryInterval    = 1 * time.Second
	nodeReapInterval = 24 * time.Hour
	nodeReapPeriod   = 2 * time.Hour
	// considering a cluster with > 20 nodes and a drain speed of 100 msg/s
	// the following is roughly 1 minute
	maxQueueLenBroadcastOnSync = 500
)

type logWriter struct{}

func (l *logWriter) Write(p []byte) (int, error) {
	str := string(p)
	str = strings.TrimSuffix(str, "\n")

	switch {
	case strings.HasPrefix(str, "[WARN] "):
		str = strings.TrimPrefix(str, "[WARN] ")
		log.G(context.TODO()).Warn(str)
	case strings.HasPrefix(str, "[DEBUG] "):
		str = strings.TrimPrefix(str, "[DEBUG] ")
		log.G(context.TODO()).Debug(str)
	case strings.HasPrefix(str, "[INFO] "):
		str = strings.TrimPrefix(str, "[INFO] ")
		log.G(context.TODO()).Info(str)
	case strings.HasPrefix(str, "[ERR] "):
		str = strings.TrimPrefix(str, "[ERR] ")
		log.G(context.TODO()).Warn(str)
	}

	return len(p), nil
}

// SetKey adds a new key to the key ring
func (nDB *NetworkDB) SetKey(key []byte) {
	log.G(context.TODO()).Debugf("Adding key %.5s", hex.EncodeToString(key))
	nDB.Lock()
	defer nDB.Unlock()
	for _, dbKey := range nDB.config.Keys {
		if bytes.Equal(key, dbKey) {
			return
		}
	}
	nDB.config.Keys = append(nDB.config.Keys, key)
	if nDB.keyring != nil {
		nDB.keyring.AddKey(key)
	}
	logEncKeys(context.TODO(), key)
}

// SetPrimaryKey sets the given key as the primary key. This should have
// been added apriori through SetKey
func (nDB *NetworkDB) SetPrimaryKey(key []byte) {
	log.G(context.TODO()).Debugf("Primary Key %.5s", hex.EncodeToString(key))
	nDB.Lock()
	defer nDB.Unlock()
	for _, dbKey := range nDB.config.Keys {
		if bytes.Equal(key, dbKey) {
			if nDB.keyring != nil {
				nDB.keyring.UseKey(dbKey)
			}
			break
		}
	}
}

// RemoveKey removes a key from the key ring. The key being removed
// can't be the primary key
func (nDB *NetworkDB) RemoveKey(key []byte) {
	log.G(context.TODO()).Debugf("Remove Key %.5s", hex.EncodeToString(key))
	nDB.Lock()
	defer nDB.Unlock()
	for i, dbKey := range nDB.config.Keys {
		if bytes.Equal(key, dbKey) {
			nDB.config.Keys = append(nDB.config.Keys[:i], nDB.config.Keys[i+1:]...)
			if nDB.keyring != nil {
				nDB.keyring.RemoveKey(dbKey)
			}
			break
		}
	}
}

func (nDB *NetworkDB) clusterInit() error {
	nDB.lastStatsTimestamp = time.Now()
	nDB.lastHealthTimestamp = nDB.lastStatsTimestamp

	config := memberlist.DefaultLANConfig()
	config.Name = nDB.config.NodeID
	config.BindAddr = nDB.config.BindAddr
	config.AdvertiseAddr = nDB.config.AdvertiseAddr
	config.UDPBufferSize = nDB.config.PacketBufferSize

	if nDB.config.BindPort != 0 {
		config.BindPort = nDB.config.BindPort
		config.AdvertisePort = nDB.config.BindPort
	}

	config.ProtocolVersion = memberlist.ProtocolVersion2Compatible
	config.Delegate = &delegate{nDB: nDB}
	config.Events = &eventDelegate{nDB: nDB}
	// custom logger that does not add time or date, so they are not
	// duplicated by logrus
	config.Logger = golog.New(&logWriter{}, "", 0)

	var err error
	if len(nDB.config.Keys) > 0 {
		for i, key := range nDB.config.Keys {
			log.G(context.TODO()).Debugf("Encryption key %d: %.5s", i+1, hex.EncodeToString(key))
		}
		logEncKeys(context.TODO(), nDB.config.Keys...)
		nDB.keyring, err = memberlist.NewKeyring(nDB.config.Keys, nDB.config.Keys[0])
		if err != nil {
			return err
		}
		config.Keyring = nDB.keyring
	}

	nDB.networkBroadcasts = &memberlist.TransmitLimitedQueue{
		NumNodes:       nDB.estNumNodes,
		RetransmitMult: config.RetransmitMult,
	}

	nDB.nodeBroadcasts = &memberlist.TransmitLimitedQueue{
		NumNodes:       nDB.estNumNodes,
		RetransmitMult: config.RetransmitMult,
	}

	mlist, err := memberlist.Create(config)
	if err != nil {
		return fmt.Errorf("failed to create memberlist: %v", err)
	}

	nDB.ctx, nDB.cancelCtx = context.WithCancel(context.Background())
	nDB.memberlist = mlist

	for _, trigger := range []struct {
		interval time.Duration
		fn       func()
	}{
		{reapPeriod, nDB.reapState},
		{config.GossipInterval, nDB.gossip},
		{config.PushPullInterval, nDB.bulkSyncTables},
		{retryInterval, nDB.reconnectNode},
		{nodeReapPeriod, nDB.reapDeadNode},
		{nDB.config.rejoinClusterInterval, nDB.rejoinClusterBootStrap},
	} {
		t := time.NewTicker(trigger.interval)
		go nDB.triggerFunc(trigger.interval, t.C, trigger.fn)
		nDB.tickers = append(nDB.tickers, t)
	}

	return nil
}

func (nDB *NetworkDB) retryJoin(ctx context.Context, members []string) {
	t := time.NewTicker(retryInterval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			if _, err := nDB.memberlist.Join(members); err != nil {
				log.G(ctx).Errorf("Failed to join memberlist %s on retry: %v", members, err)
				continue
			}
			if err := nDB.sendNodeEvent(NodeEventTypeJoin); err != nil {
				log.G(ctx).Errorf("failed to send node join on retry: %v", err)
				continue
			}
			return
		case <-ctx.Done():
			return
		}
	}
}

func (nDB *NetworkDB) clusterJoin(members []string) error {
	mlist := nDB.memberlist

	if _, err := mlist.Join(members); err != nil {
		// In case of failure, we no longer need to explicitly call retryJoin.
		// rejoinClusterBootStrap, which runs every nDB.config.rejoinClusterInterval,
		// will retryJoin for nDB.config.rejoinClusterDuration.
		return fmt.Errorf("could not join node to memberlist: %v", err)
	}

	if err := nDB.sendNodeEvent(NodeEventTypeJoin); err != nil {
		return fmt.Errorf("failed to send node join: %v", err)
	}

	return nil
}

func (nDB *NetworkDB) clusterLeave() error {
	mlist := nDB.memberlist

	if err := nDB.sendNodeEvent(NodeEventTypeLeave); err != nil {
		log.G(context.TODO()).WithError(err).Error("failed to send node leave event")
	}

	if err := mlist.Leave(time.Second); err != nil {
		log.G(context.TODO()).WithError(err).Error("failed to broadcast memberlist leave message")
	}

	// cancel the context
	nDB.cancelCtx()

	for _, t := range nDB.tickers {
		t.Stop()
	}

	return mlist.Shutdown()
}

func (nDB *NetworkDB) triggerFunc(stagger time.Duration, C <-chan time.Time, f func()) {
	// Use a random stagger to avoid synchronizing
	randStagger := time.Duration(uint64(rnd.Int63()) % uint64(stagger)) //nolint:gosec // gosec complains about the use of rand here. It should be fine.
	select {
	case <-time.After(randStagger):
	case <-nDB.ctx.Done():
		return
	}
	for {
		select {
		case <-C:
			f()
		case <-nDB.ctx.Done():
			return
		}
	}
}

func (nDB *NetworkDB) reapDeadNode() {
	nDB.Lock()
	defer nDB.Unlock()
	for _, nodeMap := range []map[string]*node{
		nDB.failedNodes,
		nDB.leftNodes,
	} {
		for id, n := range nodeMap {
			if n.reapTime > nodeReapPeriod {
				n.reapTime -= nodeReapPeriod
				continue
			}
			log.G(context.TODO()).Debugf("Garbage collect node %v", n.Name)
			delete(nodeMap, id)
		}
	}
}

// rejoinClusterBootStrap is called periodically to check if all bootStrap nodes are active in the cluster,
// if not, call the cluster join to merge 2 separate clusters that are formed when all managers
// stopped/started at the same time
func (nDB *NetworkDB) rejoinClusterBootStrap() {
	nDB.RLock()
	if len(nDB.bootStrapIP) == 0 {
		nDB.RUnlock()
		return
	}

	myself, ok := nDB.nodes[nDB.config.NodeID]
	if !ok {
		nDB.RUnlock()
		log.G(context.TODO()).Warnf("rejoinClusterBootstrap unable to find local node info using ID:%v", nDB.config.NodeID)
		return
	}
	bootStrapIPs := make([]string, 0, len(nDB.bootStrapIP))
	for _, bootIP := range nDB.bootStrapIP {
		// bootstrap IPs are usually IP:port from the Join
		bootstrapIP, err := netip.ParseAddrPort(bootIP)
		if err != nil {
			// try to parse it as an IP without port
			// Note this seems to be the case for swarm that do not specify any port
			addr, err := netip.ParseAddr(bootIP)
			if err == nil {
				bootstrapIP = netip.AddrPortFrom(addr, uint16(nDB.config.BindPort))
			}
		}
		if bootstrapIP.IsValid() {
			for _, node := range nDB.nodes {
				if node == myself {
					continue
				}
				nodeIP, _ := netip.AddrFromSlice(node.Addr)
				if bootstrapIP == netip.AddrPortFrom(nodeIP, node.Port) {
					// One of the bootstrap nodes (and not myself) is part of the cluster, return
					nDB.RUnlock()
					return
				}
			}
			bootStrapIPs = append(bootStrapIPs, bootIP)
		}
	}
	nDB.RUnlock()
	if len(bootStrapIPs) == 0 {
		// this will also avoid to call the Join with an empty list erasing the current bootstrap ip list
		log.G(context.TODO()).Debug("rejoinClusterBootStrap did not find any valid IP")
		return
	}
	// None of the bootStrap nodes are in the cluster, call memberlist join
	log.G(context.TODO()).Debugf("rejoinClusterBootStrap, calling cluster join with bootStrap %v", bootStrapIPs)
	ctx, cancel := context.WithTimeout(nDB.ctx, nDB.config.rejoinClusterDuration)
	defer cancel()
	nDB.retryJoin(ctx, bootStrapIPs)
}

func (nDB *NetworkDB) reconnectNode() {
	nDB.RLock()
	if len(nDB.failedNodes) == 0 {
		nDB.RUnlock()
		return
	}

	nodes := make([]*node, 0, len(nDB.failedNodes))
	for _, n := range nDB.failedNodes {
		nodes = append(nodes, n)
	}
	nDB.RUnlock()

	nDB.rngMu.Lock()
	offset := nDB.rng.IntN(len(nodes))
	nDB.rngMu.Unlock()
	node := nodes[offset]
	addr := net.UDPAddr{IP: node.Addr, Port: int(node.Port)}

	if _, err := nDB.memberlist.Join([]string{addr.String()}); err != nil {
		return
	}

	if err := nDB.sendNodeEvent(NodeEventTypeJoin); err != nil {
		return
	}

	log.G(context.TODO()).Debugf("Initiating bulk sync with node %s after reconnect", node.Name)
	nDB.bulkSync([]string{node.Name}, true)
}

// For timing the entry deletion in the reaper APIs that doesn't use monotonic clock
// source (time.Now, Sub etc.) should be avoided. Hence we use reapTime in every
// entry which is set initially to reapInterval and decremented by reapPeriod every time
// the reaper runs. NOTE nDB.reapTableEntries updates the reapTime with a readlock. This
// is safe as long as no other concurrent path touches the reapTime field.
func (nDB *NetworkDB) reapState() {
	// The reapTableEntries leverage the presence of the network so garbage collect entries first
	nDB.reapTableEntries()
	nDB.reapNetworks()
}

func (nDB *NetworkDB) reapNetworks() {
	nDB.Lock()
	for id, n := range nDB.thisNodeNetworks {
		if n.leaving {
			if n.reapTime <= 0 {
				delete(nDB.thisNodeNetworks, id)
				continue
			}
			n.reapTime -= reapPeriod
		}
	}
	for _, nn := range nDB.networks {
		for id, n := range nn {
			if n.leaving {
				if n.reapTime <= 0 {
					delete(nn, id)
					continue
				}
				n.reapTime -= reapPeriod
			}
		}
	}
	nDB.Unlock()
}

func (nDB *NetworkDB) reapTableEntries() {
	var nodeNetworks []string
	// This is best effort, if the list of network changes will be picked up in the next cycle
	nDB.RLock()
	for nid := range nDB.thisNodeNetworks {
		nodeNetworks = append(nodeNetworks, nid)
	}
	nDB.RUnlock()

	cycleStart := time.Now()
	// In order to avoid blocking the database for a long time, apply the garbage collection logic by network
	// The lock is taken at the beginning of the cycle and the deletion is inline
	for _, nid := range nodeNetworks {
		nDB.Lock()
		nDB.indexes[byNetwork].Root().WalkPrefix([]byte("/"+nid), func(path []byte, v *entry) bool {
			// timeCompensation compensate in case the lock took some time to be released
			timeCompensation := time.Since(cycleStart)
			if !v.deleting {
				return false
			}

			// In this check we are adding an extra 1 second to guarantee that when the number is truncated to int32 to fit the packet
			// for the tableEvent the number is always strictly > 1 and never 0
			if v.reapTime > reapPeriod+timeCompensation+time.Second {
				v.reapTime -= reapPeriod + timeCompensation
				return false
			}

			params := strings.Split(string(path[1:]), "/")
			nwID, tName, key := params[0], params[1], params[2]
			okTable, okNetwork := nDB.deleteEntry(nwID, tName, key)
			if !okTable {
				log.G(context.TODO()).Errorf("Table tree delete failed, entry with key:%s does not exist in the table:%s network:%s", key, tName, nwID)
			}
			if !okNetwork {
				log.G(context.TODO()).Errorf("Network tree delete failed, entry with key:%s does not exist in the network:%s table:%s", key, nwID, tName)
			}

			return false
		})
		nDB.Unlock()
	}
}

func (nDB *NetworkDB) gossip() {
	networkNodes := make(map[string][]string)
	nDB.RLock()
	for nid := range nDB.thisNodeNetworks {
		networkNodes[nid] = nDB.networkNodes[nid]
	}
	printStats := time.Since(nDB.lastStatsTimestamp) >= nDB.config.StatsPrintPeriod
	printHealth := time.Since(nDB.lastHealthTimestamp) >= nDB.config.HealthPrintPeriod
	nDB.RUnlock()

	if printHealth {
		healthScore := nDB.memberlist.GetHealthScore()
		if healthScore != 0 {
			log.G(context.TODO()).Warnf("NetworkDB stats %v(%v) - healthscore:%d (connectivity issues)", nDB.config.Hostname, nDB.config.NodeID, healthScore)
		}
		nDB.lastHealthTimestamp = time.Now()
	}

	for nid, nodes := range networkNodes {
		mNodes := nDB.mRandomNodes(3, nodes)
		bytesAvail := nDB.config.PacketBufferSize - compoundHeaderOverhead

		nDB.RLock()
		network, ok := nDB.thisNodeNetworks[nid]
		nDB.RUnlock()
		if !ok || network == nil {
			// It is normal for the network to be removed
			// between the time we collect the network
			// attachments of this node and processing
			// them here.
			continue
		}

		msgs := getBroadcasts(compoundOverhead, bytesAvail, network.tableBroadcasts, network.tableRebroadcasts)
		// Collect stats and print the queue info, note this code is here also to have a view of the queues empty
		network.qMessagesSent.Add(int64(len(msgs)))
		if printStats {
			msent := network.qMessagesSent.Swap(0)
			log.G(context.TODO()).Infof("NetworkDB stats %v(%v) - netID:%s leaving:%t netPeers:%d entries:%d Queue qLen:%d+%d netMsg/s:%d",
				nDB.config.Hostname, nDB.config.NodeID,
				nid, network.leaving, network.tableBroadcasts.NumNodes(), network.entriesNumber.Load(),
				network.tableBroadcasts.NumQueued(), network.tableRebroadcasts.NumQueued(),
				msent/int64((nDB.config.StatsPrintPeriod/time.Second)))
		}

		if len(msgs) == 0 {
			continue
		}

		// Create a compound message
		compound := makeCompoundMessage(msgs)

		for _, node := range mNodes {
			nDB.RLock()
			mnode := nDB.nodes[node]
			nDB.RUnlock()

			if mnode == nil {
				break
			}

			// Send the compound message
			if err := nDB.memberlist.SendBestEffort(&mnode.Node, compound); err != nil {
				log.G(context.TODO()).Errorf("Failed to send gossip to %s: %s", mnode.Addr, err)
			}
		}
	}
	// Reset the stats
	if printStats {
		nDB.lastStatsTimestamp = time.Now()
	}
}

func (nDB *NetworkDB) bulkSyncTables() {
	var networks []string
	nDB.RLock()
	for nid, network := range nDB.thisNodeNetworks {
		if network.leaving {
			continue
		}
		networks = append(networks, nid)
	}
	nDB.RUnlock()

	for len(networks) != 0 {
		nid := networks[0]
		networks = networks[1:]

		nDB.RLock()
		nodes := nDB.networkNodes[nid]
		nDB.RUnlock()

		// No peer nodes on this network. Move on.
		if len(nodes) == 0 {
			continue
		}

		completed, err := nDB.bulkSync(nodes, false)
		if err != nil {
			log.G(context.TODO()).Errorf("periodic bulk sync failure for network %s: %v", nid, err)
			continue
		}

		// Remove all the networks for which we have
		// successfully completed bulk sync in this iteration.
		updatedNetworks := make([]string, 0, len(networks))
		for _, nid := range networks {
			var found bool
			for _, completedNid := range completed {
				if nid == completedNid {
					found = true
					break
				}
			}

			if !found {
				updatedNetworks = append(updatedNetworks, nid)
			}
		}

		networks = updatedNetworks
	}
}

func (nDB *NetworkDB) bulkSync(nodes []string, all bool) ([]string, error) {
	if !all {
		// Get 2 random nodes. 2nd node will be tried if the bulk sync to
		// 1st node fails.
		nodes = nDB.mRandomNodes(2, nodes)
	}

	if len(nodes) == 0 {
		return nil, nil
	}

	var err error
	var networks []string
	var success bool
	for _, node := range nodes {
		if node == nDB.config.NodeID {
			continue
		}
		log.G(context.TODO()).Debugf("%v(%v): Initiating bulk sync with node %v", nDB.config.Hostname, nDB.config.NodeID, node)
		networks = nDB.findCommonNetworks(node)
		err = nDB.bulkSyncNode(networks, node, true)
		if err != nil {
			err = fmt.Errorf("bulk sync to node %s failed: %v", node, err)
			log.G(context.TODO()).Warn(err.Error())
		} else {
			// bulk sync succeeded
			success = true
			// if its periodic bulksync stop after the first successful sync
			if !all {
				break
			}
		}
	}

	if success {
		// if at least one node sync succeeded
		return networks, nil
	}

	return nil, err
}

// Bulk sync all the table entries belonging to a set of networks to a
// single peer node. It can be unsolicited or can be in response to an
// unsolicited bulk sync
func (nDB *NetworkDB) bulkSyncNode(networks []string, node string, unsolicited bool) error {
	var msgs [][]byte

	var unsolMsg string
	if unsolicited {
		unsolMsg = "unsolicited"
	}

	log.G(context.TODO()).Debugf("%v(%v): Initiating %s bulk sync for networks %v with node %s",
		nDB.config.Hostname, nDB.config.NodeID, unsolMsg, networks, node)

	nDB.RLock()
	mnode := nDB.nodes[node]
	if mnode == nil {
		nDB.RUnlock()
		return nil
	}

	for _, nid := range networks {
		nDB.indexes[byNetwork].Root().WalkPrefix([]byte("/"+nid), func(path []byte, v *entry) bool {
			eType := TableEventTypeCreate
			if v.deleting {
				eType = TableEventTypeDelete
			}

			params := strings.Split(string(path[1:]), "/")
			tEvent := TableEvent{
				Type:      eType,
				LTime:     v.ltime,
				NodeName:  v.node,
				NetworkID: nid,
				TableName: params[1],
				Key:       params[2],
				Value:     v.value,
				// The duration in second is a float that below would be truncated
				ResidualReapTime: int32(v.reapTime.Seconds()),
			}

			msg, err := encodeMessage(MessageTypeTableEvent, &tEvent)
			if err != nil {
				log.G(context.TODO()).Errorf("Encode failure during bulk sync: %#v", tEvent)
				return false
			}

			msgs = append(msgs, msg)
			return false
		})
	}
	nDB.RUnlock()

	// Create a compound message
	compound := makeCompoundMessage(msgs)

	bsm := BulkSyncMessage{
		LTime:       nDB.tableClock.Time(),
		Unsolicited: unsolicited,
		NodeName:    nDB.config.NodeID,
		Networks:    networks,
		Payload:     compound,
	}

	buf, err := encodeMessage(MessageTypeBulkSync, &bsm)
	if err != nil {
		return fmt.Errorf("failed to encode bulk sync message: %v", err)
	}

	nDB.Lock()
	ch := make(chan struct{})
	nDB.bulkSyncAckTbl[node] = ch
	nDB.Unlock()

	err = nDB.memberlist.SendReliable(&mnode.Node, buf)
	if err != nil {
		nDB.Lock()
		delete(nDB.bulkSyncAckTbl, node)
		nDB.Unlock()

		return fmt.Errorf("failed to send a TCP message during bulk sync: %v", err)
	}

	// Wait on a response only if it is unsolicited.
	if unsolicited {
		startTime := time.Now()
		t := time.NewTimer(30 * time.Second)
		select {
		case <-t.C:
			log.G(context.TODO()).Errorf("Bulk sync to node %s timed out", node)
		case <-ch:
			log.G(context.TODO()).Debugf("%v(%v): Bulk sync to node %s took %s", nDB.config.Hostname, nDB.config.NodeID, node, time.Since(startTime))
		}
		t.Stop()
	}

	return nil
}

// mRandomNodes is used to select up to m random nodes. It is possible
// that less than m nodes are returned.
func (nDB *NetworkDB) mRandomNodes(m int, nodes []string) []string {
	mNodes := make([]string, 0, max(0, len(nodes)-1))
	for _, node := range nodes {
		if node == nDB.config.NodeID {
			// Skip myself
			continue
		}
		mNodes = append(mNodes, node)
	}

	if len(mNodes) < m {
		nDB.rngMu.Lock()
		nDB.rng.Shuffle(len(mNodes), func(i, j int) {
			mNodes[i], mNodes[j] = mNodes[j], mNodes[i]
		})
		nDB.rngMu.Unlock()
		return mNodes
	}

	nDB.rngMu.Lock()
	perm := nDB.rng.Perm(len(mNodes))
	nDB.rngMu.Unlock()

	sample := make([]string, 0, m)
	for _, idx := range perm[:m] {
		sample = append(sample, mNodes[idx])
	}

	return sample
}
