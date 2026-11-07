package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	nodeagentpb "github.com/romanspb80/evpn-for-ovn/gen/node_agent_cni"
	"github.com/romanspb80/evpn-for-ovn/pkg/attachment"
	"github.com/romanspb80/evpn-for-ovn/pkg/endpoint"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultSocketMode      = 0o600
	defaultShutdownTimeout = 5 * time.Second
	maxNameLen             = 63
)

type endpointRecord struct {
	identity     *nodeagentpb.EndpointIdentity
	runtime      *nodeagentpb.CNIRuntimeContext
	attachment   *nodeagentpb.AttachmentRequest
	result       *nodeagentpb.CNIResult
	endpointSpec endpoint.EndpointSpec
}

type Store struct {
	mu      sync.RWMutex
	records map[string]*endpointRecord
}

func NewStore() *Store {
	return &Store{records: make(map[string]*endpointRecord)}
}

func (s *Store) get(key string) (*endpointRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[key]
	if !ok {
		return nil, false
	}
	return cloneRecord(rec), true
}

func (s *Store) putIfAbsentOrEqual(key string, rec *endpointRecord) (*endpointRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.records[key]
	if !ok {
		s.records[key] = cloneRecord(rec)
		return nil, false
	}
	return cloneRecord(existing), true
}

func (s *Store) deleteIfMatch(key string, reqID *nodeagentpb.EndpointIdentity) (found bool, deleted bool, stale bool, rec *endpointRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.records[key]
	if !ok {
		return false, false, false, nil
	}
	if isStaleDelete(existing.identity, reqID) {
		return true, false, true, cloneRecord(existing)
	}
	return true, true, false, cloneRecord(existing)
}

func (s *Store) deleteIfRecordMatches(key string, rec *endpointRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.records[key]
	if !ok {
		return
	}
	if equivalentRecord(existing, rec) {
		delete(s.records, key)
	}
}

func (s *Store) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, key)
}

type CNIServer struct {
	nodeagentpb.UnimplementedNodeAgentCNIServer
	operations        sync.Mutex
	continuity        *cniContinuity
	stopping          bool
	store             *Store
	endpoint          endpoint.EndpointManager
	network           networkBackend
	attachmentChecker attachmentStateChecker
}

type attachmentStateChecker interface {
	CheckAttachment(ctx context.Context, spec endpoint.EndpointSpec) error
}

func NewCNIServer(store *Store) *CNIServer {
	return NewCNIServerWithBackends(store, newInMemoryEndpointManager(), newInMemoryNetworkBackend())
}

func NewCNIServerWithManager(store *Store, endpointManager endpoint.EndpointManager) *CNIServer {
	return NewCNIServerWithBackends(store, endpointManager, newInMemoryNetworkBackend())
}

func NewCNIServerWithBackends(store *Store, endpointManager endpoint.EndpointManager, networkManager networkBackend) *CNIServer {
	return NewCNIServerWithBackendsAndChecker(store, endpointManager, networkManager, nil)
}

func NewCNIServerWithBackendsAndChecker(store *Store, endpointManager endpoint.EndpointManager, networkManager networkBackend, checker attachmentStateChecker) *CNIServer {
	if store == nil {
		store = NewStore()
	}
	if endpointManager == nil {
		endpointManager = newInMemoryEndpointManager()
	}
	if networkManager == nil {
		networkManager = newInMemoryNetworkBackend()
	}
	return &CNIServer{
		store:             store,
		endpoint:          endpointManager,
		network:           networkManager,
		attachmentChecker: checker,
	}
}

func (s *CNIServer) Add(ctx context.Context, req *nodeagentpb.CNIAddRequest) (*nodeagentpb.CNIAddResponse, error) {
	s.operations.Lock()
	defer s.operations.Unlock()
	if s.stopping {
		return nil, status.Error(codes.Unavailable, "CNI operations drained")
	}
	if s.continuity != nil {
		return s.continuity.add(ctx, s, req)
	}
	identity, runtime, attachment, err := validateAddRequest(req)
	if err != nil {
		return nil, err
	}
	spec, err := mapCNIAddRequestToEndpointSpec(req)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to map add request: %v", err)
	}

	key := endpointKey(identity)
	rec := &endpointRecord{
		identity:     cloneIdentity(identity),
		runtime:      cloneRuntime(runtime),
		attachment:   cloneAttachment(attachment),
		result:       buildResultFromStaticConfig(runtime, attachment),
		endpointSpec: spec,
	}

	netState, createdNetwork, netErr := s.network.Ensure(ctx, key, rec)
	if netErr != nil {
		if strings.Contains(netErr.Error(), "network attachment conflict") {
			return nil, status.Errorf(codes.AlreadyExists, "conflicting ADD for endpoint_id=%q", identity.GetEndpointId())
		}
		return nil, status.Errorf(codes.FailedPrecondition, "failed to ensure network attachment: %v", netErr)
	}
	if hostIfName := strings.TrimSpace(netState.HostIfName); hostIfName != "" {
		rec.endpointSpec.OVSInterfaceName = hostIfName
	}
	rec.result = buildResultFromNetworkState(runtime, rec.attachment, netState)

	existing, exists := s.store.putIfAbsentOrEqual(key, rec)
	if exists {
		if !equivalentRecord(existing, rec) {
			if createdNetwork {
				_ = s.network.Delete(ctx, key, rec)
			}
			return nil, status.Errorf(codes.AlreadyExists, "conflicting ADD for endpoint_id=%q", identity.GetEndpointId())
		}
		if _, ensureErr := s.endpoint.EnsureEndpoint(ctx, rec.endpointSpec); ensureErr != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "failed to ensure endpoint for idempotent add: %v", ensureErr)
		}
		return &nodeagentpb.CNIAddResponse{
			Success: true,
			Message: "endpoint already exists",
			Result:  cloneResult(rec.result),
			Outcome: &nodeagentpb.AddOutcome{
				Identity:         cloneIdentity(existing.identity),
				IdempotentReplay: true,
				Message:          "idempotent replay",
			},
		}, nil
	}

	if _, ensureErr := s.endpoint.EnsureEndpoint(ctx, rec.endpointSpec); ensureErr != nil {
		if createdNetwork {
			_ = s.network.Delete(ctx, key, rec)
		}
		s.store.delete(key)
		return nil, status.Errorf(codes.FailedPrecondition, "failed to ensure endpoint: %v", ensureErr)
	}

	return &nodeagentpb.CNIAddResponse{
		Success: true,
		Message: "endpoint created",
		Result:  cloneResult(rec.result),
		Outcome: &nodeagentpb.AddOutcome{
			Identity:         cloneIdentity(identity),
			IdempotentReplay: false,
			Message:          "created",
		},
	}, nil
}

