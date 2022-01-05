# The libp2p Network Resource Manager

This package contains the canonical implementation of the libp2p
Network Resource Manager interface.

The implementation is based on the concept of Resource Management
Scopes, whereby resource usage is constrained by a DAG of scopes,
accounting for multiple levels of resource constraints.

## Design Considerations

- The Resource Manager must account for basic resource usage at all
  levels of the stack, from the internals to the application.
- Basic resources include memory, streams, connections, and file
  descriptors. These account for both space and time used by
  the stack, as each resource has a direct effect on the system
  availability and performance.
- The design must support seamless integration for user applications,
  which should reap the benefits of resource management without any
  changes. That is, existing applications should be oblivious of the
  resource manager and transparently obtain limits which protect it
  from resource exhaustion and OOM conditions.
- At the same time, the design must support opt-in resource usage
  accounting for applications who want to explicitly utilize the
  facilities of the system to inform about and constrain their own
  resource usage.
- The design must allow the user to set its own limits, which can be
  static (fixed) or dynamic.


## Basic Resources

### Memory

Perhaps the most fundamental resource is memory, and in particular
buffers used for network operations. The system must provide an
interface for components to reserve memory that accounts for buffers
(and possibly other live objects), which is scoped within the component.
Before a new buffer is allocated, the component should try a memory
reservation, which can fail if the resource limit is exceeded. It is
then up to the component to react to the error condition, depending on
the situation. For example, a muxer failing to grow a buffer in
response to a window change should simply retain the old buffer and
operate at perhaps degraded performance.

### File Descriptors

File descriptors are an important resource that uses memory (and
computational time) at the system level. They are also a scarce
resource, as typically (unless the user explicitly intervenes) they
are constrained by the system. Exhaustion of file descriptors may
render the application incapable of operating (e.g. because it is
unable to open a file).

### Connections

Connections are a higher level concept endemic to libp2p; in order to
communicate with another peer, a connection must first be
established. Connections are an important resource in libp2p, as they
consume memory, goroutines, and possibly file descriptors.

We distinguish between inbound and outbound connections, as the former
are initiated by remote peers and consume resources in response to
network events and thus need to be tightly controlled in order to
protect the application from overload or attack.  Outbound
connections are typically initiated by the application's volition and
don't need to be controlled as tightly. However, outbound connections
still consume resources and may be initiated in response to network
events because of (potentially faulty) application logic, so they
still need to be constrained.

### Streams

Streams are the fundamental object of interaction in libp2p; all
protocol interactions happen through a stream that goes over some
connection. Streams are a fundamental resource in libp2p, as they
consume memory and goroutines at all levels of the stack.

Streams always belong to a peer, specify a protocol and they may
belong to some service in the system. Hence, this suggests that apart
from global limits, we can constrain stream usage at finer
granularity, at the protocol and service level.

Once again, we disinguish between inbound and outbound streams.
Inbound streams are initiated by remote peers and consume resources in
response to network events; controlling inbound stream usage is again
paramount for protecting the system from overload or attack.
Outbound streams are normally initiated by the application or some
service in the system in order to effect some protocol
interaction. However, they can also be initiated in response to
network events because of application or service logic, so we still
need to constrain them.


## Resource Scopes

The Resource Manager is based on the concept of resource
scopes. Resource Scopes account for resource usage that is temporally
delimited for the span of the scope. Resource Scopes conceptually
form a DAG, providing us with a mechanism to enforce multiresolution
resource accounting. Downstream resource usage is aggregated at scopes
higher up the graph.

The following diagram depicts the canonical scope graph:
```
System
  +------------> Transient.............+................+
  |                                    .                .
  +------------>  Service------------- . ----------+    .
  |                                    .           |    .
  +------------->  Protocol----------- . ----------+    .
  |                                    .           |    .
  +-------------->* Peer               \/          |    .
                     +------------> Connection     |    .
                     |                             \/   \/
                     +--------------------------->  Stream
```

