/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package http2

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/alipay/sofa-mosn/pkg/protocol"
	str "github.com/alipay/sofa-mosn/pkg/stream"
	"github.com/alipay/sofa-mosn/pkg/types"
	"golang.org/x/net/http2"
)

const (
	// H2 conn key in context
	H2ConnKey = "h2_conn"
)

var (
	connPoolOnce     sync.Once
	connPoolInstance *connPool
	transport        *http2.Transport
)

// types.ConnectionPool
type connPool struct {
	activeClients map[string][]*activeClient // key is host:port
	mux           sync.Mutex
	host          types.Host
}

func NewConnPool(host types.Host) types.ConnectionPool {
	connPoolOnce.Do(func() {
		if connPoolInstance == nil {
			connPoolInstance = &connPool{
				host:          host,
				activeClients: make(map[string][]*activeClient),
			}
		}
	})

	return connPoolInstance
}

func (p *connPool) Protocol() types.Protocol {
	return protocol.HTTP2
}

func (p *connPool) Host() types.Host {
	return p.host
}

func (p *connPool) InitActiveClient(context context.Context) error {
	return nil
}

//由 PROXY 调用
func (p *connPool) NewStream(context context.Context, streamID string, responseDecoder types.StreamReceiver,
	cb types.PoolEventListener) types.Cancellable {

	ac := p.getOrInitActiveClient(context, p.host.AddressString())

	if ac == nil {
		cb.OnFailure(streamID, types.ConnectionFailure, nil)
		return nil
	}

	if !p.host.ClusterInfo().ResourceManager().Requests().CanCreate() {
		cb.OnFailure(streamID, types.Overflow, nil)
		p.host.HostStats().UpstreamRequestPendingOverflow.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestPendingOverflow.Inc(1)
	} else {
		ac.totalStream++
		p.host.HostStats().UpstreamRequestTotal.Inc(1)
		p.host.HostStats().UpstreamRequestActive.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestTotal.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestActive.Inc(1)
		p.host.ClusterInfo().ResourceManager().Requests().Increase()
		streamEncoder := ac.codecClient.NewStream(streamID, responseDecoder)
		cb.OnReady(streamID, streamEncoder, p.host)
	}

	return nil
}

func (p *connPool) Close() {
	p.mux.Lock()
	defer p.mux.Unlock()

	for _, acs := range p.activeClients {
		for _, ac := range acs {
			ac.codecClient.Close()
		}
	}
}

func (p *connPool) onConnectionEvent(client *activeClient, event types.ConnectionEvent) {
	if event.IsClose() {

		if client.closeWithActiveReq {
			if event == types.LocalClose {
				p.host.HostStats().UpstreamConnectionLocalCloseWithActiveRequest.Inc(1)
				p.host.ClusterInfo().Stats().UpstreamConnectionLocalCloseWithActiveRequest.Inc(1)
			} else if event == types.RemoteClose {
				p.host.HostStats().UpstreamConnectionRemoteCloseWithActiveRequest.Inc(1)
				p.host.ClusterInfo().Stats().UpstreamConnectionRemoteCloseWithActiveRequest.Inc(1)
			}
		}

		p.mux.Lock()
		defer p.mux.Unlock()

		host := client.host.HostInfo.AddressString()
		delete(p.activeClients, host)
	} else if event == types.ConnectTimeout {
		p.host.HostStats().UpstreamRequestTimeout.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestTimeout.Inc(1)
		client.codecClient.Close()
	} else if event == types.ConnectFailed {
		p.host.HostStats().UpstreamConnectionConFail.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamConnectionConFail.Inc(1)
	}
}

func (p *connPool) onStreamDestroy(client *activeClient) {
	p.host.HostStats().UpstreamRequestActive.Dec(1)
	p.host.ClusterInfo().Stats().UpstreamRequestActive.Dec(1)
	p.host.ClusterInfo().ResourceManager().Requests().Decrease()
}

func (p *connPool) onStreamReset(client *activeClient, reason types.StreamResetReason) {
	if reason == types.StreamConnectionTermination || reason == types.StreamConnectionFailed {
		p.host.HostStats().UpstreamRequestFailureEject.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestFailureEject.Inc(1)
		client.closeWithActiveReq = true
	} else if reason == types.StreamLocalReset {
		p.host.HostStats().UpstreamRequestLocalReset.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestLocalReset.Inc(1)
	} else if reason == types.StreamRemoteReset {
		p.host.HostStats().UpstreamRequestRemoteReset.Inc(1)
		p.host.ClusterInfo().Stats().UpstreamRequestRemoteReset.Inc(1)
	}
}

