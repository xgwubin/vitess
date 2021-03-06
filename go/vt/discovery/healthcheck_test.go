package discovery

import (
	"flag"
	"fmt"
	"reflect"
	"testing"
	"time"

	mproto "github.com/youtube/vitess/go/mysql/proto"
	pbq "github.com/youtube/vitess/go/vt/proto/query"
	pbt "github.com/youtube/vitess/go/vt/proto/topodata"
	tproto "github.com/youtube/vitess/go/vt/tabletserver/proto"
	"github.com/youtube/vitess/go/vt/tabletserver/tabletconn"
	"github.com/youtube/vitess/go/vt/topo"
	"golang.org/x/net/context"
)

var connMap map[string]*fakeConn

func init() {
	tabletconn.RegisterDialer("fake_discovery", discoveryDialer)
	flag.Set("tablet_protocol", "fake_discovery")
	connMap = make(map[string]*fakeConn)
}

func TestHealthCheck(t *testing.T) {
	ep := topo.NewEndPoint(0, "a")
	ep.PortMap["vt"] = 1
	input := make(chan *pbq.StreamHealthResponse)
	createFakeConn(ep, input)
	t.Logf(`createFakeConn({Host: "a", PortMap: {"vt": 1}}, c)`)
	l := newListener()
	hc := NewHealthCheck(l, 1*time.Millisecond, 1*time.Millisecond)
	hc.AddEndPoint("cell", ep)
	t.Logf(`hc = HealthCheck(); hc.AddEndPoint("cell", {Host: "a", PortMap: {"vt": 1}})`)

	// no endpoint before getting first StreamHealthResponse
	epsList := hc.GetEndPointStatsFromKeyspaceShard("k", "s")
	if len(epsList) != 0 {
		t.Errorf(`hc.GetEndPointStatsFromKeyspaceShard("k", "s") = %+v; want empty`, epsList)
	}

	// one endpoint after receiving a StreamHealthResponse
	shr := &pbq.StreamHealthResponse{
		Target: &pbq.Target{Keyspace: "k", Shard: "s", TabletType: pbt.TabletType_MASTER},
		TabletExternallyReparentedTimestamp: 10,
		RealtimeStats:                       &pbq.RealtimeStats{SecondsBehindMaster: 1, CpuUsage: 0.2},
	}
	want := &EndPointStats{
		EndPoint: ep,
		Cell:     "cell",
		Target:   &pbq.Target{Keyspace: "k", Shard: "s", TabletType: pbt.TabletType_MASTER},
		Stats:    &pbq.RealtimeStats{SecondsBehindMaster: 1, CpuUsage: 0.2},
		TabletExternallyReparentedTimestamp: 10,
	}
	input <- shr
	t.Logf(`input <- {{Keyspace: "k", Shard: "s", TabletType: MASTER}, TabletExternallyReparentedTimestamp: 10, {SecondsBehindMaster: 1, CpuUsage: 0.2}}`)
	res := <-l.output
	if !reflect.DeepEqual(res, want) {
		t.Errorf(`<-l.output: %+v; want %+v`, res, want)
	}
	epsList = hc.GetEndPointStatsFromKeyspaceShard("k", "s")
	if len(epsList) != 1 || !reflect.DeepEqual(epsList[0], want) {
		t.Errorf(`hc.GetEndPointStatsFromKeyspaceShard("k", "s") = %+v; want %+v`, epsList, want)
	}

	// TabletType changed
	shr = &pbq.StreamHealthResponse{
		Target: &pbq.Target{Keyspace: "k", Shard: "s", TabletType: pbt.TabletType_REPLICA},
		TabletExternallyReparentedTimestamp: 0,
		RealtimeStats:                       &pbq.RealtimeStats{SecondsBehindMaster: 1, CpuUsage: 0.5},
	}
	want = &EndPointStats{
		EndPoint: ep,
		Cell:     "cell",
		Target:   &pbq.Target{Keyspace: "k", Shard: "s", TabletType: pbt.TabletType_REPLICA},
		Stats:    &pbq.RealtimeStats{SecondsBehindMaster: 1, CpuUsage: 0.5},
		TabletExternallyReparentedTimestamp: 0,
	}
	input <- shr
	t.Logf(`input <- {{Keyspace: "k", Shard: "s", TabletType: REPLICA}, TabletExternallyReparentedTimestamp: 0, {SecondsBehindMaster: 1, CpuUsage: 0.5}}`)
	res = <-l.output
	if !reflect.DeepEqual(res, want) {
		t.Errorf(`<-l.output: %+v; want %+v`, res, want)
	}
	epsList = hc.GetEndPointStatsFromTarget("k", "s", pbt.TabletType_REPLICA)
	if len(epsList) != 1 || !reflect.DeepEqual(epsList[0], want) {
		t.Errorf(`hc.GetEndPointStatsFromTarget("k", "s", REPLICA) = %+v; want %+v`, epsList, want)
	}

	// RealtimeStats changed
	shr = &pbq.StreamHealthResponse{
		Target: &pbq.Target{Keyspace: "k", Shard: "s", TabletType: pbt.TabletType_REPLICA},
		TabletExternallyReparentedTimestamp: 0,
		RealtimeStats:                       &pbq.RealtimeStats{SecondsBehindMaster: 1, CpuUsage: 0.3},
	}
	want = &EndPointStats{
		EndPoint: ep,
		Cell:     "cell",
		Target:   &pbq.Target{Keyspace: "k", Shard: "s", TabletType: pbt.TabletType_REPLICA},
		Stats:    &pbq.RealtimeStats{SecondsBehindMaster: 1, CpuUsage: 0.3},
		TabletExternallyReparentedTimestamp: 0,
	}
	input <- shr
	t.Logf(`input <- {{Keyspace: "k", Shard: "s", TabletType: REPLICA}, TabletExternallyReparentedTimestamp: 0, {SecondsBehindMaster: 1, CpuUsage: 0.3}}`)
	res = <-l.output
	if !reflect.DeepEqual(res, want) {
		t.Errorf(`<-l.output: %+v; want %+v`, res, want)
	}

	// remove endpoint
	hc.RemoveEndPoint(ep)
	t.Logf(`hc.RemoveEndPoint({Host: "a", PortMap: {"vt": 1}})`)
	epsList = hc.GetEndPointStatsFromKeyspaceShard("k", "s")
	if len(epsList) != 0 {
		t.Errorf(`hc.GetEndPointStatsFromKeyspaceShard("k", "s") = %+v; want empty`, epsList)
	}
}