func (s *CNIServer) Del(ctx context.Context, req *nodeagentpb.CNIDelRequest) (*nodeagentpb.CNIDelResponse, error) {
	s.operations.Lock()
	defer s.operations.Unlock()
	if s.stopping {
		return nil, status.Error(codes.Unavailable, "CNI operations drained")
	}
	if s.continuity != nil {
		return s.continuity.del(ctx, s, req)
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	identity, err := validateIdentity(req.GetIdentity())
	if err != nil {
		return nil, err
	}
	key := endpointKey(identity)
	found, deleted, stale, rec := s.store.deleteIfMatch(key, identity)
	if !found {
		if s.attachmentChecker == nil {
			return &nodeagentpb.CNIDelResponse{
				Success:  true,
				Message:  "endpoint already absent",
				Outcome:  nodeagentpb.DelOutcome_DEL_OUTCOME_ALREADY_ABSENT,
				Identity: cloneIdentity(identity),
			}, nil
		}
		recoveredRec, recErr := recoverRecordFromDelRequest(req)
		if recErr != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "cannot establish endpoint state for delete: %v", recErr)
		}
		if delErr := s.endpoint.DeleteEndpoint(ctx, recoveredRec.endpointSpec); delErr != nil {
			if isStaleOperationErr(delErr) {
				return &nodeagentpb.CNIDelResponse{
					Success:  true,
					Message:  "stale delete rejected",
					Outcome:  nodeagentpb.DelOutcome_DEL_OUTCOME_STALE_OPERATION,
					Identity: cloneIdentity(identity),
				}, nil
			}
			return nil, status.Errorf(codes.FailedPrecondition, "failed to delete endpoint: %v", delErr)
		}
		if netErr := s.network.Delete(ctx, key, recoveredRec); netErr != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "failed to delete network attachment: %v", netErr)
		}
		return &nodeagentpb.CNIDelResponse{
			Success:  true,
			Message:  "endpoint deleted",
			Outcome:  nodeagentpb.DelOutcome_DEL_OUTCOME_DELETED,
			Identity: cloneIdentity(identity),
		}, nil
	}
	if stale {
		return &nodeagentpb.CNIDelResponse{
			Success:  true,
			Message:  "stale delete rejected",
			Outcome:  nodeagentpb.DelOutcome_DEL_OUTCOME_STALE_OPERATION,
			Identity: cloneIdentity(identity),
		}, nil
	}
	if deleted {
		if rec != nil {
			if delErr := s.endpoint.DeleteEndpoint(ctx, rec.endpointSpec); delErr != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "failed to delete endpoint: %v", delErr)
			}
			if netErr := s.network.Delete(ctx, key, rec); netErr != nil {
				return nil, status.Errorf(codes.FailedPrecondition, "failed to delete network attachment: %v", netErr)
			}
			s.store.deleteIfRecordMatches(key, rec)
		}
		return &nodeagentpb.CNIDelResponse{
			Success:  true,
			Message:  "endpoint deleted",
			Outcome:  nodeagentpb.DelOutcome_DEL_OUTCOME_DELETED,
			Identity: cloneIdentity(identity),
		}, nil
	}
	return nil, status.Error(codes.Internal, "unexpected delete state")
}