### The System Scope

The system scope is the top level scope that accounts for global
resource usage at all levels of the system. This scope constrains all
other scopes and institutes global hard limits.

### The Transient Scope

The transient scope accounts for resources that are in the process of
full establishment.  For instance, a new connection prior to the
handshake does not belong to any peer, but it still needs to be
constrained as this opens an avenue for attacks in transient resource
usage. Similarly, a stream that has not negotiated a protocol yet is
constrained by the transient scope.

### Service Scopes

The system is typically organized across services, which may be
ambient and provide basic functionality to the system (e.g. identify,
autonat, relay, etc). Alternatively, services may be explicitly
instantiated by the application, and provide core components of its
functionality (e.g. pubsub, the DHT, etc).

Services consume resources such as memory and may directly own streams
that implement their protocol flow. Services typically have some
stream handler, so they are subject to inbound stream creation and
resource usage in response to network events. As such, the system
explicitly models them allowing for isolated resource usage that can
be tuned by the user.

### Protocol Scopes

Protocol Scopes account for resources at the protocol level. They are
an intermediate resource scope which can constrain streams which may
not have a service associated or for resource control within a
service.

For instance, a service that is not aware of the resource manager and
has not been ported to mark its streams, may still gain limits
transparently without any programmer intervention.  Furthermore, the
protocol scope can constrain resource usage for services that
implement multiple protocols for the shake of backwards
compatibility. A tighter limit in some older protocol can protect the
application from resource consumption caused by legacy clients or
potential attacks.

For a concrete example, consider pubsub with the gossipsub router: the
service also understands the floodsub protocol for backwards
compatibility and support for unsophisticated clients that are lagging
in the implementation effort. By specifying a lower limit for the
floodsub protocol, we can can constrain the service level for legacy
clients using an inefficient protocol.

### Peer Scopes

The peer scope accounts for resource usage by an individual peer. This
constrains connections and streams and limits the blast radius of
resource consumption by a single remote peer.

### Connection Scopes

The connection scope is delimited to the duration of a connection and
constrains resource usage by a single connection. The scope is a leaf
in the DAG, with a span that begins when a connection is established
and ends when the connection is closed. Its resources are aggregated
to the resource usage of a peer.

### Stream Scopes

The stream scope is delimited to the duration of a stream, and
constrains resource usage by a single stream. This scope is also a
leaf in the DAG, with span that begins when a stream is created and
ends when the stream is closed. Its resources are aggregated to the
resource usage of a peer, and constrained by a service and protocol
scope.

### User Transaction Scopes

User transaction scopes can be created as a child of any extant
resource scope, and provide the prgrammer with a delimited scope for
easy resource accounting. Transactions may form a tree that is rooted
to some canonical scope in the scope DAG.

For instance, a programmer may create a transaction scope within a
service that accounts for some control flow delimited resource
usage. Similarly, a programmer may create a transaction scope for some
interaction within a stream, e.g. a Request/Response interaction that
uses a buffer.

## Limits

Each resource scope has an associated limit object, which designates
limits for all basic resources.  The limit is checked every time some
resource is reserved and provides the system with an opportunity to
constrain resource usage.

There are separate limits for each class of scope, allowing us for
multiresolution and aggregate resource accounting.  As such, we have
limits for the system and transient scopes, default and specific
limits for services, protocols, and peers, and limits for connections
and streams.

## Implementation Notes

- The package only exports a constructor for the resource manager and
  basic types for defining limits. Internals are not exposed.
- Internally, there is a resources object that is embedded in every scope and
  implements resource accounting.
- There is a single implementation of a generic resource scope, that
  provides all necessary interface methods.
- There are concrete types for all canonical scopes, embedding a
  pointer to a generic resource scope.
- Peer and Protocol scopes, which may be created in response to
  network events, are periodically garbage collected.