func (p *connPool) onGoAway(client *activeClient) {
	p.host.HostStats().UpstreamConnectionCloseNotify.Inc(1)
	p.host.ClusterInfo().Stats().UpstreamConnectionCloseNotify.Inc(1)

	p.mux.Lock()
	defer p.mux.Unlock()

	host := client.host.HostInfo.AddressString()
	delete(p.activeClients, host)
}

func (p *connPool) createCodecClient(context context.Context, connData types.CreateConnectionData) str.CodecClient {
	return str.NewCodecClient(context, protocol.HTTP2, connData.Connection, connData.HostInfo)
}

// Http2 connpool interface
func (p *connPool) getOrInitActiveClient(context context.Context, addr string) *activeClient {
	p.mux.Lock()

	for _, ac := range p.activeClients[addr] {
		if ac.h2Conn.CanTakeNewRequest() {
			p.mux.Unlock()

			return ac
		}
	}

	// If connection's stream id is out of bound, closed or 'go away', make a new one
	if nac := newActiveClient(context, p); nac != nil {
		p.activeClients[addr] = append(p.activeClients[addr], nac)
		p.mux.Unlock()

		return nac
	}

	p.mux.Unlock()

	return nil
}

// GetClientConn
func (p *connPool) GetClientConn(req *http.Request, addr string) (*http2.ClientConn, error) {
	// GetClientConn will not be called, do nothing
	return nil, nil
}

// MarkDead by golang net http2 impl
func (p *connPool) MarkDead(http2Conn *http2.ClientConn) {
	p.mux.Lock()
	defer p.mux.Unlock()

	acsIdx := ""
	acIdx := -1

	for i, acs := range p.activeClients {
		for j, ac := range acs {

			if ac.h2Conn == http2Conn {
				acsIdx = i
				acIdx = j
				break
			}
		}
	}

	fmt.Printf("MarkDead %s, %d \n", acsIdx, acIdx)

	if acsIdx != "" && acIdx > -1 {
		p.activeClients[acsIdx] = append(p.activeClients[acsIdx][:acIdx],
			p.activeClients[acsIdx][acIdx+1:]...)
	}
}

// stream.CodecClientCallbacks
// types.ConnectionEventListener
// types.StreamConnectionEventListener
type activeClient struct {
	pool *connPool

	codecClient        str.CodecClient
	h2Conn             *http2.ClientConn
	host               types.CreateConnectionData
	totalStream        uint64
	closeWithActiveReq bool
}

func newActiveClient(ctx context.Context, pool *connPool) *activeClient {
	ac := &activeClient{
		pool: pool,
	}

	data := pool.host.CreateConnection(ctx)

	if err := data.Connection.Connect(false); err != nil {
		return nil
	}

	if transport == nil {
		transport = &http2.Transport{
			ConnPool: connPoolInstance,
		}
	}

	h2Conn, err := transport.NewClientConn(data.Connection.RawConn())

	if err != nil {
		return nil
	}

	codecClient := pool.createCodecClient(context.WithValue(ctx, H2ConnKey, h2Conn), data)
	codecClient.AddConnectionCallbacks(ac)
	codecClient.SetCodecClientCallbacks(ac)
	codecClient.SetCodecConnectionCallbacks(ac)

	ac.host = data
	ac.h2Conn = h2Conn
	ac.codecClient = codecClient

	pool.host.HostStats().UpstreamConnectionTotal.Inc(1)
	pool.host.HostStats().UpstreamConnectionActive.Inc(1)
	pool.host.HostStats().UpstreamConnectionTotalHTTP2.Inc(1)
	pool.host.ClusterInfo().Stats().UpstreamConnectionTotal.Inc(1)
	pool.host.ClusterInfo().Stats().UpstreamConnectionActive.Inc(1)
	pool.host.ClusterInfo().Stats().UpstreamConnectionTotalHTTP2.Inc(1)

	codecClient.SetConnectionStats(&types.ConnectionStats{
		ReadTotal:    pool.host.ClusterInfo().Stats().UpstreamBytesRead,
		ReadCurrent:  pool.host.ClusterInfo().Stats().UpstreamBytesReadCurrent,
		WriteTotal:   pool.host.ClusterInfo().Stats().UpstreamBytesWrite,
		WriteCurrent: pool.host.ClusterInfo().Stats().UpstreamBytesWriteCurrent,
	})

	return ac
}

func (ac *activeClient) OnEvent(event types.ConnectionEvent) {
	ac.pool.onConnectionEvent(ac, event)
}

func (ac *activeClient) OnStreamDestroy() {
	ac.pool.onStreamDestroy(ac)
}

func (ac *activeClient) OnStreamReset(reason types.StreamResetReason) {
	ac.pool.onStreamReset(ac, reason)
}

func (ac *activeClient) OnGoAway() {
	ac.pool.onGoAway(ac)
}