func (s *CNIServer) Check(ctx context.Context, req *nodeagentpb.CNICheckRequest) (*nodeagentpb.CNICheckResponse, error) {
	s.operations.Lock()
	defer s.operations.Unlock()
	if s.stopping {
		return nil, status.Error(codes.Unavailable, "CNI operations drained")
	}
	if s.continuity != nil {
		if err := s.continuity.check(ctx, s, req); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	identity, err := validateIdentity(req.GetIdentity())
	if err != nil {
		return nil, err
	}
	key := endpointKey(identity)
	rec, ok := s.store.get(key)
	if !ok {
		if s.attachmentChecker == nil {
			return nil, status.Errorf(codes.NotFound, "endpoint_id=%q not found", identity.GetEndpointId())
		}
		recoveredRec, recErr := recoverRecordFromCheckRequest(req)
		if recErr != nil {
			return nil, status.Errorf(codes.NotFound, "endpoint_id=%q not found", identity.GetEndpointId())
		}
		rec = recoveredRec
	}
	if isIdentityMismatch(rec.identity, identity) {
		return nil, status.Errorf(codes.FailedPrecondition, "identity mismatch for endpoint_id=%q", identity.GetEndpointId())
	}
	if identityChecker, ok := s.endpoint.(interface {
		CheckEndpointIdentity(context.Context, endpoint.EndpointSpec) error
	}); ok {
		if err := identityChecker.CheckEndpointIdentity(ctx, rec.endpointSpec); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "endpoint identity check failed for endpoint_id=%q: %v", identity.GetEndpointId(), err)
		}
	} else if _, getErr := s.endpoint.GetEndpoint(ctx, rec.endpointSpec.ID, rec.endpointSpec.LogicalSwitchPort); getErr != nil {
		return nil, status.Errorf(codes.NotFound, "endpoint_id=%q not found in endpoint manager: %v", identity.GetEndpointId(), getErr)
	}
	netState, netErr := s.network.Check(ctx, key, rec)
	if netErr != nil {
		if errors.Is(netErr, errAttachmentNotFound) {
			return nil, status.Errorf(codes.NotFound, "network attachment missing for endpoint_id=%q", identity.GetEndpointId())
		}
		return nil, status.Errorf(codes.FailedPrecondition, "network attachment check failed for endpoint_id=%q: %v", identity.GetEndpointId(), netErr)
	}

	ovsPresent := strings.TrimSpace(netState.HostIfName) != ""
	if s.attachmentChecker != nil {
		rec.endpointSpec.OVSInterfaceName = netState.HostIfName
		if err := s.attachmentChecker.CheckAttachment(ctx, rec.endpointSpec); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "ovs attachment check failed for endpoint_id=%q: %v", identity.GetEndpointId(), err)
		}
		ovsPresent = true
	}

	return &nodeagentpb.CNICheckResponse{
		Success:  true,
		Message:  "endpoint present",
		Identity: cloneIdentity(rec.identity),
		Details: &nodeagentpb.CheckDetails{
			EndpointIdentityValid:  true,
			OvnPortPresent:         true,
			OvsAttachmentPresent:   ovsPresent,
			NetnsAttachmentPresent: strings.TrimSpace(netState.ContainerIfName) != "",
			Message:                "validated against endpoint and network backends",
		},
	}, nil
}

func recoverRecordFromCheckRequest(req *nodeagentpb.CNICheckRequest) (*endpointRecord, error) {
	if req == nil {
		return nil, errors.New("request is required")
	}
	return buildRecoveredRecord(req.GetIdentity(), req.GetRuntime(), req.GetAttachment(), req.GetK8S())
}

func recoverRecordFromDelRequest(req *nodeagentpb.CNIDelRequest) (*endpointRecord, error) {
	if req == nil {
		return nil, errors.New("request is required")
	}
	return buildRecoveredRecord(req.GetIdentity(), req.GetRuntime(), req.GetAttachment(), req.GetK8S())
}

func buildRecoveredRecord(identity *nodeagentpb.EndpointIdentity, runtime *nodeagentpb.CNIRuntimeContext, attachmentReq *nodeagentpb.AttachmentRequest, k8s *nodeagentpb.K8SIdentity) (*endpointRecord, error) {
	identity, err := validateIdentity(identity)
	if err != nil {
		return nil, err
	}
	if runtime == nil {
		return nil, errors.New("runtime is required")
	}
	if attachmentReq == nil {
		attachmentReq = &nodeagentpb.AttachmentRequest{}
	}
	logicalSwitch := firstNonEmpty(strings.TrimSpace(attachmentReq.GetLogicalSwitch()), "ls-"+sanitizeName(identity.GetNetworkId()))
	logicalSwitchPort := firstNonEmpty(strings.TrimSpace(attachmentReq.GetOvsInterfaceName()), strings.TrimSpace(runtime.GetIfName()))
	if logicalSwitchPort == "" {
		logicalSwitchPort = "ep"
	}
	logicalSwitchPort = trimName(strings.Join([]string{
		logicalSwitchPort,
		sanitizeName(identity.GetNetworkId()),
		sanitizeName(identity.GetAttachmentId()),
		sanitizeName(identity.GetEndpointId()),
	}, "-"))
	ovsInterfaceName := firstNonEmpty(strings.TrimSpace(attachmentReq.GetOvsInterfaceName()), strings.TrimSpace(runtime.GetIfName()), logicalSwitchPort)
	workloadUID := firstNonEmpty(identity.GetWorkloadUid(), k8s.GetPodUid(), runtime.GetContainerId())
	spec := endpoint.EndpointSpec{
		ID:           endpoint.EndpointID(strings.TrimSpace(identity.GetEndpointId())),
		NetworkID:    strings.TrimSpace(identity.GetNetworkId()),
		AttachmentID: strings.TrimSpace(identity.GetAttachmentId()),
		Incarnation:  strings.TrimSpace(identity.GetIncarnation()),
		Workload: endpoint.WorkloadRef{
			Kind:      "pod",
			UID:       workloadUID,
			Name:      strings.TrimSpace(k8s.GetPodName()),
			Namespace: strings.TrimSpace(k8s.GetPodNamespace()),
			NodeName:  strings.TrimSpace(k8s.GetNodeName()),
		},
		LogicalSwitch:     logicalSwitch,
		LogicalSwitchPort: logicalSwitchPort,
		OVSInterfaceName:  ovsInterfaceName,
		MACAddress:        strings.TrimSpace(attachmentReq.GetStaticMac()),
	}
	if ip := strings.TrimSpace(attachmentReq.GetStaticIpCidr()); ip != "" {
		spec.IPAddresses = []string{ip}
	}
	rec := &endpointRecord{
		identity:     cloneIdentity(identity),
		runtime:      cloneRuntime(runtime),
		attachment:   cloneAttachment(attachmentReq),
		result:       buildResultFromStaticConfig(runtime, attachmentReq),
		endpointSpec: spec,
	}
	return rec, nil
}

func isStaleOperationErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, attachment.ErrStaleOperation) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "stale operation")
}

type UnixServer struct {
	socketPath string
	grpcServer *grpc.Server
	listener   net.Listener
	done       chan struct{}
	stopOnce   sync.Once
	drain      func()
}

func StartUnixServer(ctx context.Context, socketPath string, service nodeagentpb.NodeAgentCNIServer) (*UnixServer, error) {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return nil, errors.New("socket path is required")
	}
	if service == nil {
		return nil, errors.New("service is required")
	}
	if err := prepareSocketPath(socketPath); err != nil {
		return nil, err
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on unix socket %q: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, defaultSocketMode); err != nil {
		_ = listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("chmod socket %q: %w", socketPath, err)
	}

	var options []grpc.ServerOption
	if cni, ok := service.(*CNIServer); ok && cni.continuity != nil && cni.continuity.authority != nil {
		options = append(options, grpc.Creds(newCNICallerCredentials()))
	}
	grpcServer := grpc.NewServer(options...)
	nodeagentpb.RegisterNodeAgentCNIServer(grpcServer, service)
	server := &UnixServer{
		socketPath: socketPath,
		grpcServer: grpcServer,
		listener:   listener,
		done:       make(chan struct{}),
	}
	if cni, ok := service.(*CNIServer); ok {
		server.drain = func() {
			cni.operations.Lock()
			defer cni.operations.Unlock()
			cni.stopping = true
		}
	}

	go func() {
		defer close(server.done)
		if serveErr := grpcServer.Serve(listener); serveErr != nil && !errors.Is(serveErr, net.ErrClosed) && !errors.Is(serveErr, grpc.ErrServerStopped) {
			fmt.Fprintf(os.Stderr, "evpn-node-agent gRPC serve error: %v\n", serveErr)
		}
	}()

	go func() {
		<-ctx.Done()
		_ = server.Stop(context.Background())
	}()

	return server, nil
}

func (s *UnixServer) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var retErr error
	s.stopOnce.Do(func() {
		stopDone := make(chan struct{})
		go func() {
			s.grpcServer.GracefulStop()
			close(stopDone)
		}()

		shutdownCtx := ctx
		if shutdownCtx == nil {
			var cancel context.CancelFunc
			shutdownCtx, cancel = context.WithTimeout(context.Background(), defaultShutdownTimeout)
			defer cancel()
		}

		select {
		case <-stopDone:
		case <-shutdownCtx.Done():
			s.grpcServer.Stop()
		}

		_ = s.listener.Close()
		<-s.done
		if s.drain != nil {
			s.drain()
		}
		if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = fmt.Errorf("remove socket %q: %w", s.socketPath, err)
		}
	})
	return retErr
}

func prepareSocketPath(socketPath string) error {
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create socket dir %q: %w", dir, err)
	}

	info, err := os.Lstat(socketPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat socket path %q: %w", socketPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket path %q exists and is not a unix socket", socketPath)
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("remove stale socket %q: %w", socketPath, err)
	}
	return nil
}

func validateAddRequest(req *nodeagentpb.CNIAddRequest) (*nodeagentpb.EndpointIdentity, *nodeagentpb.CNIRuntimeContext, *nodeagentpb.AttachmentRequest, error) {
	if req == nil {
		return nil, nil, nil, status.Error(codes.InvalidArgument, "request is required")
	}
	identity, err := validateIdentity(req.GetIdentity())
	if err != nil {
		return nil, nil, nil, err
	}
	runtime := req.GetRuntime()
	if runtime == nil {
		return nil, nil, nil, status.Error(codes.InvalidArgument, "runtime is required")
	}
	if strings.TrimSpace(runtime.GetCniVersion()) == "" {
		return nil, nil, nil, status.Error(codes.InvalidArgument, "runtime.cni_version is required")
	}
	attachment := req.GetAttachment()
	if attachment == nil {
		attachment = &nodeagentpb.AttachmentRequest{}
	}
	if err := validateStaticConfig(attachment); err != nil {
		return nil, nil, nil, err
	}
	return identity, runtime, attachment, nil
}

