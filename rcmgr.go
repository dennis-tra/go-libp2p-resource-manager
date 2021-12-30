package rcmgr

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
)

type resourceManager struct {
	limits Limiter

	system    *systemScope
	transient *transientScope

	cancelCtx context.Context
	cancel    func()
	wg        sync.WaitGroup

	mx    sync.Mutex
	svc   map[string]*serviceScope
	proto map[protocol.ID]*protocolScope
	peer  map[peer.ID]*peerScope
}

var _ network.ResourceManager = (*resourceManager)(nil)

type systemScope struct {
	*resourceScope
}

var _ network.ResourceScope = (*systemScope)(nil)

type transientScope struct {
	*resourceScope

	system *systemScope
}

var _ network.ResourceScope = (*transientScope)(nil)

type serviceScope struct {
	*resourceScope

	name   string
	system *systemScope
}

var _ network.ServiceScope = (*serviceScope)(nil)

type protocolScope struct {
	*resourceScope

	proto  protocol.ID
	system *systemScope
}

var _ network.ProtocolScope = (*protocolScope)(nil)

type peerScope struct {
	*resourceScope

	peer  peer.ID
	rcmgr *resourceManager
}

var _ network.PeerScope = (*peerScope)(nil)

type connectionScope struct {
	*resourceScope

	dir   network.Direction
	usefd bool
	rcmgr *resourceManager
	peer  *peerScope
}

var _ network.ConnectionScope = (*connectionScope)(nil)
var _ network.ConnectionManagementScope = (*connectionScope)(nil)

type streamScope struct {
	*resourceScope

	dir   network.Direction
	rcmgr *resourceManager
	peer  *peerScope
	svc   *serviceScope
	proto *protocolScope
}

var _ network.StreamScope = (*streamScope)(nil)
var _ network.StreamManagementScope = (*streamScope)(nil)

func NewResourceManager(limits Limiter) network.ResourceManager {
	r := &resourceManager{
		limits: limits,
		svc:    make(map[string]*serviceScope),
		proto:  make(map[protocol.ID]*protocolScope),
		peer:   make(map[peer.ID]*peerScope),
	}

	r.system = newSystemScope(limits.GetSystemLimits())
	r.system.IncRef()
	r.transient = newTransientScope(limits.GetTransientLimits(), r.system)
	r.transient.IncRef()

	r.cancelCtx, r.cancel = context.WithCancel(context.Background())

	r.wg.Add(1)
	go r.background()

	return r
}

func (r *resourceManager) ViewSystem(f func(network.ResourceScope) error) error {
	return f(r.system)
}

func (r *resourceManager) ViewTransient(f func(network.ResourceScope) error) error {
	return f(r.transient)
}

func (r *resourceManager) ViewService(srv string, f func(network.ServiceScope) error) error {
	s := r.getServiceScope(srv)
	defer s.DecRef()

	return f(s)
}

func (r *resourceManager) ViewProtocol(proto protocol.ID, f func(network.ProtocolScope) error) error {
	s := r.getProtocolScope(proto)
	defer s.DecRef()

	return f(s)
}

func (r *resourceManager) ViewPeer(p peer.ID, f func(network.PeerScope) error) error {
	s := r.getPeerScope(p)
	defer s.DecRef()

	return f(s)
}

func (r *resourceManager) getServiceScope(svc string) *serviceScope {
	r.mx.Lock()
	defer r.mx.Unlock()

	s, ok := r.svc[svc]
	if !ok {
		s = newServiceScope(svc, r.limits.GetServiceLimits(svc), r.system)
		r.svc[svc] = s
	}

	s.IncRef()
	return s
}

func (r *resourceManager) getProtocolScope(proto protocol.ID) *protocolScope {
	r.mx.Lock()
	defer r.mx.Unlock()

	s, ok := r.proto[proto]
	if !ok {
		s = newProtocolScope(proto, r.limits.GetProtocolLimits(proto), r.system)
		r.proto[proto] = s
	}

	s.IncRef()
	return s
}

func (r *resourceManager) getPeerScope(p peer.ID) *peerScope {
	r.mx.Lock()
	defer r.mx.Unlock()

	s, ok := r.peer[p]
	if !ok {
		s = newPeerScope(p, r.limits.GetPeerLimits(p), r)
		r.peer[p] = s
	}

	s.IncRef()
	return s
}

func (r *resourceManager) OpenConnection(dir network.Direction, usefd bool) (network.ConnectionManagementScope, error) {
	conn := newConnectionScope(dir, usefd, r.limits.GetConnLimits(), r)

	if err := conn.AddConn(dir, usefd); err != nil {
		conn.Done()
		return nil, err
	}

	return conn, nil
}

func (r *resourceManager) OpenStream(p peer.ID, dir network.Direction) (network.StreamManagementScope, error) {
	peer := r.getPeerScope(p)
	stream := newStreamScope(dir, r.limits.GetStreamLimits(p), peer)
	peer.DecRef() // we have the reference in constraints

	err := stream.AddStream(dir)
	if err != nil {
		stream.Done()
		return nil, err
	}

	return stream, nil
}

func (r *resourceManager) Close() error {
	r.cancel()
	r.wg.Wait()

	return nil
}

func (r *resourceManager) background() {
	defer r.wg.Done()

	// periodically garbage collects unused peer and protocol scopes
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.gc()
		case <-r.cancelCtx.Done():
			return
		}
	}
}