type listener struct {
	output chan *EndPointStats
}

func newListener() *listener {
	return &listener{output: make(chan *EndPointStats, 1)}
}

func (l *listener) StatsUpdate(endPoint *pbt.EndPoint, cell string, target *pbq.Target, reparentTimestamp int64, stats *pbq.RealtimeStats) {
	l.output <- &EndPointStats{
		EndPoint: endPoint,
		Cell:     cell,
		Target:   target,
		TabletExternallyReparentedTimestamp: reparentTimestamp,
		Stats: stats,
	}
}

func createFakeConn(endPoint *pbt.EndPoint, c chan *pbq.StreamHealthResponse) *fakeConn {
	key := endPointToMapKey(endPoint)
	conn := &fakeConn{endPoint: endPoint, hcChan: c}
	connMap[key] = conn
	return conn
}

func discoveryDialer(ctx context.Context, endPoint *pbt.EndPoint, keyspace, shard string, tabletType pbt.TabletType, timeout time.Duration) (tabletconn.TabletConn, error) {
	key := endPointToMapKey(endPoint)
	return connMap[key], nil
}

type fakeConn struct {
	endPoint *pbt.EndPoint
	hcChan   chan *pbq.StreamHealthResponse
}

func (fc *fakeConn) StreamHealth(ctx context.Context) (<-chan *pbq.StreamHealthResponse, tabletconn.ErrFunc, error) {
	return fc.hcChan, func() error { return nil }, nil
}

func (fc *fakeConn) Execute(ctx context.Context, query string, bindVars map[string]interface{}, transactionID int64) (*mproto.QueryResult, error) {
	return nil, fmt.Errorf("not implemented")
}

func (fc *fakeConn) Execute2(ctx context.Context, query string, bindVars map[string]interface{}, transactionID int64) (*mproto.QueryResult, error) {
	return fc.Execute(ctx, query, bindVars, transactionID)
}

func (fc *fakeConn) ExecuteBatch(ctx context.Context, queries []tproto.BoundQuery, asTransaction bool, transactionID int64) (*tproto.QueryResultList, error) {
	return nil, fmt.Errorf("not implemented")
}

func (fc *fakeConn) ExecuteBatch2(ctx context.Context, queries []tproto.BoundQuery, asTransaction bool, transactionID int64) (*tproto.QueryResultList, error) {
	return fc.ExecuteBatch(ctx, queries, asTransaction, transactionID)
}

func (fc *fakeConn) StreamExecute(ctx context.Context, query string, bindVars map[string]interface{}, transactionID int64) (<-chan *mproto.QueryResult, tabletconn.ErrFunc, error) {
	return nil, nil, fmt.Errorf("not implemented")
}

func (fc *fakeConn) StreamExecute2(ctx context.Context, query string, bindVars map[string]interface{}, transactionID int64) (<-chan *mproto.QueryResult, tabletconn.ErrFunc, error) {
	return fc.StreamExecute(ctx, query, bindVars, transactionID)
}

func (fc *fakeConn) Begin(ctx context.Context) (int64, error) {
	return 0, fmt.Errorf("not implemented")
}

func (fc *fakeConn) Begin2(ctx context.Context) (int64, error) {
	return fc.Begin(ctx)
}

func (fc *fakeConn) Commit(ctx context.Context, transactionID int64) error {
	return fmt.Errorf("not implemented")
}

func (fc *fakeConn) Commit2(ctx context.Context, transactionID int64) error {
	return fc.Commit(ctx, transactionID)
}

func (fc *fakeConn) Rollback(ctx context.Context, transactionID int64) error {
	return fmt.Errorf("not implemented")
}

func (fc *fakeConn) Rollback2(ctx context.Context, transactionID int64) error {
	return fc.Rollback(ctx, transactionID)
}

func (fc *fakeConn) SplitQuery(ctx context.Context, query tproto.BoundQuery, splitColumn string, splitCount int) ([]tproto.QuerySplit, error) {
	return nil, fmt.Errorf("not implemented")
}

func (fc *fakeConn) SetTarget(keyspace, shard string, tabletType pbt.TabletType) error {
	return fmt.Errorf("not implemented")
}

func (fc *fakeConn) EndPoint() *pbt.EndPoint {
	return fc.endPoint
}

func (fc *fakeConn) Close() {
}