func validateIdentity(identity *nodeagentpb.EndpointIdentity) (*nodeagentpb.EndpointIdentity, error) {
	if identity == nil {
		return nil, status.Error(codes.InvalidArgument, "identity is required")
	}
	if strings.TrimSpace(identity.GetEndpointId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "identity.endpoint_id is required")
	}
	if strings.TrimSpace(identity.GetNetworkId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "identity.network_id is required")
	}
	if strings.TrimSpace(identity.GetAttachmentId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "identity.attachment_id is required")
	}
	return identity, nil
}

func validateStaticConfig(attachment *nodeagentpb.AttachmentRequest) error {
	if ip := strings.TrimSpace(attachment.GetStaticIpCidr()); ip != "" {
		if _, _, err := net.ParseCIDR(ip); err != nil {
			return status.Errorf(codes.InvalidArgument, "attachment.static_ip_cidr invalid: %v", err)
		}
	}
	if mac := strings.TrimSpace(attachment.GetStaticMac()); mac != "" {
		if _, err := net.ParseMAC(mac); err != nil {
			return status.Errorf(codes.InvalidArgument, "attachment.static_mac invalid: %v", err)
		}
	}
	return nil
}

func mapCNIAddRequestToEndpointSpec(req *nodeagentpb.CNIAddRequest) (endpoint.EndpointSpec, error) {
	if req == nil {
		return endpoint.EndpointSpec{}, errors.New("request is required")
	}
	identity := req.GetIdentity()
	runtime := req.GetRuntime()
	attachment := req.GetAttachment()
	if identity == nil || runtime == nil {
		return endpoint.EndpointSpec{}, errors.New("identity and runtime are required")
	}
	if attachment == nil {
		attachment = &nodeagentpb.AttachmentRequest{}
	}

	logicalSwitch := firstNonEmpty(strings.TrimSpace(attachment.GetLogicalSwitch()), "ls-"+sanitizeName(identity.GetNetworkId()))
	logicalSwitchPort := firstNonEmpty(
		strings.TrimSpace(attachment.GetOvsInterfaceName()),
		strings.TrimSpace(runtime.GetIfName()),
	)
	if logicalSwitchPort == "" {
		logicalSwitchPort = "ep"
	}
	logicalSwitchPort = trimName(strings.Join([]string{
		logicalSwitchPort,
		sanitizeName(identity.GetNetworkId()),
		sanitizeName(identity.GetAttachmentId()),
		sanitizeName(identity.GetEndpointId()),
	}, "-"))

	ovsInterfaceName := firstNonEmpty(
		strings.TrimSpace(attachment.GetOvsInterfaceName()),
		strings.TrimSpace(runtime.GetIfName()),
		logicalSwitchPort,
	)

	macAddress := strings.TrimSpace(attachment.GetStaticMac())
	if macAddress == "" {
		return endpoint.EndpointSpec{}, errors.New("attachment.static_mac is required to create endpoint state")
	}

	spec := endpoint.EndpointSpec{
		ID:           endpoint.EndpointID(strings.TrimSpace(identity.GetEndpointId())),
		NetworkID:    strings.TrimSpace(identity.GetNetworkId()),
		AttachmentID: strings.TrimSpace(identity.GetAttachmentId()),
		Incarnation:  strings.TrimSpace(identity.GetIncarnation()),
		Workload: endpoint.WorkloadRef{
			Kind:      "pod",
			UID:       firstNonEmpty(identity.GetWorkloadUid(), req.GetK8S().GetPodUid(), runtime.GetContainerId()),
			Name:      strings.TrimSpace(req.GetK8S().GetPodName()),
			Namespace: strings.TrimSpace(req.GetK8S().GetPodNamespace()),
			NodeName:  strings.TrimSpace(req.GetK8S().GetNodeName()),
		},
		LogicalSwitch:     logicalSwitch,
		LogicalSwitchPort: logicalSwitchPort,
		OVSInterfaceName:  ovsInterfaceName,
		MACAddress:        macAddress,
	}
	if ip := strings.TrimSpace(attachment.GetStaticIpCidr()); ip != "" {
		spec.IPAddresses = []string{ip}
	}
	if spec.Workload.UID == "" {
		spec.Workload.UID = runtime.GetContainerId()
	}
	if err := spec.Validate(); err != nil {
		return endpoint.EndpointSpec{}, err
	}
	return spec, nil
}

func endpointKey(identity *nodeagentpb.EndpointIdentity) string {
	return strings.Join([]string{
		strings.TrimSpace(identity.GetNetworkId()),
		strings.TrimSpace(identity.GetAttachmentId()),
		strings.TrimSpace(identity.GetEndpointId()),
	}, "|")
}

func buildResultFromStaticConfig(runtime *nodeagentpb.CNIRuntimeContext, attachment *nodeagentpb.AttachmentRequest) *nodeagentpb.CNIResult {
	result := &nodeagentpb.CNIResult{
		CniVersion: strings.TrimSpace(runtime.GetCniVersion()),
	}
	if ifName := strings.TrimSpace(runtime.GetIfName()); ifName != "" || strings.TrimSpace(attachment.GetStaticMac()) != "" || strings.TrimSpace(runtime.GetNetnsPath()) != "" {
		result.Interfaces = []*nodeagentpb.CNIInterface{{
			Name:    ifName,
			Mac:     strings.TrimSpace(attachment.GetStaticMac()),
			Sandbox: strings.TrimSpace(runtime.GetNetnsPath()),
		}}
	}
	if ip := strings.TrimSpace(attachment.GetStaticIpCidr()); ip != "" {
		result.Ips = []*nodeagentpb.CNIIPConfig{{
			Address:   ip,
			Interface: 0,
		}}
	}
	return result
}