func (r *resourceManager) gc() {
	r.mx.Lock()
	defer r.mx.Unlock()

	for proto, s := range r.proto {
		if s.IsUnused() {
			s.Done()
			delete(r.proto, proto)
		}
	}

	for p, s := range r.peer {
		if s.IsUnused() {
			s.Done()
			delete(r.peer, p)
		}
	}
}

func newSystemScope(limit Limit) *systemScope {
	return &systemScope{
		resourceScope: newResourceScope(limit, nil),
	}
}

func newTransientScope(limit Limit, system *systemScope) *transientScope {
	return &transientScope{
		resourceScope: newResourceScope(limit, []*resourceScope{system.resourceScope}),
		system:        system,
	}
}

func newServiceScope(name string, limit Limit, system *systemScope) *serviceScope {
	return &serviceScope{
		resourceScope: newResourceScope(limit, []*resourceScope{system.resourceScope}),
		name:          name,
		system:        system,
	}
}

func newProtocolScope(proto protocol.ID, limit Limit, system *systemScope) *protocolScope {
	return &protocolScope{
		resourceScope: newResourceScope(limit, []*resourceScope{system.resourceScope}),
		proto:         proto,
		system:        system,
	}
}

func newPeerScope(p peer.ID, limit Limit, rcmgr *resourceManager) *peerScope {
	return &peerScope{
		resourceScope: newResourceScope(limit, []*resourceScope{rcmgr.system.resourceScope}),
		peer:          p,
		rcmgr:         rcmgr,
	}
}

func newConnectionScope(dir network.Direction, usefd bool, limit Limit, rcmgr *resourceManager) *connectionScope {
	return &connectionScope{
		resourceScope: newResourceScope(limit, []*resourceScope{rcmgr.transient.resourceScope, rcmgr.system.resourceScope}),
		dir:           dir,
		usefd:         usefd,
		rcmgr:         rcmgr,
	}
}

func newStreamScope(dir network.Direction, limit Limit, peer *peerScope) *streamScope {
	return &streamScope{
		resourceScope: newResourceScope(limit, []*resourceScope{peer.resourceScope, peer.rcmgr.transient.resourceScope, peer.rcmgr.system.resourceScope}),
		dir:           dir,
		rcmgr:         peer.rcmgr,
		peer:          peer,
	}
}

func (s *serviceScope) Name() string {
	return s.name
}

func (s *protocolScope) Protocol() protocol.ID {
	return s.proto
}

func (s *peerScope) Peer() peer.ID {
	return s.peer
}

func (s *connectionScope) PeerScope() network.PeerScope {
	s.Lock()
	defer s.Unlock()
	return s.peer
}

func (s *connectionScope) SetPeer(p peer.ID) error {
	s.Lock()
	defer s.Unlock()

	if s.peer != nil {
		return fmt.Errorf("connection scope already attached to a peer")
	}
	s.peer = s.rcmgr.getPeerScope(p)

	// juggle resources from transient scope to peer scope
	stat := s.resourceScope.rc.stat()
	if err := s.peer.ReserveForChild(stat); err != nil {
		s.peer.DecRef()
		s.peer = nil
		return err
	}

	s.rcmgr.transient.ReleaseForChild(stat)
	s.rcmgr.transient.DecRef() // removed from constraints

	// update constraints
	constraints := []*resourceScope{
		s.peer.resourceScope,
		s.rcmgr.system.resourceScope,
	}
	s.resourceScope.constraints = constraints

	return nil
}

func (s *streamScope) ProtocolScope() network.ProtocolScope {
	s.Lock()
	defer s.Unlock()
	return s.proto
}

func (s *streamScope) SetProtocol(proto protocol.ID) error {
	s.Lock()
	defer s.Unlock()

	if s.proto != nil {
		return fmt.Errorf("stream scope already attached to a protocol")
	}

	s.proto = s.rcmgr.getProtocolScope(proto)

	// juggle resources from transient scope to protocol scope
	stat := s.resourceScope.rc.stat()
	if err := s.proto.ReserveForChild(stat); err != nil {
		s.proto.DecRef()
		s.proto = nil
		return err
	}

	s.rcmgr.transient.ReleaseForChild(stat)
	s.rcmgr.transient.DecRef() // removed from constraints

	// update constraints
	constraints := []*resourceScope{
		s.peer.resourceScope,
		s.proto.resourceScope,
		s.rcmgr.system.resourceScope,
	}
	s.resourceScope.constraints = constraints

	return nil
}

func (s *streamScope) ServiceScope() network.ServiceScope {
	s.Lock()
	defer s.Unlock()
	return s.svc
}

func (s *streamScope) SetService(svc string) error {
	s.Lock()
	defer s.Unlock()

	if s.proto == nil {
		return fmt.Errorf("stream scope not attached to a protocol")
	}
	if s.svc != nil {
		return fmt.Errorf("stream scope already attached to a service")
	}

	s.svc = s.rcmgr.getServiceScope(svc)

	// reserve resources in service
	if err := s.svc.ReserveForChild(s.resourceScope.rc.stat()); err != nil {
		s.svc.DecRef()
		s.svc = nil
		return err
	}

	// update constraints
	constraints := []*resourceScope{
		s.peer.resourceScope,
		s.proto.resourceScope,
		s.svc.resourceScope,
		s.rcmgr.system.resourceScope,
	}
	s.resourceScope.constraints = constraints

	return nil
}

func (s *streamScope) PeerScope() network.PeerScope {
	s.Lock()
	defer s.Unlock()
	return s.peer
}
