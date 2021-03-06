package discovery

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
	pbq "github.com/youtube/vitess/go/vt/proto/query"
	pbt "github.com/youtube/vitess/go/vt/proto/topodata"
	"github.com/youtube/vitess/go/vt/tabletserver/tabletconn"
	"github.com/youtube/vitess/go/vt/topo"
	"golang.org/x/net/context"
)

// HealthCheckStatsListener is the listener to receive health check stats update.
type HealthCheckStatsListener interface {
	StatsUpdate(endPoint *pbt.EndPoint, cell string, target *pbq.Target, tabletExternallyReparentedTimestamp int64, stats *pbq.RealtimeStats)
}

// EndPointStats is returned when getting the set of endpoints.
type EndPointStats struct {
	EndPoint                            *pbt.EndPoint
	Cell                                string
	Target                              *pbq.Target
	TabletExternallyReparentedTimestamp int64
	Stats                               *pbq.RealtimeStats
}

// NewHealthCheck creates a new HealthCheck object.
func NewHealthCheck(listener HealthCheckStatsListener, connTimeout time.Duration, retryDelay time.Duration) *HealthCheck {
	return &HealthCheck{
		addrToConns: make(map[string]*healthCheckConn),
		targetToEPs: make(map[string]map[string]map[pbt.TabletType][]*pbt.EndPoint),
		listener:    listener,
		connTimeout: connTimeout,
		retryDelay:  retryDelay,
	}
}

// HealthCheck performs health checking and notifies downstream components about any changes.
type HealthCheck struct {
	// set at construction time
	listener    HealthCheckStatsListener
	connTimeout time.Duration
	retryDelay  time.Duration

	// mu protects all the following fields
	// when locking both mutex from HealthCheck and healthCheckConn, HealthCheck.mu goes first.
	mu          sync.RWMutex
	addrToConns map[string]*healthCheckConn                              // addrToConns maps from address to the healthCheckConn object.
	targetToEPs map[string]map[string]map[pbt.TabletType][]*pbt.EndPoint // targetToEPs maps from keyspace/shard/tablettype to a list of endpoints.
}

// healthCheckConn contains details about an endpoint.
type healthCheckConn struct {
	// set at construction time
	cell       string
	cancelFunc context.CancelFunc

	// mu protects all the following fields
	// when locking both mutex from HealthCheck and healthCheckConn, HealthCheck.mu goes first.
	mu                                  sync.RWMutex
	conn                                tabletconn.TabletConn
	target                              *pbq.Target
	tabletExternallyReparentedTimestamp int64
	stats                               *pbq.RealtimeStats
}

// checkConn performs health checking on the given endpoint.
func (hc *HealthCheck) checkConn(cell string, endPoint *pbt.EndPoint) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	hcc := &healthCheckConn{
		cell:       cell,
		cancelFunc: cancelFunc,
	}

	// retry health check if it fails
	for {
		stream, errfunc, err := hcc.connect(ctx, hc, endPoint)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Errorf("cannot connect to %+v: %v", endPoint, err)
			time.Sleep(hc.retryDelay)
			continue
		}
		for {
			err = hcc.processResponse(ctx, hc, endPoint, stream, errfunc)
			if err != nil {
				hcc.mu.Lock()
				hcc.conn.Close()
				hcc.conn = nil
				hcc.mu.Unlock()
				select {
				case <-ctx.Done():
					return
				default:
				}
				log.Errorf("error when streaming tablet health from %+v: %v", endPoint, err)
				time.Sleep(hc.retryDelay)
				break
			}
		}
	}
}

// connect creates connection to the endpoint and starts streaming.
func (hcc *healthCheckConn) connect(ctx context.Context, hc *HealthCheck, endPoint *pbt.EndPoint) (<-chan *pbq.StreamHealthResponse, tabletconn.ErrFunc, error) {
	conn, err := tabletconn.GetDialer()(ctx, endPoint, "" /*keyspace*/, "" /*shard*/, pbt.TabletType_RDONLY, hc.connTimeout)
	if err != nil {
		return nil, nil, err
	}
	stream, errfunc, err := conn.StreamHealth(ctx)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	hcc.mu.Lock()
	hcc.conn = conn
	hcc.mu.Unlock()
	return stream, errfunc, nil
}

// processResponse reads one health check response, and notifies HealthCheckStatsListener.
func (hcc *healthCheckConn) processResponse(ctx context.Context, hc *HealthCheck, endPoint *pbt.EndPoint, stream <-chan *pbq.StreamHealthResponse, errfunc tabletconn.ErrFunc) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case shr, ok := <-stream:
		if !ok {
			return errfunc()
		}
		if shr.Target == nil || shr.RealtimeStats == nil || shr.RealtimeStats.HealthError != "" {
			return fmt.Errorf("health stats is not valid: %v", shr)
		}

		if hcc.target == nil {
			// The first time we see response for the endpoint.
			hcc.mu.Lock()
			hcc.target = shr.Target
			hcc.tabletExternallyReparentedTimestamp = shr.TabletExternallyReparentedTimestamp
			hcc.stats = shr.RealtimeStats
			hcc.mu.Unlock()
			hc.mu.Lock()
			key := endPointToMapKey(endPoint)
			hc.addrToConns[key] = hcc
			hc.addEndPointToTargetProtected(hcc.target, endPoint)
			hc.mu.Unlock()
		} else if hcc.target.TabletType != shr.Target.TabletType {
			hc.mu.Lock()
			hc.deleteEndPointFromTargetProtected(hcc.target, endPoint)
			hcc.mu.Lock()
			hcc.target = shr.Target
			hcc.tabletExternallyReparentedTimestamp = shr.TabletExternallyReparentedTimestamp
			hcc.stats = shr.RealtimeStats
			hcc.mu.Unlock()
			hc.addEndPointToTargetProtected(shr.Target, endPoint)
			hc.mu.Unlock()
		} else {
			hcc.mu.Lock()
			hcc.target = shr.Target
			hcc.tabletExternallyReparentedTimestamp = shr.TabletExternallyReparentedTimestamp
			hcc.stats = shr.RealtimeStats
			hcc.mu.Unlock()
		}
		// notify downstream for tablettype and realtimestats change
		hc.listener.StatsUpdate(endPoint, hcc.cell, hcc.target, hcc.tabletExternallyReparentedTimestamp, hcc.stats)
		return nil
	}
}