func buildResultFromNetworkState(runtime *nodeagentpb.CNIRuntimeContext, attachment *nodeagentpb.AttachmentRequest, state networkState) *nodeagentpb.CNIResult {
	result := &nodeagentpb.CNIResult{
		CniVersion: strings.TrimSpace(runtime.GetCniVersion()),
	}
	ifName := firstNonEmpty(strings.TrimSpace(state.ContainerIfName), strings.TrimSpace(runtime.GetIfName()))
	mac := firstNonEmpty(strings.TrimSpace(state.MACAddress), strings.TrimSpace(attachment.GetStaticMac()))
	sandbox := strings.TrimSpace(runtime.GetNetnsPath())
	if ifName != "" || mac != "" || sandbox != "" {
		result.Interfaces = []*nodeagentpb.CNIInterface{{
			Name:    ifName,
			Mac:     mac,
			Sandbox: sandbox,
		}}
	}
	ip := firstNonEmpty(strings.TrimSpace(state.StaticIPCIDR), strings.TrimSpace(attachment.GetStaticIpCidr()))
	if ip != "" {
		result.Ips = []*nodeagentpb.CNIIPConfig{{
			Address:   ip,
			Interface: 0,
		}}
	}
	return result
}

func equivalentRecord(existing, requested *endpointRecord) bool {
	if existing == nil || requested == nil {
		return false
	}
	return !isIdentityMismatch(existing.identity, requested.identity) &&
		normalizeRuntime(existing.runtime) == normalizeRuntime(requested.runtime) &&
		normalizeAttachment(existing.attachment) == normalizeAttachment(requested.attachment) &&
		normalizeResult(existing.result) == normalizeResult(requested.result) &&
		normalizeEndpointSpec(existing.endpointSpec) == normalizeEndpointSpec(requested.endpointSpec)
}

func isIdentityMismatch(existing, requested *nodeagentpb.EndpointIdentity) bool {
	return normalizeIdentity(existing) != normalizeIdentity(requested)
}

func isStaleDelete(existing, requested *nodeagentpb.EndpointIdentity) bool {
	if existing == nil || requested == nil {
		return true
	}
	if strings.TrimSpace(existing.GetEndpointId()) != strings.TrimSpace(requested.GetEndpointId()) {
		return true
	}
	if strings.TrimSpace(existing.GetNetworkId()) != strings.TrimSpace(requested.GetNetworkId()) {
		return true
	}
	if strings.TrimSpace(existing.GetAttachmentId()) != strings.TrimSpace(requested.GetAttachmentId()) {
		return true
	}
	existingWorkload := strings.TrimSpace(existing.GetWorkloadUid())
	requestWorkload := strings.TrimSpace(requested.GetWorkloadUid())
	if requestWorkload != "" && existingWorkload != "" && requestWorkload != existingWorkload {
		return true
	}
	existingInc := strings.TrimSpace(existing.GetIncarnation())
	requestInc := strings.TrimSpace(requested.GetIncarnation())
	return requestInc != "" && existingInc != "" && requestInc != existingInc
}

func cloneRecord(rec *endpointRecord) *endpointRecord {
	if rec == nil {
		return nil
	}
	return &endpointRecord{
		identity:     cloneIdentity(rec.identity),
		runtime:      cloneRuntime(rec.runtime),
		attachment:   cloneAttachment(rec.attachment),
		result:       cloneResult(rec.result),
		endpointSpec: cloneEndpointSpec(rec.endpointSpec),
	}
}

func cloneEndpointSpec(spec endpoint.EndpointSpec) endpoint.EndpointSpec {
	out := spec
	out.IPAddresses = append([]string(nil), spec.IPAddresses...)
	return out
}

func cloneIdentity(identity *nodeagentpb.EndpointIdentity) *nodeagentpb.EndpointIdentity {
	if identity == nil {
		return nil
	}
	return &nodeagentpb.EndpointIdentity{
		EndpointId:   identity.GetEndpointId(),
		NetworkId:    identity.GetNetworkId(),
		AttachmentId: identity.GetAttachmentId(),
		WorkloadUid:  identity.GetWorkloadUid(),
		Incarnation:  identity.GetIncarnation(),
	}
}

func cloneRuntime(runtime *nodeagentpb.CNIRuntimeContext) *nodeagentpb.CNIRuntimeContext {
	if runtime == nil {
		return nil
	}
	return &nodeagentpb.CNIRuntimeContext{
		CniVersion:  runtime.GetCniVersion(),
		ContainerId: runtime.GetContainerId(),
		NetnsPath:   runtime.GetNetnsPath(),
		IfName:      runtime.GetIfName(),
	}
}

func cloneAttachment(attachment *nodeagentpb.AttachmentRequest) *nodeagentpb.AttachmentRequest {
	if attachment == nil {
		return nil
	}
	return &nodeagentpb.AttachmentRequest{
		LogicalSwitch:    attachment.GetLogicalSwitch(),
		OvsInterfaceName: attachment.GetOvsInterfaceName(),
		StaticIpCidr:     attachment.GetStaticIpCidr(),
		StaticMac:        attachment.GetStaticMac(),
	}
}