// AddEndPoint adds the endpoint, and starts health check.
func (hc *HealthCheck) AddEndPoint(cell string, endPoint *pbt.EndPoint) {
	go hc.checkConn(cell, endPoint)
}

// RemoveEndPoint removes the endpoint, and stops the health check.
func (hc *HealthCheck) RemoveEndPoint(endPoint *pbt.EndPoint) {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	key := endPointToMapKey(endPoint)
	hcc, ok := hc.addrToConns[key]
	if !ok {
		return
	}
	hcc.cancelFunc()
	delete(hc.addrToConns, key)
	if hcc.target != nil {
		hc.deleteEndPointFromTargetProtected(hcc.target, endPoint)
	}
}

// GetEndPointStatsFromKeyspaceShard returns all EndPointStats for the given keyspace/shard.
func (hc *HealthCheck) GetEndPointStatsFromKeyspaceShard(keyspace, shard string) []*EndPointStats {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	shardMap, ok := hc.targetToEPs[keyspace]
	if !ok {
		return nil
	}
	ttMap, ok := shardMap[shard]
	if !ok {
		return nil
	}
	res := make([]*EndPointStats, 0, 1)
	for _, epList := range ttMap {
		for _, ep := range epList {
			key := endPointToMapKey(ep)
			hcc, ok := hc.addrToConns[key]
			if !ok {
				continue
			}
			hcc.mu.RLock()
			eps := &EndPointStats{
				EndPoint: ep,
				Cell:     hcc.cell,
				TabletExternallyReparentedTimestamp: hcc.tabletExternallyReparentedTimestamp,
				Target: hcc.target,
				Stats:  hcc.stats,
			}
			hcc.mu.RUnlock()
			res = append(res, eps)
		}
	}
	return res
}

// GetEndPointStatsFromTarget returns all EndPointStats for the given target.
func (hc *HealthCheck) GetEndPointStatsFromTarget(keyspace, shard string, tabletType pbt.TabletType) []*EndPointStats {
	hc.mu.RLock()
	defer hc.mu.RUnlock()

	shardMap, ok := hc.targetToEPs[keyspace]
	if !ok {
		return nil
	}
	ttMap, ok := shardMap[shard]
	if !ok {
		return nil
	}
	epList, ok := ttMap[tabletType]
	if !ok {
		return nil
	}
	res := make([]*EndPointStats, 0, 1)
	for _, ep := range epList {
		key := endPointToMapKey(ep)
		hcc, ok := hc.addrToConns[key]
		if !ok {
			continue
		}
		hcc.mu.RLock()
		eps := &EndPointStats{
			EndPoint: ep,
			Cell:     hcc.cell,
			TabletExternallyReparentedTimestamp: hcc.tabletExternallyReparentedTimestamp,
			Target: hcc.target,
			Stats:  hcc.stats,
		}
		hcc.mu.RUnlock()
		res = append(res, eps)
	}
	return res
}

// addEndPointToTargetProtected adds the endpoint to the given target.
// LOCK_REQUIRED hc.mu
func (hc *HealthCheck) addEndPointToTargetProtected(target *pbq.Target, endPoint *pbt.EndPoint) {
	shardMap, ok := hc.targetToEPs[target.Keyspace]
	if !ok {
		shardMap = make(map[string]map[pbt.TabletType][]*pbt.EndPoint)
		hc.targetToEPs[target.Keyspace] = shardMap
	}
	ttMap, ok := shardMap[target.Shard]
	if !ok {
		ttMap = make(map[pbt.TabletType][]*pbt.EndPoint)
		shardMap[target.Shard] = ttMap
	}
	epList, ok := ttMap[target.TabletType]
	if !ok {
		epList = make([]*pbt.EndPoint, 0, 1)
	}
	for _, ep := range epList {
		if topo.EndPointEquality(ep, endPoint) {
			log.Warningf("endpoint is already added: %+v", endPoint)
			return
		}
	}
	ttMap[target.TabletType] = append(epList, endPoint)
}

// deleteEndPointFromTargetProtected deletes the endpoint for the given target.
// LOCK_REQUIRED hc.mu
func (hc *HealthCheck) deleteEndPointFromTargetProtected(target *pbq.Target, endPoint *pbt.EndPoint) {
	shardMap, ok := hc.targetToEPs[target.Keyspace]
	if !ok {
		return
	}
	ttMap, ok := shardMap[target.Shard]
	if !ok {
		return
	}
	epList, ok := ttMap[target.TabletType]
	if !ok {
		return
	}
	for i, ep := range epList {
		if topo.EndPointEquality(ep, endPoint) {
			epList = append(epList[:i], epList[i+1:]...)
			ttMap[target.TabletType] = epList
			return
		}
	}
}

// endPointToMapKey creates a key to the map from endpoint's host and ports.
func endPointToMapKey(endPoint *pbt.EndPoint) string {
	parts := make([]string, 0, 1)
	for name, port := range endPoint.PortMap {
		parts = append(parts, name+":"+string(port))
	}
	sort.Strings(parts)
	parts = append([]string{endPoint.Host}, parts...)
	return strings.Join(parts, ":")
}