func cloneResult(result *nodeagentpb.CNIResult) *nodeagentpb.CNIResult {
	if result == nil {
		return nil
	}
	out := &nodeagentpb.CNIResult{
		CniVersion: result.GetCniVersion(),
	}
	for _, iface := range result.GetInterfaces() {
		out.Interfaces = append(out.Interfaces, &nodeagentpb.CNIInterface{
			Name:    iface.GetName(),
			Mac:     iface.GetMac(),
			Sandbox: iface.GetSandbox(),
		})
	}
	for _, ip := range result.GetIps() {
		out.Ips = append(out.Ips, &nodeagentpb.CNIIPConfig{
			Address:   ip.GetAddress(),
			Interface: ip.GetInterface(),
			Gateway:   ip.GetGateway(),
		})
	}
	for _, route := range result.GetRoutes() {
		out.Routes = append(out.Routes, &nodeagentpb.CNIRoute{
			Dst: route.GetDst(),
			Gw:  route.GetGw(),
		})
	}
	if dns := result.GetDns(); dns != nil {
		out.Dns = &nodeagentpb.CNIDNS{
			Nameservers: append([]string(nil), dns.GetNameservers()...),
			Searches:    append([]string(nil), dns.GetSearches()...),
			Options:     append([]string(nil), dns.GetOptions()...),
			Domain:      dns.GetDomain(),
		}
	}
	return out
}

func normalizeIdentity(identity *nodeagentpb.EndpointIdentity) string {
	if identity == nil {
		return "<nil>"
	}
	return strings.Join([]string{
		strings.TrimSpace(identity.GetEndpointId()),
		strings.TrimSpace(identity.GetNetworkId()),
		strings.TrimSpace(identity.GetAttachmentId()),
		strings.TrimSpace(identity.GetWorkloadUid()),
		strings.TrimSpace(identity.GetIncarnation()),
	}, "|")
}

func normalizeRuntime(runtime *nodeagentpb.CNIRuntimeContext) string {
	if runtime == nil {
		return "<nil>"
	}
	return strings.Join([]string{
		strings.TrimSpace(runtime.GetCniVersion()),
		strings.TrimSpace(runtime.GetContainerId()),
		strings.TrimSpace(runtime.GetNetnsPath()),
		strings.TrimSpace(runtime.GetIfName()),
	}, "|")
}

func normalizeAttachment(attachment *nodeagentpb.AttachmentRequest) string {
	if attachment == nil {
		return "<nil>"
	}
	return strings.Join([]string{
		strings.TrimSpace(attachment.GetLogicalSwitch()),
		strings.TrimSpace(attachment.GetOvsInterfaceName()),
		strings.TrimSpace(attachment.GetStaticIpCidr()),
		strings.TrimSpace(attachment.GetStaticMac()),
	}, "|")
}

func normalizeEndpointSpec(spec endpoint.EndpointSpec) string {
	return strings.Join([]string{
		strings.TrimSpace(string(spec.ID)),
		strings.TrimSpace(spec.NetworkID),
		strings.TrimSpace(spec.AttachmentID),
		strings.TrimSpace(spec.Incarnation),
		strings.TrimSpace(spec.Workload.UID),
		strings.TrimSpace(spec.Workload.Name),
		strings.TrimSpace(spec.Workload.Namespace),
		strings.TrimSpace(spec.Workload.NodeName),
		strings.TrimSpace(spec.LogicalSwitch),
		strings.TrimSpace(spec.LogicalSwitchPort),
		strings.TrimSpace(spec.OVSInterfaceName),
		strings.TrimSpace(spec.MACAddress),
		strings.Join(spec.IPAddresses, ","),
	}, "|")
}

func normalizeResult(result *nodeagentpb.CNIResult) string {
	if result == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(result.GetCniVersion()))
	b.WriteString(";")
	for _, iface := range result.GetInterfaces() {
		b.WriteString(strings.Join([]string{
			strings.TrimSpace(iface.GetName()),
			strings.TrimSpace(iface.GetMac()),
			strings.TrimSpace(iface.GetSandbox()),
		}, ","))
		b.WriteString(";")
	}
	for _, ip := range result.GetIps() {
		b.WriteString(fmt.Sprintf("%s,%d,%s;", strings.TrimSpace(ip.GetAddress()), ip.GetInterface(), strings.TrimSpace(ip.GetGateway())))
	}
	return b.String()
}

func sanitizeName(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return "ep"
	}
	replacer := strings.NewReplacer("/", "-", ":", "-", ".", "-", "_", "-", " ", "-")
	v = replacer.Replace(v)
	var b strings.Builder
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "ep"
	}
	return trimName(out)
}

func trimName(v string) string {
	if len(v) <= maxNameLen {
		return v
	}
	return v[:maxNameLen]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

type inMemoryEndpointBackend struct {
	mu          sync.RWMutex
	ports       map[string]endpoint.OVNPortSpec
	attachments map[string]inMemoryAttachmentRecord
}

type inMemoryAttachmentRecord struct {
	spec     endpoint.AttachmentSpec
	identity endpoint.AttachmentIdentity
}

func newInMemoryEndpointManager() endpoint.EndpointManager {
	backend := &inMemoryEndpointBackend{
		ports:       make(map[string]endpoint.OVNPortSpec),
		attachments: make(map[string]inMemoryAttachmentRecord),
	}
	return endpoint.NewManager(backend, backend, true)
}

func (b *inMemoryEndpointBackend) EnsureLogicalSwitchPort(_ context.Context, spec endpoint.OVNPortSpec) (endpoint.OVNPortStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	existing, ok := b.ports[spec.LogicalSwitchPort]
	if ok && existing.EndpointID != spec.EndpointID {
		return endpoint.OVNPortStatus{}, fmt.Errorf("logical switch port %q is owned by another endpoint", spec.LogicalSwitchPort)
	}
	b.ports[spec.LogicalSwitchPort] = spec
	return endpoint.OVNPortStatus{
		LogicalSwitch:     spec.LogicalSwitch,
		LogicalSwitchPort: spec.LogicalSwitchPort,
	}, nil
}

func (b *inMemoryEndpointBackend) GetLogicalSwitchPort(_ context.Context, logicalSwitchPort string) (endpoint.OVNPortStatus, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	spec, ok := b.ports[logicalSwitchPort]
	if !ok {
		return endpoint.OVNPortStatus{}, fmt.Errorf("logical switch port %q not found", logicalSwitchPort)
	}
	return endpoint.OVNPortStatus{
		LogicalSwitch:     spec.LogicalSwitch,
		LogicalSwitchPort: spec.LogicalSwitchPort,
	}, nil
}

func (b *inMemoryEndpointBackend) DeleteLogicalSwitchPort(_ context.Context, spec endpoint.OVNPortSpec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.ports, spec.LogicalSwitchPort)
	return nil
}

func (b *inMemoryEndpointBackend) EnsureAttachment(_ context.Context, spec endpoint.AttachmentSpec) (endpoint.AttachmentStatus, error) {
	legacyIdentity := endpoint.AttachmentIdentity{
		EndpointID:   string(spec.EndpointID),
		NetworkID:    "legacy",
		AttachmentID: spec.LogicalSwitchPort,
		WorkloadUID:  "legacy",
		Incarnation:  "legacy",
	}
	return b.EnsureAttachmentWithIdentity(context.Background(), legacyIdentity, spec)
}

func (b *inMemoryEndpointBackend) EnsureAttachmentWithIdentity(_ context.Context, identity endpoint.AttachmentIdentity, spec endpoint.AttachmentSpec) (endpoint.AttachmentStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	existing, ok := b.attachments[spec.LogicalSwitchPort]
	if ok {
		existingIdentity := existing.identity
		if existingIdentity.EndpointID != identity.EndpointID ||
			existingIdentity.NetworkID != identity.NetworkID ||
			existingIdentity.AttachmentID != identity.AttachmentID ||
			existingIdentity.WorkloadUID != identity.WorkloadUID {
			return endpoint.AttachmentStatus{}, fmt.Errorf("attachment %q is owned by another endpoint identity", spec.LogicalSwitchPort)
		}
		if existingIdentity.Incarnation != "" && identity.Incarnation != "" && existingIdentity.Incarnation != identity.Incarnation {
			// Newer incarnation replaces stale identity metadata.
		}
	}
	b.attachments[spec.LogicalSwitchPort] = inMemoryAttachmentRecord{spec: spec, identity: identity}
	return endpoint.AttachmentStatus{
		OVSInterfaceName: spec.OVSInterfaceName,
	}, nil
}

func (b *inMemoryEndpointBackend) DeleteAttachment(_ context.Context, endpointID endpoint.EndpointID, logicalSwitchPort string) error {
	legacyIdentity := endpoint.AttachmentIdentity{
		EndpointID:   string(endpointID),
		NetworkID:    "legacy",
		AttachmentID: logicalSwitchPort,
		WorkloadUID:  "legacy",
		Incarnation:  "legacy",
	}
	_, err := b.DeleteAttachmentWithIdentity(context.Background(), legacyIdentity, logicalSwitchPort)
	return err
}

func (b *inMemoryEndpointBackend) DeleteAttachmentWithIdentity(_ context.Context, identity endpoint.AttachmentIdentity, logicalSwitchPort string) (endpoint.AttachmentDeleteOutcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	spec, ok := b.attachments[logicalSwitchPort]
	if !ok {
		return endpoint.AttachmentDeleteOutcomeMissing, nil
	}
	existingIdentity := spec.identity
	if existingIdentity.EndpointID != identity.EndpointID ||
		existingIdentity.NetworkID != identity.NetworkID ||
		existingIdentity.AttachmentID != identity.AttachmentID ||
		existingIdentity.WorkloadUID != identity.WorkloadUID {
		return endpoint.AttachmentDeleteOutcomeMissing, fmt.Errorf("attachment %q belongs to another endpoint identity", logicalSwitchPort)
	}
	if existingIdentity.Incarnation != "" && identity.Incarnation != "" && existingIdentity.Incarnation != identity.Incarnation {
		return endpoint.AttachmentDeleteOutcomeStale, fmt.Errorf("attachment %q stale delete for incarnation %q", logicalSwitchPort, identity.Incarnation)
	}
	delete(b.attachments, logicalSwitchPort)
	return endpoint.AttachmentDeleteOutcomeDeleted, nil
}
